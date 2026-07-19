package userspace

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/kubernetes/pkg/proxy"
	proxyconfig "k8s.io/kubernetes/pkg/proxy/config"
	"k8s.io/kubernetes/pkg/util/iptables"
)

func TestOrderedServiceChangesPrioritizesReleases(t *testing.T) {
	changes := map[types.NamespacedName]*serviceChange{
		{Namespace: "ns", Name: "add"}: {
			current: testService("ns", "add", "tcp", "10.0.0.3", 80, 30003),
		},
		{Namespace: "ns", Name: "update"}: {
			previous: testService("ns", "update", "tcp", "10.0.0.2", 80, 30002),
			current:  testService("ns", "update", "tcp", "10.0.0.2", 80, 30022),
		},
		{Namespace: "ns", Name: "delete"}: {
			previous: testService("ns", "delete", "tcp", "10.0.0.1", 80, 30001),
		},
	}

	ordered := orderedServiceChanges(changes)
	if len(ordered) != 3 {
		t.Fatalf("expected 3 ordered changes, got %d", len(ordered))
	}

	if ordered[0].name.Name != "delete" {
		t.Fatalf("expected delete change first, got %q", ordered[0].name.Name)
	}
	if ordered[1].name.Name != "update" {
		t.Fatalf("expected update change second, got %q", ordered[1].name.Name)
	}
	if ordered[2].name.Name != "add" {
		t.Fatalf("expected add change last, got %q", ordered[2].name.Name)
	}
}

func TestSyncProxyRulesReleasesDeletedNodePortBeforeReusingIt(t *testing.T) {
	fakeIPT := newFakeIPTables()
	proxier, err := createProxier(
		&fakeLoadBalancer{},
		net.ParseIP("169.254.20.10"),
		fakeIPT,
		nil,
		net.ParseIP("169.254.20.10"),
		newPortAllocator(utilnet.PortRange{}),
		time.Second,
		time.Second,
		time.Second,
		newFakeProxySocket,
	)
	if err != nil {
		t.Fatalf("create proxier: %v", err)
	}

	oldSvc := testService("dayu-lhz", "controller-both", "tcp-0", "10.0.0.10", 80, 30263)
	newSvc := testService("dayu-hjy", "processor-license-plate-recognition-edge13-both", "tcp-0", "10.0.0.11", 80, 30263)

	if ports := proxier.mergeService(oldSvc, nil); ports.Len() != 1 {
		t.Fatalf("expected old service to expose 1 port, got %d", ports.Len())
	}

	key := portMapKey{ip: net.IP(nil).String(), port: 30263, protocol: v1.ProtocolTCP}
	value, found := proxier.portMap[key]
	if !found {
		t.Fatalf("expected old service to claim nodePort %d before sync", key.port)
	}
	if owner := value.owner; owner.NamespacedName.Name != "controller-both" {
		t.Fatalf("expected old service to own nodePort before sync, got %v", owner)
	}

	proxier.serviceChanges = map[types.NamespacedName]*serviceChange{
		{Namespace: oldSvc.Namespace, Name: oldSvc.Name}: {
			previous: oldSvc,
		},
		{Namespace: newSvc.Namespace, Name: newSvc.Name}: {
			current: newSvc,
		},
	}
	atomic.StoreInt32(&proxier.initialized, 1)

	proxier.syncProxyRules()

	expectedOwner := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: newSvc.Namespace, Name: newSvc.Name},
		Port:           newSvc.Spec.Ports[0].Name,
	}
	value, found = proxier.portMap[key]
	if !found {
		t.Fatalf("expected nodePort %d to stay claimed after sync", key.port)
	}
	if value.owner != expectedOwner {
		t.Fatalf("expected nodePort owner %v, got %v", expectedOwner, value.owner)
	}

	oldServiceName := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: oldSvc.Namespace, Name: oldSvc.Name},
		Port:           oldSvc.Spec.Ports[0].Name,
	}
	if _, exists := proxier.serviceMap[oldServiceName]; exists {
		t.Fatalf("expected old service %v to be removed from serviceMap", oldServiceName)
	}
	if _, exists := proxier.serviceMap[expectedOwner]; !exists {
		t.Fatalf("expected new service %v to exist in serviceMap", expectedOwner)
	}
}

