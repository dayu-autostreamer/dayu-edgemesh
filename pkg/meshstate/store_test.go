package meshstate

import (
	"net"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/pkg/proxy"
)

func TestStorePublishesOnlyExactAppliedManagedRoute(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-a", "runtime-service-a")

	if err := store.UpsertService(service); err != nil {
		t.Fatalf("upsert service: %v", err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatalf("upsert endpoints: %v", err)
	}

	route := requireRoute(t, store, "runtime-service-a")
	if route.Phase != PhaseReady || route.ProxyApplied {
		t.Fatalf("route before source/proxy acknowledgement = phase %q proxyApplied %v", route.Phase, route.ProxyApplied)
	}
	if route.FQDN != "runtime-a.default.svc.cluster.local" {
		t.Fatalf("unexpected FQDN %q", route.FQDN)
	}
	if route.EndpointPodUID != "pod-a" || route.DeploymentRevision != 7 {
		t.Fatalf("route identity not projected: %#v", route)
	}

	endpoint, managed, applied := store.ManagedEndpoint(servicePortName(service))
	if endpoint != route.Endpoint() || !managed || applied {
		t.Fatalf("pre-sync endpoint = %q managed=%v applied=%v", endpoint, managed, applied)
	}

	store.MarkPortalApplied(service.UID, servicePortName(service), true)
	if got := requireRoute(t, store, "runtime-service-a"); got.Phase != PhaseReady || !got.ProxyApplied {
		t.Fatalf("portal-before-source route = phase %q proxyApplied %v", got.Phase, got.ProxyApplied)
	}
	store.MarkSourceSynced()

	route = requireRoute(t, store, "runtime-service-a")
	if route.Phase != PhaseApplied || !route.ProxyApplied {
		t.Fatalf("applied route = phase %q proxyApplied %v", route.Phase, route.ProxyApplied)
	}
	endpoint, managed, applied = store.ManagedEndpoint(servicePortName(service))
	if !managed || !applied || endpoint != "node-a:runtime-pod-a:10.244.0.10:8080" {
		t.Fatalf("published endpoint = %q managed=%v applied=%v", endpoint, managed, applied)
	}

	// Snapshot callers must not be able to mutate the published projection.
	snapshot := store.Snapshot()
	delete(snapshot.Routes, "runtime-service-a")
	if _, exists := store.Snapshot().Routes["runtime-service-a"]; !exists {
		t.Fatal("Snapshot returned the store's mutable route map")
	}
}

func TestStoreIgnoresUnlabelledLegacyService(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("legacy-service", "legacy-runtime")
	service.Spec.Type = v1.ServiceTypeNodePort
	delete(service.Labels, LabelMeshManaged)
	delete(endpoints.Labels, LabelMeshManaged)

	if err := store.UpsertService(service); err != nil {
		t.Fatalf("upsert legacy Service: %v", err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatalf("upsert legacy Endpoints: %v", err)
	}
	if routes := store.Snapshot().Routes; len(routes) != 0 {
		t.Fatalf("legacy Service entered managed projection: %#v", routes)
	}
	if endpoint, managed, applied := store.ManagedEndpoint(servicePortName(service)); endpoint != "" || managed || applied {
		t.Fatalf("legacy Service classified as managed: endpoint=%q managed=%v applied=%v", endpoint, managed, applied)
	}
}

func TestPortalPendingCanArriveBeforeInformerHandlers(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-a", "runtime-service-a")
	name := servicePortName(service)

	store.MarkPortalPending(service.UID, name)
	if _, managed, applied := store.ManagedEndpoint(name); !managed || applied {
		t.Fatalf("pending-only state must fail closed, managed=%v applied=%v", managed, applied)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatalf("upsert endpoints first: %v", err)
	}
	if err := store.UpsertService(service); err != nil {
		t.Fatalf("upsert service second: %v", err)
	}
	store.MarkSourceSynced()
	if _, managed, applied := store.ManagedEndpoint(name); !managed || applied {
		t.Fatalf("exact informer state without portal application must remain closed, managed=%v applied=%v", managed, applied)
	}
	store.MarkPortalApplied(service.UID, name, true)

	if _, managed, applied := store.ManagedEndpoint(name); !managed || !applied {
		t.Fatalf("exact informer state did not join portal application, managed=%v applied=%v", managed, applied)
	}
}

func TestManagedRouteContractRejectsAmbiguousState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1.Service, *v1.Endpoints)
		want   string
	}{
		{
			name: "missing target node",
			mutate: func(service *v1.Service, _ *v1.Endpoints) {
				delete(service.Annotations, AnnotationTargetNode)
			},
			want: "target-node",
		},
		{
			name: "identity mismatch",
			mutate: func(_ *v1.Service, endpoints *v1.Endpoints) {
				endpoints.Labels[LabelDeploymentRevision] = "8"
			},
			want: "identity label",
		},
		{
			name: "missing service uid",
			mutate: func(service *v1.Service, _ *v1.Endpoints) {
				service.UID = ""
			},
			want: "Service UID",
		},
		{
			name: "runtime id differs from service name",
			mutate: func(service *v1.Service, endpoints *v1.Endpoints) {
				service.Labels[LabelRuntimeID] = "other"
				endpoints.Labels[LabelRuntimeID] = "other"
			},
			want: "runtime-id",
		},
		{
			name: "non decimal revision",
			mutate: func(service *v1.Service, endpoints *v1.Endpoints) {
				service.Labels[LabelDeploymentRevision] = "r7"
				endpoints.Labels[LabelDeploymentRevision] = "r7"
			},
			want: "base-10 int64",
		},
		{
			name: "headless service",
			mutate: func(service *v1.Service, _ *v1.Endpoints) {
				service.Spec.ClusterIP = v1.ClusterIPNone
			},
			want: "valid ClusterIP",
		},
		{
			name: "named target port",
			mutate: func(service *v1.Service, _ *v1.Endpoints) {
				service.Spec.Ports[0].TargetPort = intstr.FromString("http")
			},
			want: "targetPort must be numeric",
		},
		{
			name: "service and target port differ",
			mutate: func(service *v1.Service, _ *v1.Endpoints) {
				service.Spec.Ports[0].Port++
			},
			want: "must equal numeric targetPort",
		},
		{
			name: "multiple ready addresses",
			mutate: func(_ *v1.Service, endpoints *v1.Endpoints) {
				endpoints.Subsets[0].Addresses = append(endpoints.Subsets[0].Addresses, endpoints.Subsets[0].Addresses[0])
			},
			want: "exactly one subset",
		},
		{
			name: "not ready address",
			mutate: func(_ *v1.Service, endpoints *v1.Endpoints) {
				endpoints.Subsets[0].NotReadyAddresses = []v1.EndpointAddress{{IP: "10.244.0.11"}}
			},
			want: "no not-ready",
		},
		{
			name: "wrong node",
			mutate: func(_ *v1.Service, endpoints *v1.Endpoints) {
				node := "node-b"
				endpoints.Subsets[0].Addresses[0].NodeName = &node
			},
			want: "target-node",
		},
		{
			name: "missing pod uid",
			mutate: func(_ *v1.Service, endpoints *v1.Endpoints) {
				endpoints.Subsets[0].Addresses[0].TargetRef.UID = ""
			},
			want: "Pod UID",
		},
		{
			name: "pod reference from another namespace",
			mutate: func(_ *v1.Service, endpoints *v1.Endpoints) {
				endpoints.Subsets[0].Addresses[0].TargetRef.Namespace = "other"
			},
			want: "Service namespace",
		},
		{
			name: "endpoint port mismatch",
			mutate: func(_ *v1.Service, endpoints *v1.Endpoints) {
				endpoints.Subsets[0].Ports[0].Port++
			},
			want: "does not match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore()
			service, endpoints := managedObjects("service-a", "runtime-service-a")
			tt.mutate(service, endpoints)
			if err := store.UpsertService(service); err != nil {
				t.Fatalf("upsert service: %v", err)
			}
			if err := store.UpsertEndpoints(endpoints); err != nil {
				t.Fatalf("upsert endpoints: %v", err)
			}
			route := requireRoute(t, store, "runtime-service-a")
			if route.Phase != PhaseDegraded || !strings.Contains(route.Reason, tt.want) {
				t.Fatalf("degraded route = phase %q reason %q, want reason containing %q", route.Phase, route.Reason, tt.want)
			}
			if endpoint, managed, applied := store.ManagedEndpoint(servicePortName(service)); !managed || applied {
				t.Fatalf("invalid route escaped fail-closed gate: %q managed=%v applied=%v", endpoint, managed, applied)
			}
		})
	}
}

