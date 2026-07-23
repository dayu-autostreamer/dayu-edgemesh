package dns

import (
	"strings"
	"testing"
)

func TestStubDomainIncludesManagedHostsAndShortNegativeCache(t *testing.T) {
	corefile, err := getStubDomainStr(map[string][]string{
		".": {"10.96.0.10"},
	}, &stubDomainInfo{
		LocalIP:                 "169.254.96.16",
		Port:                    "53",
		CacheTTL:                30,
		UpstreamServers:         "10.96.0.10",
		KubernetesPlugin:        "kubernetes cluster.local",
		ManagedRuntimeHostsFile: "/tmp/managed.hosts",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"hosts /tmp/managed.hosts cluster.local",
		"reload 1s",
		"denial 9984 3 1",
		"kubernetes cluster.local",
	} {
		if !strings.Contains(corefile, want) {
			t.Fatalf("Corefile missing %q:\n%s", want, corefile)
		}
	}
}
