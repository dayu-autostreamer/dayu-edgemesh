package proxy

import (
	"errors"
	"fmt"
	"time"

	istioclientset "istio.io/client-go/pkg/clientset/versioned"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/proxy"
	proxyconfigapi "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/kubernetes/pkg/proxy/config"
	"k8s.io/kubernetes/pkg/proxy/userspace"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	"k8s.io/utils/exec"
	netutils "k8s.io/utils/net"

	"github.com/kubeedge/edgemesh/pkg/apis/config/defaults"
	"github.com/kubeedge/edgemesh/pkg/apis/config/v1alpha1"
	"github.com/kubeedge/edgemesh/pkg/loadbalancer"
)

// Copy and update from https://github.com/kubernetes/kubernetes/blob/v1.23.0/cmd/kube-proxy/app/server.go and
// https://github.com/kubernetes/kubernetes/blob/v1.23.0/cmd/kube-proxy/app/server_others.go.

// Server represents all the parameters required to start the Kubernetes proxy server.
type Server struct {
	kubeClient        clientset.Interface
	istioClient       istioclientset.Interface
	IptInterface      utiliptables.Interface
	execer            exec.Interface
	Proxier           proxy.Provider
	ConfigSyncPeriod  time.Duration
	loadBalancer      *loadbalancer.LoadBalancer
	serviceFilterMode defaults.ServiceFilterMode
}

// NewDefaultKubeProxyConfiguration new default kube-proxy config for edgemesh-agent runtime.
// TODO(Poorunga) Use container config for this.
func NewDefaultKubeProxyConfiguration(bindAddress string) *proxyconfigapi.KubeProxyConfiguration {
	return &proxyconfigapi.KubeProxyConfiguration{
		BindAddress: bindAddress,
		PortRange:   "",
		IPTables: proxyconfigapi.KubeProxyIPTablesConfiguration{
			SyncPeriod:    metav1.Duration{Duration: 30 * time.Second},
			MinSyncPeriod: metav1.Duration{Duration: time.Second},
		},
		UDPIdleTimeout:    metav1.Duration{Duration: 250 * time.Millisecond},
		NodePortAddresses: nil,
		ConfigSyncPeriod:  metav1.Duration{Duration: 15 * time.Minute},
	}
}

func newProxyServer(
	config *proxyconfigapi.KubeProxyConfiguration,
	lbConfig *v1alpha1.LoadBalancer,
	client clientset.Interface,
	istioClient istioclientset.Interface,
	serviceFilterMode defaults.ServiceFilterMode) (*Server, error) {
	klog.V(0).Info("Using userspace Proxier.")

	// Create a iptables utils.
	execer := exec.New()
	iptInterface := utiliptables.New(execer, utiliptables.ProtocolIPv4)

	// Initialize a loadBalancer
	loadBalancer := loadbalancer.New(lbConfig, client, istioClient, config.ConfigSyncPeriod.Duration)
	initLoadBalancer(loadBalancer)

	proxier, err := userspace.NewCustomProxier(
		loadBalancer,
		netutils.ParseIPSloppy(config.BindAddress),
		iptInterface,
		execer,
		*utilnet.ParsePortRangeOrDie(config.PortRange),
		config.IPTables.SyncPeriod.Duration,
		config.IPTables.MinSyncPeriod.Duration,
		config.UDPIdleTimeout.Duration,
		config.NodePortAddresses,
		newProxySocket,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create proxier: %v", err)
	}

	return &Server{
		kubeClient:        client,
		istioClient:       istioClient,
		IptInterface:      iptInterface,
		execer:            execer,
		Proxier:           proxier,
		ConfigSyncPeriod:  config.ConfigSyncPeriod.Duration,
		loadBalancer:      loadBalancer,
		serviceFilterMode: serviceFilterMode,
	}, nil
}

