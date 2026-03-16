package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	istioclientset "istio.io/client-go/pkg/clientset/versioned"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
	if directValidationClient, source, err := buildValidationClient(s.kubeClient); err == nil {
		validationClient = directValidationClient
		validationServiceInformerFactory = informers.NewSharedInformerFactoryWithOptions(validationClient, s.ConfigSyncPeriod,
			informers.WithTweakListOptions(func(options *metav1.ListOptions) {
				options.LabelSelector = labelSelector.String()
			}))
		validationNamespaceInformerFactory = informers.NewSharedInformerFactory(validationClient, s.ConfigSyncPeriod)
		klog.InfoS("Using independent Kubernetes API validation source for EdgeMesh proxy state", "source", source)
	} else {
		klog.InfoS("Falling back to primary Kubernetes client for EdgeMesh proxy validation", "err", err)
	}
	// Create configs (i.e. Watches for Services and Endpoints or EndpointSlices)
	// Note: RegisterHandler() calls need to happen before creation of Sources because sources
	// only notify on changes, and the initial update (on process start) may be lost if no handlers
	// are registered yet.
	serviceInformer := informerFactory.Core().V1().Services()
	validationServiceInformer := validationServiceInformerFactory.Core().V1().Services()
	validationNamespaceInformer := validationNamespaceInformerFactory.Core().V1().Namespaces()
	hasIndependentValidation := validationClient != s.kubeClient
	if userspaceProxier, ok := s.Proxier.(*userspace.Proxier); ok {
		userspaceProxier.SetNamespaceExistsHandler(func(namespace string) bool {
			if validationNamespaceInformer.Informer().HasSynced() {
				_, err := validationNamespaceInformer.Lister().Get(namespace)
				return err == nil
			}
			if !hasIndependentValidation {
				return true
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := validationClient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
			switch {
			case err == nil:
				return true
			case apierrors.IsNotFound(err):
				return false
			default:
				klog.V(2).InfoS("Namespace validation lookup failed, falling back to trusting primary source", "namespace", namespace, "err", err)
				return true
			}
		})
		userspaceProxier.SetServiceExistsHandler(func(servicePort proxy.ServicePortName) bool {
			if validationServiceInformer.Informer().HasSynced() {
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
			}
			if !hasIndependentValidation {
				return true
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			service, err := validationClient.CoreV1().Services(servicePort.NamespacedName.Namespace).Get(ctx, servicePort.NamespacedName.Name, metav1.GetOptions{})
			switch {
			case err == nil:
			case apierrors.IsNotFound(err):
				return false
			default:
				klog.V(2).InfoS("Service validation lookup failed, falling back to trusting primary source", "servicePortName", servicePort, "err", err)
				return true
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

	// Run loadBalancer
	s.loadBalancer.Config.Caller = defaults.ProxyCaller
	err = s.loadBalancer.Run()
	if err != nil {
		return fmt.Errorf("failed to run loadBalancer: %w", err)
	}

	go s.Proxier.SyncLoop()

	return nil
}

func buildValidationClient(primaryClient clientset.Interface) (clientset.Interface, string, error) {
	var errs []error
	if directClient, source, err := buildInClusterValidationClient(primaryClient); err == nil {
		return directClient, source, nil
	} else {
		errs = append(errs, err)
	}

	if validationClient, source, err := buildKubeconfigValidationClient(); err == nil {
		return validationClient, source, nil
	} else {
		errs = append(errs, err)
	}

	return nil, "", joinValidationErrors(errs)
}

func buildKubeconfigValidationClient() (clientset.Interface, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig validation unavailable: load config failed: %w", err)
	}
	if isMetaServerHost(kubeConfig.Host) {
		return nil, "", fmt.Errorf("kubeconfig validation unavailable: discovered kubeconfig still points to MetaServer host %q", kubeConfig.Host)
	}

	kubeConfig = rest.CopyConfig(kubeConfig)
	kubeConfig.Timeout = 3 * time.Second
	validationClient, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig validation unavailable: build client failed: %w", err)
	}
	if err := validateClientReachability(validationClient); err != nil {
		return nil, "", fmt.Errorf("kubeconfig validation unavailable: client is unreachable: %w", err)
	}

	return validationClient, fmt.Sprintf("kubeconfig:%s", kubeConfig.Host), nil
}

func buildInClusterValidationClient(primaryClient clientset.Interface) (clientset.Interface, string, error) {
	inClusterConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("in-cluster config unavailable: %w", err)
	}

	directConfig := rest.CopyConfig(inClusterConfig)
	directConfig.Timeout = 3 * time.Second
	directClient, err := clientset.NewForConfig(directConfig)
	if err != nil {
		return nil, "", fmt.Errorf("build in-cluster client failed: %w", err)
	}
	directReachabilityErr := validateClientReachability(directClient)
	if directReachabilityErr == nil {
		return directClient, fmt.Sprintf("in-cluster:%s", directConfig.Host), nil
	}

	endpointHost, err := discoverKubernetesEndpointHost(primaryClient)
	if err != nil {
		return nil, "", fmt.Errorf("in-cluster client %q is unreachable: %v; kubernetes endpoints discovery failed: %w", directConfig.Host, directReachabilityErr, err)
	}

	endpointConfig := rest.CopyConfig(inClusterConfig)
	endpointConfig.Host = endpointHost
	endpointConfig.Timeout = 3 * time.Second
	endpointConfig.TLSClientConfig.ServerName = "kubernetes.default.svc"

	endpointClient, err := clientset.NewForConfig(endpointConfig)
	if err != nil {
		return nil, "", fmt.Errorf("build endpoint validation client failed: %w", err)
	}
	if err := validateClientReachability(endpointClient); err != nil {
		return nil, "", fmt.Errorf("in-cluster client %q is unreachable: %v; endpoint validation client %q is unreachable: %w", directConfig.Host, directReachabilityErr, endpointHost, err)
	}

	return endpointClient, fmt.Sprintf("kubernetes-endpoint:%s", endpointHost), nil
}

func validateClientReachability(client clientset.Interface) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := client.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{})
	if err != nil {
		return err
	}
	return nil
}