func TestStaleTeardownPreservesReplacementNodePortOwner(t *testing.T) {
	fakeIPT := newFakeIPTables()
	proxier, err := createProxier(
		&fakeLoadBalancer{},
		net.ParseIP("169.254.20.10"),
		fakeIPT,
		nil,
		net.ParseIP("169.254.20.10"),
		newPortAllocator(utilnet.PortRange{}),
		time.Second,
		time.Second,
		time.Second,
		newFakeProxySocket,
	)
	if err != nil {
		t.Fatalf("create proxier: %v", err)
	}

	staleService := testService("shy-dayu", "scheduler-cloud", "tcp-0", "10.0.0.10", 80, 32701)
	if ports := proxier.mergeService(staleService, nil); ports.Len() != 1 {
		t.Fatalf("expected stale service to expose 1 port, got %d", ports.Len())
	}
	staleName := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: staleService.Namespace, Name: staleService.Name},
		Port:           staleService.Spec.Ports[0].Name,
	}
	staleInfo := proxier.serviceMap[staleName]
	if staleInfo == nil {
		t.Fatal("stale service was not installed")
	}

	key := portMapKey{ip: net.IP(nil).String(), port: 32701, protocol: v1.ProtocolTCP}
	replacementName := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: "dayu", Name: "processor-vehicle-detection"},
		Port:           "tcp-0",
	}
	replacementSocket := &fakeProxySocket{addr: &net.TCPAddr{IP: net.IPv4zero, Port: key.port}, port: key.port}
	proxier.portMap[key] = &portMapValue{owner: replacementName, socket: replacementSocket}

	if err := proxier.cleanupPortalAndProxy(staleName, staleInfo, true); err != nil {
		t.Fatalf("stale teardown failed after NodePort ownership moved: %v", err)
	}
	claim, found := proxier.portMap[key]
	if !found || claim.owner != replacementName {
		t.Fatalf("replacement NodePort claim = %#v, want owner %v", claim, replacementName)
	}
	if replacementSocket.closed.Load() {
		t.Fatal("stale teardown closed the replacement owner's NodePort socket")
	}
	if _, found := proxier.serviceMap[staleName]; found {
		t.Fatal("stale service remained in proxier state after successful teardown")
	}
	if staleInfo.teardownPending || !staleInfo.portalClosed || !staleInfo.socketClosed {
		t.Fatalf("stale teardown state was not committed: %#v", staleInfo)
	}
}

func TestSyncProxyRulesCleansServiceMissingFromCurrentState(t *testing.T) {
	fakeIPT := newFakeIPTables()
	proxier, err := createProxier(
		&fakeLoadBalancer{},
		net.ParseIP("169.254.20.10"),
		fakeIPT,
		nil,
		net.ParseIP("169.254.20.10"),
		newPortAllocator(utilnet.PortRange{}),
		time.Second,
		time.Second,
		time.Second,
		newFakeProxySocket,
	)
	if err != nil {
		t.Fatalf("create proxier: %v", err)
	}

	orphanSvc := testService("dayu-lhz", "controller-both", "tcp-0", "10.0.0.10", 80, 30263)
	serviceName := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: orphanSvc.Namespace, Name: orphanSvc.Name},
		Port:           orphanSvc.Spec.Ports[0].Name,
	}
	proxier.SetNamespaceExistsHandler(func(namespace string) bool {
		return namespace == orphanSvc.Namespace
	})
	proxier.SetServiceExistsHandler(func(proxy.ServicePortName) bool {
		return false
	})

	proxier.mergeService(orphanSvc, nil)
	atomic.StoreInt32(&proxier.initialized, 1)

	proxier.syncProxyRules()

	if _, exists := proxier.serviceMap[serviceName]; exists {
		t.Fatalf("expected orphaned service %v to be removed from serviceMap", serviceName)
	}
	key := portMapKey{ip: net.IP(nil).String(), port: 30263, protocol: v1.ProtocolTCP}
	if _, found := proxier.portMap[key]; found {
		t.Fatalf("expected orphaned nodePort %d to be released", key.port)
	}
}