func (s *Server) Run() error {
	// Determine the service filter mode.
	// By default, we will proxy all services that are not labeled with the LabelEdgeMeshServiceProxyName label.
	operation := selection.DoesNotExist
	if s.serviceFilterMode != defaults.FilterIfLabelExistsMode {
		operation = selection.Exists
	}
	noEdgeMeshProxyName, err := labels.NewRequirement(defaults.LabelEdgeMeshServiceProxyName, operation, nil)
	if err != nil {
		return err
	}

	noHeadlessEndpoints, err := labels.NewRequirement(v1.IsHeadlessService, selection.DoesNotExist, nil)
	if err != nil {
		return err
	}

	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*noEdgeMeshProxyName, *noHeadlessEndpoints)

	// Make informers that filter out objects that want a non-default service proxy.
	informerFactory := informers.NewSharedInformerFactoryWithOptions(s.kubeClient, s.ConfigSyncPeriod,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}))
	namespaceInformerFactory := informers.NewSharedInformerFactory(s.kubeClient, s.ConfigSyncPeriod)
	validationServiceInformerFactory := informerFactory
	validationNamespaceInformerFactory := namespaceInformerFactory
	validationClient := s.kubeClient
	if inClusterConfig, err := rest.InClusterConfig(); err == nil {
		if directClient, directErr := clientset.NewForConfig(inClusterConfig); directErr == nil {
			validationClient = directClient
			validationServiceInformerFactory = informers.NewSharedInformerFactoryWithOptions(validationClient, s.ConfigSyncPeriod,
				informers.WithTweakListOptions(func(options *metav1.ListOptions) {
					options.LabelSelector = labelSelector.String()
				}))
			validationNamespaceInformerFactory = informers.NewSharedInformerFactory(validationClient, s.ConfigSyncPeriod)
			klog.InfoS("Using direct Kubernetes API validation source for EdgeMesh proxy state")
		} else {
			klog.InfoS("Falling back to primary Kubernetes client for EdgeMesh proxy validation", "err", directErr)
		}
	} else {
		klog.V(2).InfoS("In-cluster Kubernetes API validation source unavailable; falling back to primary Kubernetes client", "err", err)
	}
	// Create configs (i.e. Watches for Services and Endpoints or EndpointSlices)
	// Note: RegisterHandler() calls need to happen before creation of Sources because sources
	// only notify on changes, and the initial update (on process start) may be lost if no handlers
	// are registered yet.
	serviceInformer := informerFactory.Core().V1().Services()
	validationServiceInformer := validationServiceInformerFactory.Core().V1().Services()
	validationNamespaceInformer := validationNamespaceInformerFactory.Core().V1().Namespaces()
	if userspaceProxier, ok := s.Proxier.(*userspace.Proxier); ok {
		userspaceProxier.SetValidationReadyHandler(func() bool {
			return validationServiceInformer.Informer().HasSynced() && validationNamespaceInformer.Informer().HasSynced()
		})
		userspaceProxier.SetNamespaceExistsHandler(func(namespace string) bool {
			if !validationNamespaceInformer.Informer().HasSynced() {
				return true
			}
			_, err := validationNamespaceInformer.Lister().Get(namespace)
			return err == nil
		})
		userspaceProxier.SetServiceExistsHandler(func(servicePort proxy.ServicePortName) bool {
			if !validationServiceInformer.Informer().HasSynced() {
				return true
			}
			service, err := validationServiceInformer.Lister().Services(servicePort.NamespacedName.Namespace).Get(servicePort.NamespacedName.Name)
			if err != nil {
				return false
			}
			for i := range service.Spec.Ports {
				if service.Spec.Ports[i].Name == servicePort.Port {
					return true
				}
			}
			return false
		})
	}
	serviceConfig := config.NewServiceConfig(serviceInformer, s.ConfigSyncPeriod)
	serviceConfig.RegisterEventHandler(s.Proxier)
	go serviceConfig.Run(wait.NeverStop)

	if endpointsHandler, ok := s.Proxier.(config.EndpointsHandler); ok {
		endpointsConfig := config.NewEndpointsConfig(informerFactory.Core().V1().Endpoints(), s.ConfigSyncPeriod)
		endpointsConfig.RegisterEventHandler(endpointsHandler)
		go endpointsConfig.Run(wait.NeverStop)
	}

	// This has to start after the calls to NewServiceConfig and NewEndpointsConfig because those
	// functions must configure their shared informer event handlers first.
	informerFactory.Start(wait.NeverStop)
	namespaceInformerFactory.Start(wait.NeverStop)
	if validationClient != s.kubeClient {
		validationServiceInformerFactory.Start(wait.NeverStop)
		validationNamespaceInformerFactory.Start(wait.NeverStop)
	}
	if userspaceProxier, ok := s.Proxier.(*userspace.Proxier); ok {
		go func() {
			if cache.WaitForNamedCacheSync("userspace proxy validation", wait.NeverStop,
				validationServiceInformer.Informer().HasSynced,
				validationNamespaceInformer.Informer().HasSynced,
			) {
				userspaceProxier.Sync()
			}
		}()
	}

	// Run loadBalancer
	s.loadBalancer.Config.Caller = defaults.ProxyCaller
	err = s.loadBalancer.Run()
	if err != nil {
		return fmt.Errorf("failed to run loadBalancer: %w", err)
	}

	go s.Proxier.SyncLoop()

	return nil
}

// CleanupAndExit remove iptables rules
func (s *Server) CleanupAndExit() error {
	ipts := []utiliptables.Interface{
		utiliptables.New(s.execer, utiliptables.ProtocolIPv4),
	}
	var encounteredError bool
	for _, ipt := range ipts {
		encounteredError = userspace.CleanupLeftovers(ipt) || encounteredError
	}
	if encounteredError {
		return errors.New("encountered an error while tearing down rules")
	}
	return nil
}