func discoverKubernetesEndpointHost(primaryClient clientset.Interface) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	endpoints, err := primaryClient.CoreV1().Endpoints("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get default/kubernetes endpoints failed: %w", err)
	}

	return kubernetesEndpointHost(endpoints)
}

func kubernetesEndpointHost(endpoints *v1.Endpoints) (string, error) {
	if endpoints == nil {
		return "", errors.New("default/kubernetes endpoints are nil")
	}

	for _, subset := range endpoints.Subsets {
		port, found := preferredEndpointPort(subset.Ports)
		if !found {
			continue
		}
		for _, address := range subset.Addresses {
			if address.IP == "" {
				continue
			}
			return fmt.Sprintf("https://%s", net.JoinHostPort(address.IP, strconv.Itoa(int(port)))), nil
		}
	}

	return "", errors.New("default/kubernetes endpoints do not contain a ready address with a TCP port")
}

func preferredEndpointPort(ports []v1.EndpointPort) (int32, bool) {
	var firstTCPPort int32
	for _, port := range ports {
		if port.Protocol != "" && port.Protocol != v1.ProtocolTCP {
			continue
		}
		if firstTCPPort == 0 {
			firstTCPPort = port.Port
		}
		if port.Name == "https" || port.Port == 443 {
			return port.Port, true
		}
	}
	if firstTCPPort == 0 {
		return 0, false
	}
	return firstTCPPort, true
}

func isMetaServerHost(host string) bool {
	if host == "" {
		return false
	}

	rawHost := host
	if strings.Contains(rawHost, "://") {
		parsedURL, err := url.Parse(rawHost)
		if err == nil && parsedURL.Host != "" {
			rawHost = parsedURL.Host
		}
	}

	hostname := rawHost
	port := ""
	if parsedHost, parsedPort, err := net.SplitHostPort(rawHost); err == nil {
		hostname = parsedHost
		port = parsedPort
	}

	hostname = strings.Trim(hostname, "[]")
	if port != "10550" {
		return false
	}

	switch strings.ToLower(hostname) {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func joinValidationErrors(errs []error) error {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		if err == nil {
			continue
		}
		messages = append(messages, err.Error())
	}
	if len(messages) == 0 {
		return nil
	}
	return errors.New(strings.Join(messages, "; "))
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
