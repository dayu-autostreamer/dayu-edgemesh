package meshstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedHostsFollowsAppliedRouteLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.hosts")
	store, err := NewStoreWithManagedHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	assertHosts := func(want string) {
		t.Helper()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(content)); got != want {
			t.Fatalf("managed hosts = %q, want %q", got, want)
		}
	}
	assertHosts("")

	service, endpoints := managedObjects("service-a", "runtime-service-a")
	if err := store.UpsertService(service); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatal(err)
	}
	store.MarkSourceSynced()
	assertHosts("")

	route := store.Snapshot().Routes["runtime-service-a"]
	store.MarkPortalApplied(service.UID, route.ServicePortName(), true)
	assertHosts(route.ServiceIP + " " + route.FQDN)

	store.MarkSourceDegraded("test source loss")
	assertHosts("")

	store.MarkSourceSynced()
	assertHosts(route.ServiceIP + " " + route.FQDN)

	if err := store.DeleteEndpoints(endpoints); err != nil {
		t.Fatal(err)
	}
	assertHosts("")
}
