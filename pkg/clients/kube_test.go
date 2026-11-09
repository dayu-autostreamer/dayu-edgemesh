package clients

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kubeedge/edgemesh/pkg/apis/config/v1alpha1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestDNSUsesResolvedMasterAndRotatingCredentials(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("token-one"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "original")
	original := clientcmdapi.Config{
		Clusters:  map[string]*clientcmdapi.Cluster{"api": {Server: "https://old-api:6443", InsecureSkipTLSVerify: true}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"agent": {TokenFile: token}},
		Contexts:  map[string]*clientcmdapi.Context{"agent": {Cluster: "api", AuthInfo: "agent"}}, CurrentContext: "agent",
	}
	if err := clientcmd.WriteToFile(original, path); err != nil {
		t.Fatal(err)
	}
	cli, err := NewClients(&v1alpha1.KubeAPIConfig{Master: "https://current-api:6443", KubeConfig: path})
	if err != nil {
		t.Fatal(err)
	}
	dnsPath := filepath.Join(dir, "dns")
	if err := cli.WriteKubeConfig(dnsPath); err != nil {
		t.Fatal(err)
	}
	dns, err := clientcmd.BuildConfigFromFlags("", dnsPath)
	if err != nil {
		t.Fatal(err)
	}
	if dns.Host != cli.restConfig.Host || dns.Host != "https://current-api:6443" {
		t.Fatalf("different authorities: %s / %s", dns.Host, cli.restConfig.Host)
	}
	if dns.BearerTokenFile != token || !dns.Insecure || dns.Timeout != 0 {
		t.Fatalf("credentials or watch timeout changed: tokenFile=%s timeout=%s", dns.BearerTokenFile, dns.Timeout)
	}
	info, err := os.Stat(dnsPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("DNS credentials must be private: %v", err)
	}
	saved, err := clientcmd.LoadFromFile(path)
	if err != nil || saved.Clusters["api"].Server != "https://old-api:6443" {
		t.Fatal("operator config was modified")
	}
}
