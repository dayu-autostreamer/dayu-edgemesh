package meshstate

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
)

type fakeManagedObjectGetter struct {
	service   *v1.Service
	endpoints *v1.Endpoints
	calls     []string
}

func (f *fakeManagedObjectGetter) GetService(_ context.Context, namespace, name string) (*v1.Service, error) {
	f.calls = append(f.calls, "services/"+namespace+"/"+name)
	return f.service.DeepCopy(), nil
}

func (f *fakeManagedObjectGetter) GetEndpoints(_ context.Context, namespace, name string) (*v1.Endpoints, error) {
	f.calls = append(f.calls, "endpoints/"+namespace+"/"+name)
	return f.endpoints.DeepCopy(), nil
}

func TestControllerRepairsOnlyInvalidManagedProjection(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-a", "runtime-service-a")
	service.ResourceVersion = "20"
	endpoints.ResourceVersion = "20"
	applyManagedRoute(t, store, service, endpoints)
	store.MarkSourceSynced()
	store.MarkPortalApplied(service.UID, servicePortName(service), true)

	client := &fakeManagedObjectGetter{service: service.DeepCopy(), endpoints: endpoints.DeepCopy()}
	controller := NewController(store)
	controller.repairManagedObjects(client)
	if len(client.calls) != 0 {
		t.Fatalf("valid projection triggered repair requests: %#v", client.calls)
	}

	invalid := endpoints.DeepCopy()
	invalid.ResourceVersion = "21"
	invalid.Subsets = nil
	if err := store.UpsertEndpoints(invalid); err != nil {
		t.Fatalf("degrade endpoints: %v", err)
	}
	if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseDegraded {
		t.Fatalf("invalid Endpoints did not degrade route: %#v", route)
	}

	recovered := endpoints.DeepCopy()
	recovered.ResourceVersion = "22"
	client.endpoints = recovered
	client.calls = nil
	controller.repairManagedObjects(client)

	if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseApplied || route.EndpointPodUID != "pod-a" {
		t.Fatalf("MetaServer point lookup did not recover route: %#v", route)
	}
	wantCalls := []string{"services/default/runtime-a", "endpoints/default/runtime-a"}
	if len(client.calls) != len(wantCalls) || client.calls[0] != wantCalls[0] || client.calls[1] != wantCalls[1] {
		t.Fatalf("repair calls = %#v, want %#v", client.calls, wantCalls)
	}

	if err := store.DeleteEndpoints(recovered); err != nil {
		t.Fatalf("delete endpoints: %v", err)
	}
	client.calls = nil
	controller.repairManagedObjects(client)
	if len(client.calls) != 0 {
		t.Fatalf("observed Endpoints deletion triggered stale-object repair: %#v", client.calls)
	}
	if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseDegraded {
		t.Fatalf("deleted Endpoints route reopened: %#v", route)
	}
}