func TestLogicalServiceIsOptionalAndEndpointsAnnotationsAreNotRequired(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-a", "runtime-service-a")
	delete(service.Annotations, AnnotationLogicalService)
	endpoints.Annotations = nil

	if err := store.UpsertService(service); err != nil {
		t.Fatalf("upsert service: %v", err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatalf("upsert endpoints: %v", err)
	}
	if route := requireRoute(t, store, "runtime-service-a"); route.LogicalService != "" || route.Phase != PhaseReady {
		t.Fatalf("optional logical service route = %#v", route)
	}
}

func TestStoreRejectsOutOfOrderProjectionUpdates(t *testing.T) {
	t.Run("Endpoints", func(t *testing.T) {
		store := NewStore()
		service, endpoints := managedObjects("service-a", "runtime-service-a")
		service.ResourceVersion = "20"
		endpoints.ResourceVersion = "20"
		applyManagedRoute(t, store, service, endpoints)

		stale := endpoints.DeepCopy()
		stale.ResourceVersion = "19"
		stale.Subsets = nil
		if err := store.UpsertEndpoints(stale); err != nil {
			t.Fatalf("upsert stale endpoints: %v", err)
		}
		if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseApplied || route.EndpointPodUID != "pod-a" {
			t.Fatalf("older Endpoints update replaced applied route: %#v", route)
		}

		newerInvalid := stale.DeepCopy()
		newerInvalid.ResourceVersion = "21"
		if err := store.UpsertEndpoints(newerInvalid); err != nil {
			t.Fatalf("upsert newer invalid endpoints: %v", err)
		}
		if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseDegraded {
			t.Fatalf("newer invalid Endpoints did not fail closed: %#v", route)
		}

		recovered := endpoints.DeepCopy()
		recovered.ResourceVersion = "22"
		if err := store.UpsertEndpoints(recovered); err != nil {
			t.Fatalf("recover endpoints: %v", err)
		}
		if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseApplied || route.EndpointPodUID != "pod-a" {
			t.Fatalf("newer valid Endpoints did not recover route: %#v", route)
		}
	})

	t.Run("Service", func(t *testing.T) {
		store := NewStore()
		service, endpoints := managedObjects("service-a", "runtime-service-a")
		service.ResourceVersion = "20"
		endpoints.ResourceVersion = "20"
		applyManagedRoute(t, store, service, endpoints)

		stale := service.DeepCopy()
		stale.ResourceVersion = "19"
		delete(stale.Annotations, AnnotationTargetNode)
		if err := store.UpsertService(stale); err != nil {
			t.Fatalf("upsert stale service: %v", err)
		}
		if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseApplied || route.TargetNode != "node-a" {
			t.Fatalf("older Service update replaced applied route: %#v", route)
		}

		newerInvalid := stale.DeepCopy()
		newerInvalid.ResourceVersion = "21"
		if err := store.UpsertService(newerInvalid); err != nil {
			t.Fatalf("upsert newer invalid service: %v", err)
		}
		if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseDegraded {
			t.Fatalf("newer invalid Service did not fail closed: %#v", route)
		}

		recovered := service.DeepCopy()
		recovered.ResourceVersion = "22"
		if err := store.UpsertService(recovered); err != nil {
			t.Fatalf("recover service: %v", err)
		}
		if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseApplied || route.TargetNode != "node-a" {
			t.Fatalf("newer valid Service did not recover route: %#v", route)
		}
	})
}

func TestIsOlderNumericResourceVersion(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		incoming string
		want     bool
	}{
		{name: "older", current: "20", incoming: "19", want: true},
		{name: "newer", current: "20", incoming: "21", want: false},
		{name: "equal", current: "20", incoming: "20", want: false},
		{name: "arbitrary precision", current: "100000000000000000000000000000000000000", incoming: "99999999999999999999999999999999999999", want: true},
		{name: "leading zeroes are not canonical", current: "00020", incoming: "0019", want: false},
		{name: "zero is not an object version", current: "20", incoming: "0", want: false},
		{name: "empty current", current: "", incoming: "19", want: false},
		{name: "empty incoming", current: "20", incoming: "", want: false},
		{name: "opaque current", current: "rv-20", incoming: "19", want: false},
		{name: "opaque incoming", current: "20", incoming: "rv-19", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isOlderNumericResourceVersion(test.current, test.incoming); got != test.want {
				t.Fatalf("isOlderNumericResourceVersion(%q, %q) = %t, want %t", test.current, test.incoming, got, test.want)
			}
		})
	}
}

