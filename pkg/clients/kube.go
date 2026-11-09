package clients

import (
	"fmt"
	"net/http"

	istio "istio.io/client-go/pkg/clientset/versioned"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/kubeedge/edgemesh/pkg/apis/config/v1alpha1"
)

type Clients struct {
	Config      *v1alpha1.KubeAPIConfig
	restConfig  *rest.Config
	kubeClient  kubernetes.Interface
	istioClient istio.Interface
}

func NewClients(config *v1alpha1.KubeAPIConfig) (*Clients, error) {
	kubeConfig, err := buildConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to build config, %v", err)
	}
	kubeConfig.QPS = float32(config.QPS)
	kubeConfig.Burst = int(config.Burst)
	kubeConfig.ContentType = config.ContentType
	kubeClient := kubernetes.NewForConfigOrDie(kubeConfig)

	istioKubeConfig := rest.CopyConfig(kubeConfig)
	istioKubeConfig.ContentType = runtime.ContentTypeJSON
	istioClient := istio.NewForConfigOrDie(istioKubeConfig)

	return &Clients{
		Config:      config,
		restConfig:  kubeConfig,
		kubeClient:  kubeClient,
		istioClient: istioClient,
	}, nil
}

func (cli *Clients) GetKubeClient() kubernetes.Interface {
	return cli.kubeClient
}

func (cli *Clients) GetIstioClient() istio.Interface {
	return cli.istioClient
}

// Resolve the API once for every consumer. An explicit master can use the Pod's
// rotating service-account credentials without a separately mounted kubeconfig.
func buildConfig(config *v1alpha1.KubeAPIConfig) (*rest.Config, error) {
	if config.KubeConfig == "" {
		cfg, err := rest.InClusterConfig()
		if err == nil {
			if config.Master != "" {
				cfg.Host = config.Master
			}
			return cfg, nil
		}
	}
	return clientcmd.BuildConfigFromFlags(config.Master, config.KubeConfig)
}

// WriteKubeConfig gives CoreDNS the same resolved endpoint and credentials as
// the proxy client. CoreDNS otherwise gives kubeconfig precedence over master.
func (cli *Clients) WriteKubeConfig(path string) error {
	cfg := cli.restConfig
	cluster := &clientcmdapi.Cluster{
		Server: cfg.Host, TLSServerName: cfg.ServerName,
		InsecureSkipTLSVerify: cfg.Insecure,
		CertificateAuthority:  cfg.CAFile, CertificateAuthorityData: cfg.CAData,
	}
	if cfg.Proxy != nil {
		request, err := http.NewRequest(http.MethodGet, cfg.Host, nil)
		if err != nil {
			return err
		}
		proxy, err := cfg.Proxy(request)
		if err != nil {
			return err
		}
		if proxy != nil {
			cluster.ProxyURL = proxy.String()
		}
	}
	auth := &clientcmdapi.AuthInfo{
		Token: cfg.BearerToken, TokenFile: cfg.BearerTokenFile,
		ClientCertificate: cfg.CertFile, ClientCertificateData: cfg.CertData,
		ClientKey: cfg.KeyFile, ClientKeyData: cfg.KeyData,
		Username: cfg.Username, Password: cfg.Password,
		AuthProvider: cfg.AuthProvider, Exec: cfg.ExecProvider,
		Impersonate: cfg.Impersonate.UserName, ImpersonateGroups: cfg.Impersonate.Groups,
		ImpersonateUserExtra: cfg.Impersonate.Extra,
	}
	return clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"api": cluster},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"agent": auth},
		Contexts:       map[string]*clientcmdapi.Context{"agent": {Cluster: "api", AuthInfo: "agent"}},
		CurrentContext: "agent",
	}, path)
}
