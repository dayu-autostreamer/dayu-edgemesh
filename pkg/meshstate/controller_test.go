package meshstate

import (
	"errors"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

func awaitProjection(t *testing.T, condition func() bool) {
	t.Helper()
	if err := wait.PollImmediate(10*time.Millisecond, 10*time.Second, func() (bool, error) { return condition(), nil }); err != nil {
		t.Fatal(err)
	}
}

func TestRelistRemovesAppliedRouteButAPIErrorPreservesIt(t *testing.T) {
	service, endpoints := managedObjects("service-a", "runtime-service-a")
	var mu sync.Mutex
	items := []v1.Service{*service}
	unavailable, failures := false, 0
	watches := make(chan *watch.RaceFreeFakeWatcher, 4)
	services := cache.NewSharedIndexInformer(&cache.ListWatch{
		ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
			mu.Lock()
			defer mu.Unlock()
			if unavailable {
				failures++
				return nil, errors.New("API unavailable")
			}
			return &v1.ServiceList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: items}, nil
		},
		WatchFunc: func(metav1.ListOptions) (watch.Interface, error) {
			w := watch.NewRaceFreeFake()
			watches <- w
			return w, nil
		},
	}, &v1.Service{}, 0, cache.Indexers{})
	eps := cache.NewSharedIndexInformer(&cache.ListWatch{
		ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
			return &v1.EndpointsList{Items: []v1.Endpoints{*endpoints}}, nil
		},
		WatchFunc: func(metav1.ListOptions) (watch.Interface, error) { return watch.NewRaceFreeFake(), nil },
	}, &v1.Endpoints{}, 0, cache.Indexers{})
	store := NewStore()
	stop := make(chan struct{})
	defer close(stop)
	NewController(store).Register(services, eps, stop)
	go services.Run(stop)
	go eps.Run(stop)
	awaitProjection(t, func() bool {
		route, ok := store.Snapshot().Routes["runtime-service-a"]
		return ok && route.Phase == PhaseReady && store.Snapshot().SourceState == SourceSynced
	})
	store.MarkPortalApplied(service.UID, servicePortName(service), true)
	if requireRoute(t, store, "runtime-service-a").Phase != PhaseApplied {
		t.Fatal("route did not become applied")
	}
	w := <-watches
	mu.Lock()
	unavailable = true
	items = nil
	mu.Unlock()
	// 410 forces a new LIST, whose errors must not be interpreted as absence.
	w.Error(&metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired})
	awaitProjection(t, func() bool { mu.Lock(); defer mu.Unlock(); return failures > 0 })
	if requireRoute(t, store, "runtime-service-a").Phase != PhaseApplied {
		t.Fatal("API error retired a valid cached route")
	}
	mu.Lock()
	unavailable = false
	mu.Unlock()
	awaitProjection(t, func() bool { _, exists := store.Snapshot().Routes["runtime-service-a"]; return !exists })
}

func TestDeleteTombstoneCannotRetireReplacementUID(t *testing.T) {
	store := NewStore()
	service, endpoints := managedObjects("service-new", "runtime-service-new")
	applyManagedRoute(t, store, service, endpoints)
	old := service.DeepCopy()
	old.UID = "service-old"
	controller := NewController(store)
	controller.onServiceDelete(cache.DeletedFinalStateUnknown{Obj: old})
	if _, ok := store.Snapshot().Routes["runtime-service-new"]; !ok {
		t.Fatal("old tombstone removed replacement")
	}
	controller.onServiceDelete(cache.DeletedFinalStateUnknown{Obj: service})
	if _, ok := store.Snapshot().Routes["runtime-service-new"]; ok {
		t.Fatal("current tombstone retained route")
	}
}
