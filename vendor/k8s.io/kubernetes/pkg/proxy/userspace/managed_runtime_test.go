package userspace

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/proxy"
)

func TestSameConfigIncludesManagedServiceIncarnation(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "runtime-a", UID: "service-a"},
		Spec: v1.ServiceSpec{
			ClusterIP:       "10.96.0.42",
			SessionAffinity: v1.ServiceAffinityNone,
			Ports: []v1.ServicePort{{
				Name: "runtime", Protocol: v1.ProtocolTCP, Port: 8080,
			}},
		},
	}
	info := &ServiceInfo{
		serviceUID:          "service-a",
		managed:             true,
		protocol:            v1.ProtocolTCP,
		portal:              portal{ip: net.ParseIP("10.96.0.42"), port: 8080},
		sessionAffinityType: v1.ServiceAffinityNone,
	}
	if !sameConfig(info, service, &service.Spec.Ports[0], true) {
		t.Fatal("exact managed Service incarnation was considered changed")
	}
	changedUID := service.DeepCopy()
	changedUID.UID = "service-b"
	if sameConfig(info, changedUID, &changedUID.Spec.Ports[0], true) {
		t.Fatal("Service UID replacement was hidden by sameConfig")
	}
	if sameConfig(info, service, &service.Spec.Ports[0], false) {
		t.Fatal("managed-to-legacy transition was hidden by sameConfig")
	}
	info.teardownPending = true
	if sameConfig(info, service, &service.Spec.Ports[0], true) {
		t.Fatal("failed teardown was hidden instead of being reconciled again")
	}
}

type failingManagedPortAllocator struct{}

func (*failingManagedPortAllocator) AllocateNext() (int, error) {
	return 0, errors.New("injected allocation failure")
}

func (*failingManagedPortAllocator) Release(int) {}

type stagedCloseProxySocket struct {
	closeErrors []error
	closeCalls  int
	port        int
}

func (socket *stagedCloseProxySocket) Addr() net.Addr { return &net.TCPAddr{} }
func (socket *stagedCloseProxySocket) Close() error {
	var err error
	if socket.closeCalls < len(socket.closeErrors) {
		err = socket.closeErrors[socket.closeCalls]
	}
	socket.closeCalls++
	return err
}
func (*stagedCloseProxySocket) ProxyLoop(proxy.ServicePortName, *ServiceInfo, LoadBalancer) {}
func (socket *stagedCloseProxySocket) ListenPort() int                                      { return socket.port }

type recordingPortAllocator struct {
	released []int
}

func (*recordingPortAllocator) AllocateNext() (int, error) { return 0, nil }
func (allocator *recordingPortAllocator) Release(port int) {
	allocator.released = append(allocator.released, port)
}

type managedRetryRunner struct {
	runs int32
	done chan struct{}
}

func (runner *managedRetryRunner) Run() {
	if atomic.AddInt32(&runner.runs, 1) == 1 {
		close(runner.done)
	}
}

func (*managedRetryRunner) Loop(<-chan struct{}) {}

func TestManagedSetupFailureRetriesOnlyCurrentServiceUID(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "runtime-a", UID: "service-a"},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP, ClusterIP: "10.96.0.42",
			Ports: []v1.ServicePort{{Name: "runtime", Protocol: v1.ProtocolTCP, Port: 8080}},
		},
	}
	name := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	runner := &managedRetryRunner{done: make(chan struct{})}
	proxier := &Proxier{
		serviceMap:      make(map[proxy.ServicePortName]*ServiceInfo),
		serviceChanges:  make(map[types.NamespacedName]*serviceChange),
		desiredServices: map[types.NamespacedName]*v1.Service{name: service.DeepCopy()},
		proxyPorts:      &failingManagedPortAllocator{},
		syncRunner:      runner,
		minSyncPeriod:   time.Millisecond,
		stopChan:        make(chan struct{}),
	}

	proxier.mergeService(service, nil)
	select {
	case <-runner.done:
	case <-time.After(time.Second):
		t.Fatal("setup failure did not schedule a bounded retry")
	}
	proxier.serviceChangesLock.Lock()
	retry := proxier.serviceChanges[name]
	proxier.serviceChangesLock.Unlock()
	if retry == nil || retry.current == nil || retry.current.UID != service.UID {
		t.Fatalf("retry did not preserve current Service UID: %#v", retry)
	}

	// A later retry for the old UID must not overwrite a replacement.
	proxier.serviceChangesLock.Lock()
	proxier.serviceChanges = make(map[types.NamespacedName]*serviceChange)
	replacement := service.DeepCopy()
	replacement.UID = "service-b"
	proxier.desiredServices[name] = replacement
	proxier.serviceChangesLock.Unlock()
	proxier.scheduleServiceRetry(service)
	time.Sleep(5 * time.Millisecond)
	proxier.serviceChangesLock.Lock()
	_, staleQueued := proxier.serviceChanges[name]
	proxier.serviceChangesLock.Unlock()
	if staleQueued {
		t.Fatal("retry resurrected a replaced Service UID")
	}

	// If an old retry is already queued, a replacement's retry must supersede
	// its current payload instead of being suppressed by timer ownership.
	proxier.serviceChangesLock.Lock()
	proxier.serviceChanges[name] = &serviceChange{current: service.DeepCopy()}
	proxier.desiredServices[name] = replacement
	proxier.serviceChangesLock.Unlock()
	proxier.scheduleServiceRetry(replacement)
	proxier.serviceChangesLock.Lock()
	latest := proxier.serviceChanges[name]
	proxier.serviceChangesLock.Unlock()
	if latest == nil || latest.current == nil || latest.current.UID != replacement.UID {
		t.Fatalf("replacement retry did not supersede stale queued identity: %#v", latest)
	}
}

