package dns

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coredns/caddy"
	dnskubernetes "github.com/coredns/coredns/plugin/kubernetes"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/kubeedge/edgemesh/pkg/apis/config/defaults"
	"github.com/kubeedge/edgemesh/pkg/apis/config/v1alpha1"
	"github.com/kubeedge/edgemesh/pkg/clients"
)

// Exercise the real CoreDNS plugin and client-go cache against one TLS API,
// without starting DNS/agent listeners or touching host networking.
func TestDNSAndProxyCachesRecoverFromTheSameAPI(t *testing.T) {
	var mu sync.Mutex
	available, failures := true, 0
	revision := "10"
	relist := make(chan struct{})
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "redis", Namespace: "sample", UID: "service-1", ResourceVersion: "10"},
		Spec:       v1.ServiceSpec{ClusterIP: "10.96.0.10", Ports: []v1.ServicePort{{Name: "tcp", Port: 6379}}},
	}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("API consumer lost the configured credentials")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		if !available {
			failures++
			mu.Unlock()
			http.Error(w, "API unavailable", http.StatusServiceUnavailable)
			return
		}
		changed := relist
		version := revision
		var items []v1.Service
		if service != nil {
			items = []v1.Service{*service.DeepCopy()}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-changed:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"type": "ERROR", "object": &metav1.Status{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Code: 410, Reason: metav1.StatusReasonExpired,
				}})
			}
			return
		}
		var response interface{}
		switch r.URL.Path {
		case "/api/v1/services":
			response = &v1.ServiceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceList"},
				ListMeta: metav1.ListMeta{ResourceVersion: version}, Items: items}
		case "/api/v1/endpoints":
			response = &v1.EndpointsList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EndpointsList"},
				ListMeta: metav1.ListMeta{ResourceVersion: version}}
		case "/api/v1/namespaces":
			response = &v1.NamespaceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NamespaceList"},
				ListMeta: metav1.ListMeta{ResourceVersion: version}, Items: []v1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "sample"}}}}
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer api.Close()

	dir := t.TempDir()
	original, resolved := filepath.Join(dir, "input"), filepath.Join(dir, "dns")
	cfg := clientcmdapi.Config{
		Clusters:  map[string]*clientcmdapi.Cluster{"api": {Server: "https://unused.invalid", CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"agent": {Token: "fixture-token"}},
		Contexts:  map[string]*clientcmdapi.Context{"agent": {Cluster: "api", AuthInfo: "agent"}}, CurrentContext: "agent",
	}
	if err := clientcmd.WriteToFile(cfg, original); err != nil {
		t.Fatal(err)
	}
	cli, err := clients.NewClients(&v1alpha1.KubeAPIConfig{Master: api.URL, KubeConfig: original})
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.WriteKubeConfig(resolved); err != nil {
		t.Fatal(err)
	}
	stanza, err := getKubernetesPluginStr()
	if err != nil {
		t.Fatal(err)
	}
	parser := caddy.NewTestController("dns", strings.Replace(stanza, defaults.TempDNSKubeConfigPath, strconv.Quote(resolved), 1))
	parser.Next()
	plugin, err := dnskubernetes.ParseStanza(parser)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.InitKubeCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	go plugin.APIConn.Run()
	defer plugin.APIConn.Stop()
	stop := make(chan struct{})
	defer close(stop)
	factory := informers.NewSharedInformerFactory(cli.GetKubeClient(), 0)
	services := factory.Core().V1().Services()
	services.Informer()
	factory.Start(stop)
	await := func(condition func() bool) {
		t.Helper()
		if err := wait.PollImmediate(10*time.Millisecond, 10*time.Second, func() (bool, error) { return condition(), nil }); err != nil {
			t.Fatal(err)
		}
	}
	message := new(dns.Msg)
	message.SetQuestion("redis.sample.svc.cluster.local.", dns.TypeA)
	query := request.Request{Req: message, Zone: "cluster.local."}
	agree := func(ip string) bool {
		current, proxyErr := services.Lister().Services("sample").Get("redis")
		records, dnsErr := plugin.Records(context.Background(), query, false)
		if ip == "" {
			return apierrors.IsNotFound(proxyErr) && plugin.IsNameError(dnsErr) && len(records) == 0
		}
		return proxyErr == nil && current.Spec.ClusterIP == ip && dnsErr == nil && len(records) == 1 && records[0].Host == ip
	}
	await(func() bool {
		return services.Informer().HasSynced() && plugin.APIConn.HasSynced() && agree("10.96.0.10")
	})

	mu.Lock()
	available = false
	service = service.DeepCopy()
	service.UID, service.ResourceVersion, service.Spec.ClusterIP = "service-2", "21", "10.96.0.11"
	revision = "21"
	close(relist)
	relist = make(chan struct{})
	mu.Unlock()
	await(func() bool { mu.Lock(); defer mu.Unlock(); return failures >= 2 })
	if !agree("10.96.0.10") {
		t.Fatal("API error erased a synchronized DNS or proxy cache")
	}
	mu.Lock()
	available = true
	mu.Unlock()
	await(func() bool { return agree("10.96.0.11") })
	current, _ := services.Lister().Services("sample").Get("redis")
	if current.UID != "service-2" {
		t.Fatal("proxy retained the replaced Service UID")
	}
	mu.Lock()
	service = nil
	revision = "22"
	close(relist)
	relist = make(chan struct{})
	mu.Unlock()
	await(func() bool { return agree("") })
}
