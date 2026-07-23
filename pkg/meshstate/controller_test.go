package meshstate

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeManagedObjectGetter struct {
	service      *v1.Service
	endpoints    *v1.Endpoints
	serviceErr   error
	endpointsErr error
	calls        []string
}

func (f *fakeManagedObjectGetter) GetService(_ context.Context, namespace, name string) (*v1.Service, error) {
	f.calls = append(f.calls, "services/"+namespace+"/"+name)
	if f.serviceErr != nil {
		return nil, f.serviceErr
	}
	return f.service.DeepCopy(), nil
}

func (f *fakeManagedObjectGetter) GetEndpoints(_ context.Context, namespace, name string) (*v1.Endpoints, error) {
	f.calls = append(f.calls, "endpoints/"+namespace+"/"+name)
	if f.endpointsErr != nil {
		return nil, f.endpointsErr
	}
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
	client.endpointsErr = apierrors.NewNotFound(
		schema.GroupResource{Resource: "endpoints"},
		recovered.Name,
	)
	client.calls = nil
	controller.repairManagedObjects(client)
	if len(client.calls) != len(wantCalls) || client.calls[0] != wantCalls[0] || client.calls[1] != wantCalls[1] {
		t.Fatalf("authoritative Endpoints recheck calls = %#v, want %#v", client.calls, wantCalls)
	}
	if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseDegraded {
		t.Fatalf("deleted Endpoints route reopened: %#v", route)
	}
}

func TestControllerRecoversManagedPortalWithCompletelyMissingProjection(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-a", "runtime-service-a")
	service.ResourceVersion = "20"
	endpoints.ResourceVersion = "20"
	store.MarkSourceSynced()

	// The proxier observed the managed Service and opened its portal, but the
	// independent mesh-state informer missed the Service/Endpoints Add events.
	store.MarkPortalPending(service.UID, servicePortName(service))
	store.MarkPortalApplied(service.UID, servicePortName(service), true)
	if _, exists := store.Snapshot().Routes["runtime-service-a"]; exists {
		t.Fatal("test setup unexpectedly created a managed route")
	}

	client := &fakeManagedObjectGetter{
		service:   service.DeepCopy(),
		endpoints: endpoints.DeepCopy(),
	}
	NewController(store).repairManagedObjects(client)

	route := requireRoute(t, store, "runtime-service-a")
	if route.Phase != PhaseApplied || route.EndpointPodUID != "pod-a" {
		t.Fatalf("missing managed projection was not recovered: %#v", route)
	}
	wantCalls := []string{
		"services/default/runtime-a",
		"endpoints/default/runtime-a",
	}
	if len(client.calls) != len(wantCalls) ||
		client.calls[0] != wantCalls[0] ||
		client.calls[1] != wantCalls[1] {
		t.Fatalf("repair calls = %#v, want %#v", client.calls, wantCalls)
	}
}

func TestControllerRetiresReadyProjectionMissingFromAuthoritativeAPI(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-a", "runtime-service-a")
	if err := store.UpsertService(service); err != nil {
		t.Fatalf("upsert service: %v", err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatalf("upsert endpoints: %v", err)
	}
	store.MarkSourceSynced()

	route := requireRoute(t, store, "runtime-service-a")
	if route.Phase != PhaseReady {
		t.Fatalf("route phase = %s, want %s", route.Phase, PhaseReady)
	}

	client := &fakeManagedObjectGetter{
		serviceErr: apierrors.NewNotFound(
			schema.GroupResource{Resource: "services"},
			service.Name,
		),
	}
	NewController(store).repairManagedObjects(client)

	if _, exists := store.Snapshot().Routes["runtime-service-a"]; exists {
		t.Fatal("authoritatively missing Service remained in managed route snapshot")
	}
	if err := store.UpsertService(service); err != nil {
		t.Fatalf("replay retired Service event: %v", err)
	}
	if _, exists := store.Snapshot().Routes["runtime-service-a"]; exists {
		t.Fatal("replayed informer event restored an authoritatively retired Service")
	}
	wantCalls := []string{"services/default/runtime-a"}
	if len(client.calls) != len(wantCalls) || client.calls[0] != wantCalls[0] {
		t.Fatalf("repair calls = %#v, want %#v", client.calls, wantCalls)
	}
}