func TestServiceIncarnationReplacementAndStaleDeletes(t *testing.T) {
	store := NewStore()
	oldService, oldEndpoints := managedObjects("service-a", "runtime-service-a")
	applyManagedRoute(t, store, oldService, oldEndpoints)

	newService, newEndpoints := managedObjects("service-b", "runtime-service-b")
	if err := store.UpsertService(newService); err != nil {
		t.Fatalf("replace service: %v", err)
	}
	if _, exists := store.Snapshot().Routes["runtime-service-a"]; exists {
		t.Fatal("old RuntimeService route survived Service UID replacement")
	}
	if _, managed, applied := store.ManagedEndpoint(servicePortName(newService)); !managed || applied {
		t.Fatalf("replacement before exact Endpoints must fail closed, managed=%v applied=%v", managed, applied)
	}
	if err := store.UpsertEndpoints(newEndpoints); err != nil {
		t.Fatalf("replace endpoints: %v", err)
	}
	store.MarkPortalApplied(newService.UID, servicePortName(newService), true)
	if route := requireRoute(t, store, "runtime-service-b"); route.Phase != PhaseApplied || route.ServiceUID != newService.UID {
		t.Fatalf("new incarnation not applied: %#v", route)
	}

	if err := store.DeleteService(oldService); err != nil {
		t.Fatalf("stale service delete: %v", err)
	}
	if err := store.DeleteEndpoints(oldEndpoints); err != nil {
		t.Fatalf("stale endpoints delete: %v", err)
	}
	if route := requireRoute(t, store, "runtime-service-b"); route.Phase != PhaseApplied {
		t.Fatalf("stale delete damaged current incarnation: %#v", route)
	}
}

