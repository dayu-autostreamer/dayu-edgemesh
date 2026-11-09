package dns

import (
	"strings"
	"testing"
)

func TestStubDomainUsesKubernetesAndShortNegativeCache(t *testing.T) {
	corefile, err := getStubDomainStr(map[string][]string{
		".": {"10.96.0.10"},
	}, &stubDomainInfo{
		LocalIP:          "169.254.96.16",
		Port:             "53",
		CacheTTL:         30,
		UpstreamServers:  "10.96.0.10",
		KubernetesPlugin: "kubernetes cluster.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"denial 9984 3 1",
		"kubernetes cluster.local",
	} {
		if !strings.Contains(corefile, want) {
			t.Fatalf("Corefile missing %q:\n%s", want, corefile)
		}
	}
}

func TestKubernetesPluginUsesOnlyResolvedAPIConfig(t *testing.T) {
	plugin, err := getKubernetesPluginStr()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plugin, "kubeconfig dns-kubeconfig") || strings.Contains(plugin, "endpoint ") {
		t.Fatalf("DNS uses a second API config: %s", plugin)
	}
}