func TestSyncProxyRulesCleansServiceInDeletedNamespace(t *testing.T) {
	fakeIPT := newFakeIPTables()
	proxier, err := createProxier(
		&fakeLoadBalancer{},
		net.ParseIP("169.254.20.10"),
		fakeIPT,
		nil,
		net.ParseIP("169.254.20.10"),
		newPortAllocator(utilnet.PortRange{}),
		time.Second,
		time.Second,
		time.Second,
		newFakeProxySocket,
	)
	if err != nil {
		t.Fatalf("create proxier: %v", err)
	}

	orphanSvc := testService("dayu-lhz", "controller-both", "tcp-0", "10.0.0.10", 80, 30263)
	serviceName := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: orphanSvc.Namespace, Name: orphanSvc.Name},
		Port:           orphanSvc.Spec.Ports[0].Name,
	}
	proxier.SetNamespaceExistsHandler(func(string) bool {
		return false
	})
	proxier.SetServiceExistsHandler(func(proxy.ServicePortName) bool {
		return true
	})

	proxier.mergeService(orphanSvc, nil)
	atomic.StoreInt32(&proxier.initialized, 1)

	proxier.syncProxyRules()

	if _, exists := proxier.serviceMap[serviceName]; exists {
		t.Fatalf("expected deleted-namespace service %v to be removed from serviceMap", serviceName)
	}
	key := portMapKey{ip: net.IP(nil).String(), port: 30263, protocol: v1.ProtocolTCP}
	if _, found := proxier.portMap[key]; found {
		t.Fatalf("expected deleted-namespace nodePort %d to be released", key.port)
	}
}

func TestCurrentServicePortNamesSkipsStalePorts(t *testing.T) {
	fakeIPT := newFakeIPTables()
	proxier, err := createProxier(
		&fakeLoadBalancer{},
		net.ParseIP("169.254.20.10"),
		fakeIPT,
		nil,
		net.ParseIP("169.254.20.10"),
		newPortAllocator(utilnet.PortRange{}),
		time.Second,
		time.Second,
		time.Second,
		newFakeProxySocket,
	)
	if err != nil {
		t.Fatalf("create proxier: %v", err)
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "dayu-lhz",
			Name:      "controller-both",
		},
		Spec: v1.ServiceSpec{
			ClusterIP: "10.0.0.10",
			Ports: []v1.ServicePort{
				{Name: "tcp-live", Protocol: v1.ProtocolTCP, Port: 80, NodePort: 30263},
				{Name: "tcp-stale", Protocol: v1.ProtocolTCP, Port: 81, NodePort: 30264},
			},
		},
	}
	proxier.SetNamespaceExistsHandler(func(string) bool { return true })
	proxier.SetServiceExistsHandler(func(servicePort proxy.ServicePortName) bool {
		return servicePort.Port == "tcp-live"
	})

	ports := proxier.currentServicePortNames(svc)
	if !ports.Has("tcp-live") {
		t.Fatalf("expected validated port to be retained")
	}
	if ports.Has("tcp-stale") {
		t.Fatalf("expected stale port to be filtered out")
	}
}

func testService(namespace, name, portName, clusterIP string, port, nodePort int32) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: v1.ServiceSpec{
			ClusterIP: clusterIP,
			Ports: []v1.ServicePort{{
				Name:     portName,
				Protocol: v1.ProtocolTCP,
				Port:     port,
				NodePort: nodePort,
			}},
		},
	}
}

type fakeLoadBalancer struct{}

func (f *fakeLoadBalancer) NextEndpoint(proxy.ServicePortName, net.Addr, bool) (string, error) {
	return "", nil
}

func (f *fakeLoadBalancer) NewService(proxy.ServicePortName, v1.ServiceAffinity, int) error {
	return nil
}

func (f *fakeLoadBalancer) DeleteService(proxy.ServicePortName) {}

func (f *fakeLoadBalancer) CleanupStaleStickySessions(proxy.ServicePortName) {}

func (f *fakeLoadBalancer) ServiceHasEndpoints(proxy.ServicePortName) bool {
	return true
}

func (f *fakeLoadBalancer) OnEndpointsAdd(*v1.Endpoints) {}

func (f *fakeLoadBalancer) OnEndpointsUpdate(*v1.Endpoints, *v1.Endpoints) {}

func (f *fakeLoadBalancer) OnEndpointsDelete(*v1.Endpoints) {}

func (f *fakeLoadBalancer) OnEndpointsSynced() {}