func TestRuntimeServiceUIDCollisionPreservesExistingValidRoute(t *testing.T) {
	store := NewStore()
	oldService, oldEndpoints := managedObjects("service-a", "runtime-service-shared")
	applyManagedRoute(t, store, oldService, oldEndpoints)

	newService, newEndpoints := managedObjects("service-b", "runtime-service-shared")
	newService.Name = "runtime-b"
	newService.Labels[LabelRuntimeID] = "runtime-b"
	newService.Spec.ClusterIP = "10.96.0.43"
	newEndpoints.Name = "runtime-b"
	newEndpoints.Labels[LabelRuntimeID] = "runtime-b"
	newEndpoints.Subsets[0].Addresses[0].IP = "10.244.0.11"
	newEndpoints.Subsets[0].Addresses[0].TargetRef.Name = "runtime-pod-b"
	newEndpoints.Subsets[0].Addresses[0].TargetRef.UID = "pod-b"
	if err := store.UpsertService(newService); err != nil {
		t.Fatalf("upsert colliding service: %v", err)
	}
	if err := store.UpsertEndpoints(newEndpoints); err != nil {
		t.Fatalf("upsert colliding endpoints: %v", err)
	}

	oldRoute := requireRoute(t, store, "runtime-service-shared")
	if oldRoute.ServiceUID != oldService.UID || oldRoute.Phase != PhaseApplied {
		t.Fatalf("valid route was overwritten by colliding identity: %#v", oldRoute)
	}
	newRoute := requireRoute(t, store, "invalid/service-b")
	if newRoute.ServiceUID != newService.UID || newRoute.Phase != PhaseDegraded || !strings.Contains(newRoute.Reason, "already belongs") {
		t.Fatalf("colliding route was not isolated as degraded: %#v", newRoute)
	}
	if _, managed, applied := store.ManagedEndpoint(servicePortName(newService)); !managed || applied {
		t.Fatalf("colliding managed Service escaped fail-closed gate: managed=%v applied=%v", managed, applied)
	}
}