func TestReplacementPreservesLegacyLoadBalancerEndpoints(t *testing.T) {
	name := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "legacy"},
		Port:           "http",
	}
	loadBalancer := NewLoadBalancerRR()
	if err := loadBalancer.NewService(name, v1.ServiceAffinityNone, 0); err != nil {
		t.Fatal(err)
	}
	loadBalancer.lock.Lock()
	loadBalancer.services[name].endpoints = []string{"10.0.0.7:8080"}
	loadBalancer.lock.Unlock()

	info := &ServiceInfo{
		portalClosed:      true,
		socketClosed:      true,
		proxyPortReleased: true,
	}
	proxier := &Proxier{
		loadBalancer: loadBalancer,
		serviceMap:   map[proxy.ServicePortName]*ServiceInfo{name: info},
	}
	if err := proxier.cleanupPortalAndProxy(name, info, false); err != nil {
		t.Fatal(err)
	}
	if !loadBalancer.ServiceHasEndpoints(name) {
		t.Fatal("same-port replacement deleted legacy endpoints without an Endpoints event")
	}
}

func TestDesiredReplacementPreservesEndpointsAfterFinalTeardownRetry(t *testing.T) {
	oldService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "legacy", UID: "service-a"},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP, ClusterIP: "10.96.0.42",
			Ports: []v1.ServicePort{{Name: "http", Protocol: v1.ProtocolTCP, Port: 8080}},
		},
	}
	replacement := oldService.DeepCopy()
	replacement.UID = "service-b"
	name := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name},
		Port:           "http",
	}
	loadBalancer := NewLoadBalancerRR()
	if err := loadBalancer.NewService(name, v1.ServiceAffinityNone, 0); err != nil {
		t.Fatal(err)
	}
	loadBalancer.lock.Lock()
	loadBalancer.services[name].endpoints = []string{"10.0.0.8:8080"}
	loadBalancer.lock.Unlock()

	// The old incarnation requested final LB deletion, but its socket close
	// failed. Before the bounded retry completes, the replacement Service and
	// its Endpoints have already reached their respective informer handlers.
	info := &ServiceInfo{
		serviceUID:         oldService.UID,
		teardownPending:    true,
		removeLoadBalancer: true,
		portalClosed:       true,
		socketClosed:       true,
		proxyPortReleased:  true,
	}
	resourceName := types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}
	proxier := &Proxier{
		loadBalancer:    loadBalancer,
		serviceMap:      map[proxy.ServicePortName]*ServiceInfo{name: info},
		desiredServices: map[types.NamespacedName]*v1.Service{resourceName: replacement},
	}
	proxier.cleanupOrphanedServices()

	if _, exists := proxier.serviceMap[name]; exists {
		t.Fatal("successful old-incarnation teardown retry retained ServiceInfo")
	}
	if !loadBalancer.ServiceHasEndpoints(name) {
		t.Fatal("old final teardown retry erased endpoints already published for the desired replacement")
	}

	// The same ordering is possible after an earlier setup failure has already
	// removed ServiceInfo: a delete change is snapshotted, then the replacement
	// Service and Endpoints arrive before that old change is unmerged.
	withoutInfoLoadBalancer := NewLoadBalancerRR()
	if err := withoutInfoLoadBalancer.NewService(name, v1.ServiceAffinityNone, 0); err != nil {
		t.Fatal(err)
	}
	withoutInfoLoadBalancer.lock.Lock()
	withoutInfoLoadBalancer.services[name].endpoints = []string{"10.0.0.9:8080"}
	withoutInfoLoadBalancer.lock.Unlock()
	withoutInfo := &Proxier{
		loadBalancer:    withoutInfoLoadBalancer,
		serviceMap:      make(map[proxy.ServicePortName]*ServiceInfo),
		desiredServices: map[types.NamespacedName]*v1.Service{resourceName: replacement},
	}
	withoutInfo.unmergeService(oldService, nil)
	if !withoutInfoLoadBalancer.ServiceHasEndpoints(name) {
		t.Fatal("snapshotted old delete erased replacement endpoints when ServiceInfo was already absent")
	}
}