var _ proxyconfig.EndpointsHandler = (*fakeLoadBalancer)(nil)

type fakeProxySocket struct {
	addr   net.Addr
	port   int
	closed atomic.Bool
}

func newFakeProxySocket(_ v1.Protocol, ip net.IP, port int) (ProxySocket, error) {
	if ip == nil {
		ip = net.IPv4zero
	}
	return &fakeProxySocket{
		addr: &net.TCPAddr{IP: ip, Port: port},
		port: port,
	}, nil
}

func (f *fakeProxySocket) Addr() net.Addr {
	return f.addr
}

func (f *fakeProxySocket) Close() error {
	f.closed.Store(true)
	return nil
}

func (f *fakeProxySocket) ProxyLoop(proxy.ServicePortName, *ServiceInfo, LoadBalancer) {}

func (f *fakeProxySocket) ListenPort() int {
	return f.port
}

type fakeIPTables struct {
	mu     sync.Mutex
	chains map[iptables.Table]map[iptables.Chain]struct{}
	rules  map[iptables.Table]map[iptables.Chain]map[string]struct{}
}

func newFakeIPTables() *fakeIPTables {
	return &fakeIPTables{
		chains: map[iptables.Table]map[iptables.Chain]struct{}{},
		rules:  map[iptables.Table]map[iptables.Chain]map[string]struct{}{},
	}
}

func (f *fakeIPTables) EnsureChain(table iptables.Table, chain iptables.Chain) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.chains[table]; !ok {
		f.chains[table] = map[iptables.Chain]struct{}{}
	}
	_, existed := f.chains[table][chain]
	f.chains[table][chain] = struct{}{}
	if _, ok := f.rules[table]; !ok {
		f.rules[table] = map[iptables.Chain]map[string]struct{}{}
	}
	if _, ok := f.rules[table][chain]; !ok {
		f.rules[table][chain] = map[string]struct{}{}
	}
	return existed, nil
}

func (f *fakeIPTables) FlushChain(table iptables.Table, chain iptables.Chain) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.rules[table]; !ok {
		return nil
	}
	f.rules[table][chain] = map[string]struct{}{}
	return nil
}

func (f *fakeIPTables) DeleteChain(table iptables.Table, chain iptables.Chain) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.chains[table]; ok {
		delete(f.chains[table], chain)
	}
	if _, ok := f.rules[table]; ok {
		delete(f.rules[table], chain)
	}
	return nil
}

func (f *fakeIPTables) ChainExists(table iptables.Table, chain iptables.Chain) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.chains[table][chain]
	return ok, nil
}

func (f *fakeIPTables) EnsureRule(_ iptables.RulePosition, table iptables.Table, chain iptables.Chain, args ...string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.rules[table]; !ok {
		f.rules[table] = map[iptables.Chain]map[string]struct{}{}
	}
	if _, ok := f.rules[table][chain]; !ok {
		f.rules[table][chain] = map[string]struct{}{}
	}
	key := ruleKey(args)
	_, existed := f.rules[table][chain][key]
	f.rules[table][chain][key] = struct{}{}
	return existed, nil
}

func (f *fakeIPTables) DeleteRule(table iptables.Table, chain iptables.Chain, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.rules[table]; !ok {
		return nil
	}
	delete(f.rules[table][chain], ruleKey(args))
	return nil
}

func (f *fakeIPTables) IsIPv6() bool {
	return false
}

func (f *fakeIPTables) Protocol() iptables.Protocol {
	return iptables.ProtocolIPv4
}

func (f *fakeIPTables) SaveInto(iptables.Table, *bytes.Buffer) error {
	return nil
}

func (f *fakeIPTables) Restore(iptables.Table, []byte, iptables.FlushFlag, iptables.RestoreCountersFlag) error {
	return nil
}

func (f *fakeIPTables) RestoreAll([]byte, iptables.FlushFlag, iptables.RestoreCountersFlag) error {
	return nil
}

func (f *fakeIPTables) Monitor(iptables.Chain, []iptables.Table, func(), time.Duration, <-chan struct{}) {
}

func (f *fakeIPTables) HasRandomFully() bool {
	return false
}

func (f *fakeIPTables) Present() bool {
	return true
}

func ruleKey(args []string) string {
	return strings.Join(args, "\x00")
}