func TestSourceRecoveryOnlyRestoresStillVerifiedRoutes(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-a", "runtime-service-a")
	applyManagedRoute(t, store, service, endpoints)

	store.MarkSourceDegraded("MetaServer disconnected")
	if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseDegraded {
		t.Fatalf("source degradation did not close applied route: %#v", route)
	}
	store.MarkSourceSynced()
	if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseApplied {
		t.Fatalf("verified route did not recover after source sync: %#v", route)
	}

	if err := store.DeleteEndpoints(endpoints); err != nil {
		t.Fatalf("delete endpoints: %v", err)
	}
	store.MarkSourceDegraded("MetaServer disconnected again")
	store.MarkSourceSynced()
	if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseDegraded {
		t.Fatalf("unverified route was incorrectly restored after source sync: %#v", route)
	}
	if _, managed, applied := store.ManagedEndpoint(servicePortName(service)); !managed || applied {
		t.Fatalf("unverified route escaped fail-closed gate: managed=%v applied=%v", managed, applied)
	}
}

func TestIdentityChangeOnSameServiceUIDCannotLeaveOldRuntimeRoute(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-a", "runtime-service-a")
	applyManagedRoute(t, store, service, endpoints)

	service = service.DeepCopy()
	endpoints = endpoints.DeepCopy()
	service.Labels[LabelRuntimeServiceUID] = "runtime-service-b"
	endpoints.Labels[LabelRuntimeServiceUID] = "runtime-service-b"
	if err := store.UpsertService(service); err != nil {
		t.Fatalf("upsert changed identity service: %v", err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatalf("upsert changed identity endpoints: %v", err)
	}
	if _, exists := store.Snapshot().Routes["runtime-service-a"]; exists {
		t.Fatal("old runtime UID route survived identity change")
	}
	if route := requireRoute(t, store, "runtime-service-b"); route.Phase != PhaseApplied || route.RuntimeID != service.Name {
		t.Fatalf("identity replacement route = %#v", route)
	}
}

func TestLabelRemovalAndEndpointsLossFailClosed(t *testing.T) {
	t.Run("service label removal remains managed until portal teardown", func(t *testing.T) {
		store := NewStore()
		service, endpoints := managedObjects("service-a", "runtime-service-a")
		applyManagedRoute(t, store, service, endpoints)

		legacy := service.DeepCopy()
		delete(legacy.Labels, LabelMeshManaged)
		if err := store.UpsertService(legacy); err != nil {
			t.Fatalf("remove service label: %v", err)
		}
		if _, managed, applied := store.ManagedEndpoint(servicePortName(service)); !managed || applied {
			t.Fatalf("portal still exists, route must fail closed: managed=%v applied=%v", managed, applied)
		}
		store.MarkPortalApplied(service.UID, servicePortName(service), false)
		if _, managed, _ := store.ManagedEndpoint(servicePortName(service)); !managed {
			t.Fatal("REMOVING cleared the managed tombstone before teardown completed")
		}
		store.MarkPortalRemoved(service.UID, servicePortName(service))
		if _, managed, _ := store.ManagedEndpoint(servicePortName(service)); managed {
			t.Fatal("legacy Service remained managed after REMOVED")
		}
	})

	t.Run("proxier teardown before informer removal remains fail closed", func(t *testing.T) {
		store := NewStore()
		service, endpoints := managedObjects("service-a", "runtime-service-a")
		applyManagedRoute(t, store, service, endpoints)

		store.MarkPortalApplied(service.UID, servicePortName(service), false)
		if _, managed, applied := store.ManagedEndpoint(servicePortName(service)); !managed || applied {
			t.Fatalf("current managed informer object must retain tombstone: managed=%v applied=%v", managed, applied)
		}
		legacy := service.DeepCopy()
		delete(legacy.Labels, LabelMeshManaged)
		if err := store.UpsertService(legacy); err != nil {
			t.Fatalf("remove service label: %v", err)
		}
		if _, managed, applied := store.ManagedEndpoint(servicePortName(service)); !managed || applied {
			t.Fatalf("informer removal cleared REMOVING tombstone: managed=%v applied=%v", managed, applied)
		}
		store.MarkPortalRemoved(service.UID, servicePortName(service))
		if _, managed, _ := store.ManagedEndpoint(servicePortName(service)); managed {
			t.Fatal("managed tombstone survived both REMOVED and informer label removal")
		}
	})

	t.Run("multi-port invalid service remains managed until every portal is removed", func(t *testing.T) {
		store := NewStore()
		service, _ := managedObjects("service-a", "runtime-service-a")
		service.Spec.Ports = append(service.Spec.Ports, v1.ServicePort{
			Name: "extra", Protocol: v1.ProtocolTCP, Port: 9001,
		})
		if err := store.UpsertService(service); err != nil {
			t.Fatalf("upsert invalid multi-port service: %v", err)
		}
		first := servicePortName(service)
		second := first
		second.Port = "extra"
		store.MarkPortalPending(service.UID, first)
		store.MarkPortalPending(service.UID, second)

		legacy := service.DeepCopy()
		delete(legacy.Labels, LabelMeshManaged)
		if err := store.UpsertService(legacy); err != nil {
			t.Fatalf("remove managed label: %v", err)
		}
		store.MarkPortalRemoved(service.UID, first)
		if _, managed, applied := store.ManagedEndpoint(second); !managed || applied {
			t.Fatalf("first REMOVED released tombstone while second portal remained: managed=%v applied=%v", managed, applied)
		}
		store.MarkPortalRemoved(service.UID, second)
		if _, managed, _ := store.ManagedEndpoint(second); managed {
			t.Fatal("managed tombstone survived removal of every portal")
		}
	})

	t.Run("non-managed endpoints update supersedes current projection", func(t *testing.T) {
		store := NewStore()
		service, endpoints := managedObjects("service-a", "runtime-service-a")
		applyManagedRoute(t, store, service, endpoints)

		nonManaged := endpoints.DeepCopy()
		nonManaged.UID = "endpoints-new"
		delete(nonManaged.Labels, LabelMeshManaged)
		if err := store.UpsertEndpoints(nonManaged); err != nil {
			t.Fatalf("remove endpoints label: %v", err)
		}
		route := requireRoute(t, store, "runtime-service-a")
		if route.Phase != PhaseDegraded || !strings.Contains(route.Reason, "label removed") {
			t.Fatalf("non-managed Endpoints update did not degrade route: %#v", route)
		}
		if _, managed, applied := store.ManagedEndpoint(servicePortName(service)); !managed || applied {
			t.Fatalf("degraded route escaped fail-closed gate: managed=%v applied=%v", managed, applied)
		}
	})

	t.Run("current endpoints deletion degrades but stale deletion is ignored", func(t *testing.T) {
		store := NewStore()
		service, endpoints := managedObjects("service-a", "runtime-service-a")
		applyManagedRoute(t, store, service, endpoints)

		stale := endpoints.DeepCopy()
		stale.UID = "stale-endpoints"
		if err := store.DeleteEndpoints(stale); err != nil {
			t.Fatalf("delete stale endpoints: %v", err)
		}
		if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseApplied {
			t.Fatalf("stale endpoints delete damaged route: %#v", route)
		}
		if err := store.DeleteEndpoints(endpoints); err != nil {
			t.Fatalf("delete current endpoints: %v", err)
		}
		if route := requireRoute(t, store, "runtime-service-a"); route.Phase != PhaseDegraded {
			t.Fatalf("current endpoints deletion did not degrade route: %#v", route)
		}
	})
}

func applyManagedRoute(t *testing.T, store *Store, service *v1.Service, endpoints *v1.Endpoints) {
	t.Helper()
	if err := store.UpsertService(service); err != nil {
		t.Fatalf("upsert service: %v", err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatalf("upsert endpoints: %v", err)
	}
	store.MarkSourceSynced()
	store.MarkPortalApplied(service.UID, servicePortName(service), true)
	if route := requireRoute(t, store, service.Labels[LabelRuntimeServiceUID]); route.Phase != PhaseApplied {
		t.Fatalf("route did not become applied: %#v", route)
	}
}

func requireRoute(t *testing.T, store *Store, runtimeServiceUID string) Route {
	t.Helper()
	route, exists := store.Snapshot().Routes[runtimeServiceUID]
	if !exists {
		t.Fatalf("route %q not found in %#v", runtimeServiceUID, store.Snapshot().Routes)
	}
	return route
}

func servicePortName(service *v1.Service) proxy.ServicePortName {
	return proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: service.Namespace, Name: service.Name},
		Port:           service.Spec.Ports[0].Name,
	}
}