func TestTeardownIsIdempotentAndRetriesThroughBoundedSync(t *testing.T) {
	name := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "runtime-a"},
		Port:           "runtime",
	}
	injected := errors.New("injected close failure")
	socket := &stagedCloseProxySocket{
		closeErrors: []error{injected, injected, nil},
		port:        31001,
	}
	allocator := &recordingPortAllocator{}
	runner := &managedRetryRunner{done: make(chan struct{})}
	loadBalancer := NewLoadBalancerRR()
	if err := loadBalancer.NewService(name, v1.ServiceAffinityNone, 0); err != nil {
		t.Fatal(err)
	}
	info := &ServiceInfo{
		isAliveAtomic: 1,
		portalClosed:  true,
		socket:        socket,
		managed:       true,
		serviceUID:    "service-a",
	}
	proxier := &Proxier{
		loadBalancer: loadBalancer,
		serviceMap:   map[proxy.ServicePortName]*ServiceInfo{name: info},
		proxyPorts:   allocator,
		syncRunner:   runner,
	}
	if err := proxier.cleanupPortalAndProxy(name, info, true); err == nil {
		t.Fatal("first injected socket close failure was ignored")
	}
	if !info.teardownPending || !info.removeLoadBalancer {
		t.Fatal("failed final teardown did not retain its lifecycle stage")
	}

	proxier.cleanupOrphanedServices()
	if atomic.LoadInt32(&runner.runs) != 1 {
		t.Fatal("persistent teardown failure was not delegated to the bounded sync runner")
	}
	proxier.cleanupOrphanedServices()
	if _, exists := proxier.serviceMap[name]; exists {
		t.Fatal("successful retry retained the old ServiceInfo")
	}
	if len(allocator.released) != 1 || allocator.released[0] != socket.port {
		t.Fatalf("proxy port releases = %#v, want one exact release", allocator.released)
	}
	if loadBalancer.ServiceHasEndpoints(name) {
		t.Fatal("final teardown retained load-balancer state")
	}
	if !info.IsFinished() {
		t.Fatal("successful teardown retry did not finish the ServiceInfo")
	}
}

func TestDeletePromotesPendingReplacementToFinalTeardown(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "runtime-a", UID: "service-a"},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP, ClusterIP: "10.96.0.42",
			Ports: []v1.ServicePort{{Name: "runtime", Protocol: v1.ProtocolTCP, Port: 8080}},
		},
	}
	name := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: service.Namespace, Name: service.Name}, Port: "runtime"}
	loadBalancer := NewLoadBalancerRR()
	if err := loadBalancer.NewService(name, v1.ServiceAffinityNone, 0); err != nil {
		t.Fatal(err)
	}
	loadBalancer.lock.Lock()
	loadBalancer.services[name].endpoints = []string{"10.0.0.7:8080"}
	loadBalancer.lock.Unlock()

	info := &ServiceInfo{
		managed:            true,
		serviceUID:         service.UID,
		teardownPending:    true,
		portalClosed:       true,
		socketClosed:       true,
		proxyPortReleased:  true,
		removeLoadBalancer: false,
	}
	runner := &managedRetryRunner{done: make(chan struct{})}
	resourceName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	proxier := &Proxier{
		loadBalancer:    loadBalancer,
		serviceMap:      map[proxy.ServicePortName]*ServiceInfo{name: info},
		serviceChanges:  make(map[types.NamespacedName]*serviceChange),
		desiredServices: map[types.NamespacedName]*v1.Service{resourceName: service.DeepCopy()},
		syncRunner:      runner,
	}
	proxier.scheduleServiceRetry(service)
	proxier.serviceChange(service, nil, "delete during retry")
	proxier.serviceChangesLock.Lock()
	change := proxier.serviceChanges[resourceName]
	proxier.serviceChangesLock.Unlock()
	if change == nil || change.previous == nil || change.previous.UID != service.UID || change.current != nil {
		t.Fatalf("delete collapsed the retry change and lost final teardown: %#v", change)
	}
	proxier.cleanupOrphanedServices()
	if loadBalancer.ServiceHasEndpoints(name) {
		t.Fatal("final delete preserved endpoint state from replacement teardown")
	}

	// The same final delete must clean independent LB state even when a prior
	// setup cleanup already removed ServiceInfo from serviceMap.
	secondLoadBalancer := NewLoadBalancerRR()
	if err := secondLoadBalancer.NewService(name, v1.ServiceAffinityNone, 0); err != nil {
		t.Fatal(err)
	}
	secondLoadBalancer.lock.Lock()
	secondLoadBalancer.services[name].endpoints = []string{"10.0.0.8:8080"}
	secondLoadBalancer.lock.Unlock()
	second := &Proxier{
		loadBalancer: secondLoadBalancer,
		serviceMap:   make(map[proxy.ServicePortName]*ServiceInfo),
	}
	second.unmergeService(service, nil)
	if secondLoadBalancer.ServiceHasEndpoints(name) {
		t.Fatal("final delete retained LB state after ServiceInfo was already removed")
	}
}

