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
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
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
	"github.com/kubeedge/edgemesh/pkg/meshstate"
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
	managedRuntime    *meshstate.Runtime
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
	serviceFilterMode defaults.ServiceFilterMode,
	managedRuntime *meshstate.Runtime) (*Server, error) {
	klog.V(0).Info("Using userspace Proxier.")

	// Create a iptables utils.
	execer := exec.New()
	iptInterface := utiliptables.New(execer, utiliptables.ProtocolIPv4)

	// Initialize a loadBalancer
	loadBalancer := loadbalancer.New(lbConfig, client, istioClient, config.ConfigSyncPeriod.Duration)
	if managedRuntime != nil {
		loadBalancer.SetManagedRuntimeStore(managedRuntime.Store)
	}
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
		managedRuntime:    managedRuntime,
	}, nil
}

func (s *Server) Run() error {
	operation := selection.DoesNotExist
	if s.serviceFilterMode != defaults.FilterIfLabelExistsMode {
		operation = selection.Exists
	}
	proxyLabel, err := labels.NewRequirement(defaults.LabelEdgeMeshServiceProxyName, operation, nil)
	if err != nil {
		return err
	}
	noHeadless, err := labels.NewRequirement(v1.IsHeadlessService, selection.DoesNotExist, nil)
	if err != nil {
		return err
	}
	selector := labels.NewSelector().Add(*proxyLabel, *noHeadless)
	factory := informers.NewSharedInformerFactoryWithOptions(s.kubeClient, s.ConfigSyncPeriod,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = selector.String()
		}))
	services := factory.Core().V1().Services()
	endpoints := factory.Core().V1().Endpoints()

	if proxier, ok := s.Proxier.(*userspace.Proxier); ok {
		// Service cache updates precede their callbacks. A separate Namespace
		// watch can lag creation or replacement and must not veto this source.
		proxier.SetServiceUIDExistsHandler(func(port proxy.ServicePortName, uid types.UID) bool {
			if !services.Informer().HasSynced() {
				return true
			}
			service, err := services.Lister().Services(port.Namespace).Get(port.Name)
			if err != nil || service.UID != uid {
				return false
			}
			for _, candidate := range service.Spec.Ports {
				if candidate.Name == port.Port {
					return true
				}
			}
			return false
		})
		if s.managedRuntime != nil {
			proxier.SetManagedServiceHandler(func(service *v1.Service) bool {
				return service != nil && service.Labels[meshstate.LabelMeshManaged] == "true"
			})
			proxier.SetServicePendingHandler(func(port proxy.ServicePortName, uid types.UID) {
				s.managedRuntime.Store.MarkPortalPending(uid, port)
			})
			proxier.SetServiceAppliedHandler(func(port proxy.ServicePortName, uid types.UID, applied bool) {
				s.managedRuntime.Store.MarkPortalApplied(uid, port, applied)
			})
			proxier.SetServiceRemovedHandler(func(port proxy.ServicePortName, uid types.UID) {
				s.managedRuntime.Store.MarkPortalRemoved(uid, port)
			})
		}
	}
	// Both consumers observe the same ordered informer stream. Relists supply
	// delete tombstones for objects missed during a disconnected watch.
	if s.managedRuntime != nil {
		s.managedRuntime.RegisterInformers(services.Informer(), endpoints.Informer(), wait.NeverStop)
		if err := s.managedRuntime.StartStatusServer(); err != nil {
			return err
		}
	}
	serviceConfig := config.NewServiceConfig(services, s.ConfigSyncPeriod)
	serviceConfig.RegisterEventHandler(s.Proxier)
	go serviceConfig.Run(wait.NeverStop)
	if handler, ok := s.Proxier.(config.EndpointsHandler); ok {
		endpointConfig := config.NewEndpointsConfig(endpoints, s.ConfigSyncPeriod)
		endpointConfig.RegisterEventHandler(handler)
		go endpointConfig.Run(wait.NeverStop)
	}
	factory.Start(wait.NeverStop)
	s.loadBalancer.Config.Caller = defaults.ProxyCaller
	if err := s.loadBalancer.Run(); err != nil {
		if s.managedRuntime != nil {
			_ = s.managedRuntime.Close()
		}
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