func managedObjects(serviceUID, runtimeServiceUID string) (*v1.Service, *v1.Endpoints) {
	labels := map[string]string{
		LabelMeshManaged:        "true",
		LabelInstallID:          "install-a",
		LabelDeploymentRevision: "7",
		LabelRuntimeID:          "runtime-a",
		LabelComponent:          "processor",
		LabelRuntimeServiceUID:  runtimeServiceUID,
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "runtime-a",
			UID:       types.UID(serviceUID),
			Labels:    copyLabels(labels),
			Annotations: map[string]string{
				AnnotationLogicalService: "detector",
				AnnotationTargetNode:     "node-a",
			},
		},
		Spec: v1.ServiceSpec{
			Type:      v1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.42",
			Ports: []v1.ServicePort{{
				Name:       "runtime",
				Protocol:   v1.ProtocolTCP,
				Port:       8080,
				TargetPort: intstr.FromInt(8080),
			}},
		},
	}
	node := "node-a"
	endpoints := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "runtime-a",
			UID:       types.UID("endpoints-" + serviceUID),
			Labels:    copyLabels(labels),
		},
		Subsets: []v1.EndpointSubset{{
			Addresses: []v1.EndpointAddress{{
				IP:       "10.244.0.10",
				NodeName: &node,
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: "default",
					Name:      "runtime-pod-a",
					UID:       "pod-a",
				},
			}},
			Ports: []v1.EndpointPort{{Name: "runtime", Protocol: v1.ProtocolTCP, Port: 8080}},
		}},
	}
	if net.ParseIP(service.Spec.ClusterIP) == nil {
		panic("test fixture has invalid ClusterIP")
	}
	return service, endpoints
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