func TestManagedServiceBypassesIndependentLegacyExistenceValidation(t *testing.T) {
	proxier := &Proxier{}
	proxier.SetNamespaceExistsHandler(func(string) bool { return false })
	proxier.SetServiceExistsHandler(func(proxy.ServicePortName) bool { return false })
	proxier.SetManagedServiceHandler(func(service *v1.Service) bool {
		return service.Labels["dayu.io/mesh-managed"] == "true"
	})
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "runtime-a",
			Labels:    map[string]string{"dayu.io/mesh-managed": "true"},
		},
		Spec: v1.ServiceSpec{ClusterIP: "10.96.0.42", Ports: []v1.ServicePort{{Name: "runtime", Port: 8080}}},
	}
	if ports := proxier.currentServicePortNames(service); !ports.Has("runtime") {
		t.Fatal("managed Service was incorrectly rejected by independent legacy validation")
	}
	legacy := service.DeepCopy()
	delete(legacy.Labels, "dayu.io/mesh-managed")
	if ports := proxier.currentServicePortNames(legacy); ports.Has("runtime") {
		t.Fatal("legacy Service unexpectedly bypassed independent validation")
	}
}

func TestManagedPortalCallbacksCarryExactUIDAndLifecycleOrder(t *testing.T) {
	proxier := &Proxier{}
	type event struct {
		phase string
		name  proxy.ServicePortName
		uid   types.UID
	}
	var events []event
	proxier.SetServicePendingHandler(func(name proxy.ServicePortName, uid types.UID) {
		events = append(events, event{phase: "PENDING", name: name, uid: uid})
	})
	proxier.SetServiceAppliedHandler(func(name proxy.ServicePortName, uid types.UID, applied bool) {
		phase := "REMOVING"
		if applied {
			phase = "APPLIED"
		}
		events = append(events, event{phase: phase, name: name, uid: uid})
	})
	proxier.SetServiceRemovedHandler(func(name proxy.ServicePortName, uid types.UID) {
		events = append(events, event{phase: "REMOVED", name: name, uid: uid})
	})
	name := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "runtime-a"},
		Port:           "runtime",
	}
	info := &ServiceInfo{managed: true, serviceUID: "service-a"}
	proxier.notifyServicePending(name, info)
	proxier.notifyServiceApplied(name, info, true)
	proxier.notifyServiceApplied(name, info, false)
	proxier.notifyServiceRemoved(name, info)
	if len(events) != 4 {
		t.Fatalf("callback events = %#v", events)
	}
	for i, wantPhase := range []string{"PENDING", "APPLIED", "REMOVING", "REMOVED"} {
		if events[i].phase != wantPhase || events[i].name != name || events[i].uid != "service-a" {
			t.Fatalf("event[%d] = %#v, want phase %s and exact identity", i, events[i], wantPhase)
		}
	}
	legacy := &ServiceInfo{managed: false, serviceUID: "legacy"}
	proxier.notifyServicePending(name, legacy)
	proxier.notifyServiceApplied(name, legacy, false)
	proxier.notifyServiceRemoved(name, legacy)
	if len(events) != 4 {
		t.Fatal("legacy Service emitted a managed portal lifecycle callback")
	}
}
