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

func TestManagedHostsPublicationFailureDegradesAndRecoversRouteState(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "managed.hosts")
	store, err := NewStoreWithManagedHosts(path)
	if err != nil {
		t.Fatal(err)
	}

	service, endpoints := managedObjects("service-a", "runtime-service-a")
	if err := store.UpsertService(service); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatal(err)
	}
	store.MarkSourceSynced()
	route := store.Snapshot().Routes["runtime-service-a"]

	// Renaming a temporary file over a directory must fail. The status snapshot
	// must fail closed even though the proxy portal itself is already applied.
	store.managedHostsPath = tempDir
	store.MarkPortalApplied(service.UID, route.ServicePortName(), true)
	failed := store.Snapshot()
	if failed.SourceState != SourceDegraded || store.projectionError == "" {
		t.Fatalf("DNS publication failure was not exposed: state=%s error=%q", failed.SourceState, store.projectionError)
	}
	candidates := store.repairCandidates()
	if len(candidates) != 1 || candidates[0].Namespace != service.Namespace || candidates[0].Name != service.Name {
		t.Fatalf("DNS publication failure did not expose a repair candidate: %#v", candidates)
	}

	store.managedHostsPath = path
	if err := store.UpsertService(service); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatal(err)
	}
	recovered := store.Snapshot()
	if recovered.SourceState != SourceSynced || store.projectionError != "" {
		t.Fatalf("DNS projection did not recover: state=%s error=%q", recovered.SourceState, store.projectionError)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(content)), route.ServiceIP+" "+route.FQDN; got != want {
		t.Fatalf("recovered managed hosts = %q, want %q", got, want)
	}
}
