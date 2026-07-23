package meshstate

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/proxy"
)

// Store is the in-memory projection of the primary MetaServer-backed Service
// and Endpoints informers. Writers serialize validation and publication; data
// plane readers consume immutable snapshots without Kubernetes API calls.
type Store struct {
	mu sync.RWMutex

	services      map[string]*v1.Service
	endpoints     map[string]*v1.Endpoints
	managed       map[string]bool
	retiredUIDs   map[types.UID]struct{}
	portalApplied map[string]bool   // keyed by Service UID and named port
	verified      map[string]string // namespaced Service key -> current Service UID

	managedHostsPath string
	projectionError  string
	snapshot         atomic.Value // *Snapshot
}

func NewStore() *Store {
	s, _ := newStore("")
	return s
}

// NewStoreWithManagedHosts creates a managed-route store that atomically
// projects every APPLIED route into a CoreDNS hosts file. The proxy route and
// DNS record therefore share one validation/source-of-truth boundary.
func NewStoreWithManagedHosts(path string) (*Store, error) {
	return newStore(path)
}

func newStore(managedHostsPath string) (*Store, error) {
	s := &Store{
		services:         make(map[string]*v1.Service),
		endpoints:        make(map[string]*v1.Endpoints),
		managed:          make(map[string]bool),
		retiredUIDs:      make(map[types.UID]struct{}),
		portalApplied:    make(map[string]bool),
		verified:         make(map[string]string),
		managedHostsPath: managedHostsPath,
	}
	initial := &Snapshot{
		SourceState:   SourceSyncing,
		SourceMessage: "waiting for Service and Endpoints informer caches to sync",
		UpdatedAt:     time.Now().UTC(),
		Routes:        make(map[string]Route),
		servicePorts:  make(map[string]string),
		serviceUIDs:   make(map[string]string),
	}
	if err := writeManagedHostsFile(managedHostsPath, initial); err != nil {
		return nil, fmt.Errorf("initialize managed runtime DNS projection: %w", err)
	}
	s.snapshot.Store(initial)
	return s, nil
}

func (s *Store) Snapshot() *Snapshot {
	return cloneSnapshot(s.snapshot.Load().(*Snapshot))
}

func (s *Store) snapshotLocked() *Snapshot {
	return cloneSnapshot(s.snapshot.Load().(*Snapshot))
}

func cloneSnapshot(in *Snapshot) *Snapshot {
	out := *in
	out.Routes = make(map[string]Route, len(in.Routes))
	for key, route := range in.Routes {
		out.Routes[key] = route
	}
	out.servicePorts = make(map[string]string, len(in.servicePorts))
	for key, runtimeUID := range in.servicePorts {
		out.servicePorts[key] = runtimeUID
	}
	out.serviceUIDs = make(map[string]string, len(in.serviceUIDs))
	for key, runtimeUID := range in.serviceUIDs {
		out.serviceUIDs[key] = runtimeUID
	}
	return &out
}

func (s *Store) UpsertService(service *v1.Service) error {
	if service == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := namespacedKey(service.Namespace, service.Name)
	if _, retired := s.retiredUIDs[service.UID]; retired {
		klog.V(4).InfoS("Ignore retired managed Service incarnation",
			"service", key, "uid", service.UID)
		return nil
	}
	current := s.services[key]
	if current != nil && isOlderNumericResourceVersion(current.ResourceVersion, service.ResourceVersion) {
		klog.InfoS("Ignore stale managed Service projection event",
			"service", key,
			"currentResourceVersion", current.ResourceVersion,
			"incomingResourceVersion", service.ResourceVersion)
		return nil
	}
	if service.Labels[LabelMeshManaged] != "true" {
		// An informer Update to a non-managed current object supersedes any
		// previously observed managed incarnation at this namespaced key.
		if current != nil {
			return s.retireServiceLocked(current, "Service replaced or mesh-managed label removed")
		}
		return nil
	}
	if current != nil && current.UID != service.UID {
		if err := s.retireServiceLocked(current, "Service UID replaced"); err != nil {
			return err
		}
	}
	s.services[key] = service.DeepCopy()
	s.managed[key] = true
	return s.reconcileLocked(key)
}

func (s *Store) UpsertEndpoints(endpoints *v1.Endpoints) error {
	if endpoints == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := namespacedKey(endpoints.Namespace, endpoints.Name)
	current := s.endpoints[key]
	if current != nil && isOlderNumericResourceVersion(current.ResourceVersion, endpoints.ResourceVersion) {
		klog.InfoS("Ignore stale managed Endpoints projection event",
			"endpoints", key,
			"currentResourceVersion", current.ResourceVersion,
			"incomingResourceVersion", endpoints.ResourceVersion)
		return nil
	}
	if endpoints.Labels[LabelMeshManaged] != "true" {
		if !s.managed[key] {
			return nil
		}
		delete(s.endpoints, key)
		delete(s.verified, key)
		return s.markDegradedLocked(key, "mesh-managed label removed from Endpoints")
	}
	s.endpoints[key] = endpoints.DeepCopy()
	return s.reconcileLocked(key)
}

func (s *Store) DeleteService(service *v1.Service) error {
	if service == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retiredUIDs[service.UID] = struct{}{}
	return s.retireServiceLocked(service, "managed Service deleted")
}

// RetireService removes the locally cached incarnation for a namespaced name
// after an authoritative API lookup confirms that the Service no longer
// exists. Edge MetaManager caches can miss delete events, so informer state
// alone is not sufficient to retain a managed route indefinitely.
func (s *Store) RetireService(namespace, name, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	service := s.services[namespacedKey(namespace, name)]
	if service == nil {
		return nil
	}
	s.retiredUIDs[service.UID] = struct{}{}
	return s.retireServiceLocked(service, reason)
}

func (s *Store) retireServiceLocked(service *v1.Service, reason string) error {
	key := namespacedKey(service.Namespace, service.Name)
	current := s.services[key]
	knownIncarnation := current != nil && current.UID == service.UID && s.managed[key]
	next := s.snapshotLocked()
	for _, route := range next.Routes {
		if route.Key() == key && route.ServiceUID == service.UID {
			knownIncarnation = true
			break
		}
	}
	if !knownIncarnation {
		return nil
	}

	if current != nil && current.UID == service.UID {
		delete(s.services, key)
		delete(s.verified, key)
		if endpoints := s.endpoints[key]; endpoints != nil && identityMatches(service.Labels, endpoints.Labels) {
			delete(s.endpoints, key)
		}
	}
	changed := false
	for runtimeUID, route := range next.Routes {
		if route.Key() == key && route.ServiceUID == service.UID {
			delete(next.Routes, runtimeUID)
			changed = true
		}
	}
	if changed {
		s.publishLocked(next)
		klog.InfoS("Retired managed Service incarnation", "service", key, "uid", service.UID, "reason", reason)
	}
	if !s.hasPortalLocked(service.UID) {
		delete(s.managed, key)
	}
	return nil
}

// DeleteEndpoints preserves the last validated endpoint only as diagnostics;
// the route is immediately DEGRADED and cannot be selected by the data plane.
func (s *Store) DeleteEndpoints(endpoints *v1.Endpoints) error {
	if endpoints == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := namespacedKey(endpoints.Namespace, endpoints.Name)
	current := s.endpoints[key]
	if current == nil || current.UID != endpoints.UID {
		return nil
	}
	delete(s.endpoints, key)
	delete(s.verified, key)
	return s.markDegradedLocked(key, "managed Endpoints temporarily unavailable")
}

func (s *Store) MarkSourceSynced() {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.snapshotLocked()
	next.SourceState = SourceSynced
	next.SourceMessage = "Service and Endpoints informer caches synced"
	for runtimeUID, route := range next.Routes {
		if s.routeAppliedLocked(route) {
			route.ProxyApplied = true
			route.Phase = PhaseApplied
			route.Reason = ""
		} else if s.verified[route.Key()] == string(route.ServiceUID) {
			route.ProxyApplied = false
			route.Phase = PhaseReady
			route.Reason = "waiting for userspace portal"
		}
		route.UpdatedAt = time.Now().UTC()
		next.Routes[runtimeUID] = route
	}
	s.publishLocked(next)
}

func (s *Store) MarkSourceDegraded(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.snapshotLocked()
	next.SourceState = SourceDegraded
	next.SourceMessage = reason
	for runtimeUID, route := range next.Routes {
		route.Phase = PhaseDegraded
		route.Reason = reason
		route.UpdatedAt = time.Now().UTC()
		next.Routes[runtimeUID] = route
	}
	s.publishLocked(next)
}

func (s *Store) IsManagedServicePort(name proxy.ServicePortName) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.managed[namespacedKey(name.NamespacedName.Namespace, name.NamespacedName.Name)]
}

// ManagedEndpoint distinguishes an invalid managed route from a legacy route.
// managed=true, applied=false must fail closed and never enter legacy/random LB.
func (s *Store) ManagedEndpoint(name proxy.ServicePortName) (endpoint string, managed, applied bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := namespacedKey(name.NamespacedName.Namespace, name.NamespacedName.Name)
	if !s.managed[key] {
		return "", false, false
	}
	snapshot := s.snapshot.Load().(*Snapshot)
	runtimeUID, exists := snapshot.servicePorts[servicePortKey(name)]
	if !exists {
		return "", true, false
	}
	route, exists := snapshot.Routes[runtimeUID]
	if !exists {
		return "", true, false
	}
	return route.Endpoint(), true, snapshot.SourceState == SourceSynced && route.Phase == PhaseApplied
}

func (s *Store) repairCandidates() []types.NamespacedName {
	s.mu.RLock()
	defer s.mu.RUnlock()
	projectionFailed := s.projectionError != ""
	if !projectionFailed && s.snapshot.Load().(*Snapshot).SourceState != SourceSynced {
		return nil
	}

	candidates := make([]types.NamespacedName, 0)
	seen := make(map[string]bool)
	snapshot := s.snapshot.Load().(*Snapshot)
	for _, route := range snapshot.Routes {
		key := route.Key()
		if seen[key] || !s.managed[key] {
			continue
		}
		// APPLIED routes are already complete. READY and DEGRADED routes are
		// revalidated by point lookup: this repairs missed informer events and
		// retires stale Service incarnations left behind by a missed delete.
		if !projectionFailed && route.Phase == PhaseApplied {
			continue
		}
		// A confirmed delete must stay fail-closed; repair only objects that
		// are still present but currently violate the managed route contract.
		if s.services[key] == nil {
			continue
		}
		namespace, name, found := strings.Cut(key, "/")
		if !found || namespace == "" || name == "" {
			continue
		}
		candidates = append(candidates, types.NamespacedName{Namespace: namespace, Name: name})
		seen[key] = true
	}
	// The userspace proxier and the mesh-state informer consume independent
	// event streams.  A portal can therefore already exist while the managed
	// Service Add event was lost completely.  Such a service has no Route to
	// iterate above, so recover it from MetaServer by namespaced name.  A
	// confirmed Endpoints deletion retains a degraded Route and is deliberately
	// handled by the loop above, which still requires both cached objects; it
	// therefore remains fail-closed.
	for key, managed := range s.managed {
		if !managed || seen[key] {
			continue
		}
		hasRoute := false
		for _, route := range snapshot.Routes {
			if route.Key() == key {
				hasRoute = true
				break
			}
		}
		if hasRoute {
			continue
		}
		namespace, name, found := strings.Cut(key, "/")
		if !found || namespace == "" || name == "" {
			continue
		}
		candidates = append(candidates, types.NamespacedName{Namespace: namespace, Name: name})
		seen[key] = true
	}
	return candidates
}

func (s *Store) projectionVerified(name types.NamespacedName) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.verified[namespacedKey(name.Namespace, name.Name)] != ""
}

// MarkPortalPending is called before a managed userspace portal is opened.
// Recording the identity first makes the data path fail closed even when the
// proxier handler runs before the meshstate informer handler.
func (s *Store) MarkPortalPending(serviceUID types.UID, name proxy.ServicePortName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := namespacedKey(name.NamespacedName.Namespace, name.NamespacedName.Name)
	s.managed[key] = true
	s.portalApplied[portalKey(serviceUID, name.Port)] = false

	next := s.snapshotLocked()
	changed := false
	for runtimeUID, route := range next.Routes {
		if route.ServiceUID != serviceUID || route.Namespace != name.NamespacedName.Namespace || route.ServiceName != name.NamespacedName.Name || route.PortName != name.Port {
			continue
		}
		route.ProxyApplied = false
		if s.verified[route.Key()] == string(serviceUID) && next.SourceState == SourceSynced {
			route.Phase = PhaseReady
			route.Reason = "waiting for userspace portal"
		}
		route.UpdatedAt = time.Now().UTC()
		next.Routes[runtimeUID] = route
		changed = true
	}
	if changed {
		s.publishLocked(next)
	}
}

// MarkPortalApplied records the terminal portal transition. applied=true is
// emitted only after both the userspace portal and load-balancer entry exist;
// applied=false is emitted before an existing portal is removed.
func (s *Store) MarkPortalApplied(serviceUID types.UID, name proxy.ServicePortName, applied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	portalKey := portalKey(serviceUID, name.Port)
	s.portalApplied[portalKey] = applied
	key := namespacedKey(name.NamespacedName.Namespace, name.NamespacedName.Name)
	if applied {
		// A proxier event can run before the meshstate informer handler. Keep
		// the key fail-closed until the matching projection is validated.
		s.managed[key] = true
	}
	next := s.snapshotLocked()
	changed := false
	for runtimeUID, route := range next.Routes {
		if route.ServiceUID != serviceUID || route.Namespace != name.NamespacedName.Namespace || route.ServiceName != name.NamespacedName.Name || route.PortName != name.Port {
			continue
		}
		verified := s.verified[route.Key()] == string(serviceUID)
		route.ProxyApplied = applied
		if applied && next.SourceState == SourceSynced && verified {
			route.Phase = PhaseApplied
			route.Reason = ""
		} else if verified && next.SourceState == SourceSynced {
			route.Phase = PhaseReady
			route.Reason = "waiting for userspace portal"
		}
		route.UpdatedAt = time.Now().UTC()
		next.Routes[runtimeUID] = route
		changed = true
	}
	if changed {
		s.publishLocked(next)
	}
	// applied=false is REMOVING, not REMOVED. Keep the managed tombstone
	// until the proxier confirms portal, socket and legacy LB state are gone.
}

// MarkPortalRemoved completes teardown after the userspace portal, proxy
// socket and legacy load-balancer entry have all been removed.
func (s *Store) MarkPortalRemoved(serviceUID types.UID, name proxy.ServicePortName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.portalApplied, portalKey(serviceUID, name.Port))
	key := namespacedKey(name.NamespacedName.Namespace, name.NamespacedName.Name)
	if !s.hasCurrentManagedIncarnationLocked(key, serviceUID) && !s.hasPortalLocked(serviceUID) {
		delete(s.managed, key)
	}
}

func (s *Store) reconcileLocked(key string) error {
	delete(s.verified, key)
	service := s.services[key]
	endpoints := s.endpoints[key]
	next := s.snapshotLocked()
	route, err := validateRoute(service, endpoints)
	if err != nil {
		if service != nil && service.Labels[LabelMeshManaged] == "true" {
			s.publishDegradedServiceLocked(next, service, err.Error())
			return nil
		}
		return s.markDegradedSnapshotLocked(next, key, err.Error())
	}
	if existing, exists := next.Routes[route.RuntimeServiceUID]; exists && existing.ServiceUID != route.ServiceUID {
		reason := fmt.Sprintf("runtime-service-uid already belongs to Service UID %s", existing.ServiceUID)
		s.publishDegradedServiceLocked(next, service, reason)
		return nil
	}
	for runtimeUID, existing := range next.Routes {
		if existing.Key() == key && runtimeUID != route.RuntimeServiceUID {
			delete(next.Routes, runtimeUID)
		}
	}
	s.verified[key] = string(route.ServiceUID)
	route.ProxyApplied = s.portalApplied[portalKey(route.ServiceUID, route.PortName)]
	route.Phase = PhaseReady
	route.Reason = "waiting for userspace portal"
	route.UpdatedAt = time.Now().UTC()
	if route.ProxyApplied && next.SourceState == SourceSynced {
		route.Phase = PhaseApplied
		route.Reason = ""
	}
	next.Routes[route.RuntimeServiceUID] = route
	s.publishLocked(next)
	return nil
}

func (s *Store) publishDegradedServiceLocked(next *Snapshot, service *v1.Service, reason string) {
	key := namespacedKey(service.Namespace, service.Name)
	for runtimeUID, route := range next.Routes {
		if route.Key() == key {
			delete(next.Routes, runtimeUID)
		}
	}
	revision, _ := strconv.ParseInt(service.Labels[LabelDeploymentRevision], 10, 64)
	route := Route{
		RuntimeServiceUID:      service.Labels[LabelRuntimeServiceUID],
		ServiceUID:             service.UID,
		ServiceResourceVersion: service.ResourceVersion,
		Namespace:              service.Namespace,
		ServiceName:            service.Name,
		LogicalService:         service.Annotations[AnnotationLogicalService],
		InstallID:              service.Labels[LabelInstallID],
		DeploymentRevision:     revision,
		RuntimeID:              service.Labels[LabelRuntimeID],
		Component:              service.Labels[LabelComponent],
		TargetNode:             service.Annotations[AnnotationTargetNode],
		FQDN:                   fqdn(service.Name, service.Namespace),
		Phase:                  PhaseDegraded,
		Reason:                 reason,
		UpdatedAt:              time.Now().UTC(),
	}
	if endpoints := s.endpoints[key]; endpoints != nil {
		route.EndpointsResourceVersion = endpoints.ResourceVersion
	}
	if len(service.Spec.Ports) == 1 {
		route.PortName = service.Spec.Ports[0].Name
		route.ServicePort = service.Spec.Ports[0].Port
		route.Protocol = service.Spec.Ports[0].Protocol
	}
	runtimeKey := route.RuntimeServiceUID
	if runtimeKey == "" {
		runtimeKey = "invalid/" + string(service.UID)
	} else if existing, collision := next.Routes[runtimeKey]; collision && existing.ServiceUID != service.UID {
		runtimeKey = "invalid/" + string(service.UID)
	}
	next.Routes[runtimeKey] = route
	s.publishLocked(next)
}

func (s *Store) routeAppliedLocked(route Route) bool {
	return s.verified[route.Key()] == string(route.ServiceUID) && s.portalApplied[portalKey(route.ServiceUID, route.PortName)]
}

func (s *Store) markDegradedLocked(key, reason string) error {
	return s.markDegradedSnapshotLocked(s.snapshotLocked(), key, reason)
}

func (s *Store) markDegradedSnapshotLocked(next *Snapshot, key, reason string) error {
	changed := false
	for runtimeUID, route := range next.Routes {
		if route.Key() != key {
			continue
		}
		route.Phase = PhaseDegraded
		route.Reason = reason
		route.UpdatedAt = time.Now().UTC()
		next.Routes[runtimeUID] = route
		changed = true
	}
	if changed {
		s.publishLocked(next)
	}
	return nil
}

func validateRoute(service *v1.Service, endpoints *v1.Endpoints) (Route, error) {
	if service == nil {
		return Route{}, fmt.Errorf("managed Service unavailable")
	}
	if endpoints == nil {
		return Route{}, fmt.Errorf("managed Endpoints unavailable")
	}
	for _, key := range identityLabels {
		if service.Labels[key] == "" {
			return Route{}, fmt.Errorf("Service missing identity label %s", key)
		}
		if endpoints.Labels[key] != service.Labels[key] {
			return Route{}, fmt.Errorf("Endpoints identity label %s does not match Service", key)
		}
	}
	if service.Labels[LabelMeshManaged] != "true" {
		return Route{}, fmt.Errorf("Service is not mesh managed")
	}
	if service.UID == "" {
		return Route{}, fmt.Errorf("managed Service UID is required")
	}
	if service.Labels[LabelRuntimeID] != service.Name {
		return Route{}, fmt.Errorf("runtime-id must equal the managed Service name")
	}
	targetNode := service.Annotations[AnnotationTargetNode]
	if targetNode == "" {
		return Route{}, fmt.Errorf("Service missing target-node annotation")
	}
	if service.Spec.Type != v1.ServiceTypeClusterIP || service.Spec.ClusterIP == "" || service.Spec.ClusterIP == v1.ClusterIPNone || net.ParseIP(service.Spec.ClusterIP) == nil {
		return Route{}, fmt.Errorf("managed Service must have a valid ClusterIP")
	}
	if len(service.Spec.Ports) != 1 {
		return Route{}, fmt.Errorf("managed Service must expose exactly one port")
	}
	servicePort := service.Spec.Ports[0]
	if servicePort.Name == "" || servicePort.Protocol != v1.ProtocolTCP || servicePort.NodePort != 0 {
		return Route{}, fmt.Errorf("managed Service requires one named TCP port and nodePort=0")
	}
	if servicePort.TargetPort.Type != intstr.Int || servicePort.TargetPort.IntVal <= 0 {
		return Route{}, fmt.Errorf("managed Service targetPort must be numeric")
	}
	if servicePort.Port != servicePort.TargetPort.IntVal {
		return Route{}, fmt.Errorf("managed Service port must equal numeric targetPort")
	}
	if len(endpoints.Subsets) != 1 || len(endpoints.Subsets[0].Ports) != 1 || len(endpoints.Subsets[0].Addresses) != 1 || len(endpoints.Subsets[0].NotReadyAddresses) != 0 {
		return Route{}, fmt.Errorf("managed Endpoints requires exactly one subset, one port, one ready address and no not-ready addresses")
	}
	subset := endpoints.Subsets[0]
	address := subset.Addresses[0]
	endpointPort := subset.Ports[0]
	if endpointPort.Name != servicePort.Name || endpointPort.Protocol != v1.ProtocolTCP || endpointPort.Port != servicePort.TargetPort.IntVal {
		return Route{}, fmt.Errorf("managed endpoint port does not match Service targetPort")
	}
	if net.ParseIP(address.IP) == nil || address.NodeName == nil || *address.NodeName != targetNode {
		return Route{}, fmt.Errorf("managed endpoint IP/node does not match target-node")
	}
	if address.TargetRef == nil || address.TargetRef.Kind != "Pod" || address.TargetRef.Name == "" || address.TargetRef.UID == "" ||
		(address.TargetRef.Namespace != "" && address.TargetRef.Namespace != service.Namespace) {
		return Route{}, fmt.Errorf("managed endpoint must reference one Pod UID in the Service namespace")
	}
	revision, err := strconv.ParseInt(service.Labels[LabelDeploymentRevision], 10, 64)
	if err != nil || revision < 1 {
		return Route{}, fmt.Errorf("deployment-revision must be a positive base-10 int64")
	}
	return Route{
		RuntimeServiceUID:        service.Labels[LabelRuntimeServiceUID],
		ServiceUID:               service.UID,
		ServiceResourceVersion:   service.ResourceVersion,
		EndpointsResourceVersion: endpoints.ResourceVersion,
		Namespace:                service.Namespace,
		ServiceName:              service.Name,
		LogicalService:           service.Annotations[AnnotationLogicalService],
		InstallID:                service.Labels[LabelInstallID],
		DeploymentRevision:       revision,
		RuntimeID:                service.Labels[LabelRuntimeID],
		Component:                service.Labels[LabelComponent],
		TargetNode:               targetNode,
		FQDN:                     fqdn(service.Name, service.Namespace),
		PortName:                 servicePort.Name,
		ServiceIP:                service.Spec.ClusterIP,
		ServicePort:              servicePort.Port,
		EndpointIP:               address.IP,
		EndpointPort:             endpointPort.Port,
		EndpointPod:              address.TargetRef.Name,
		EndpointPodUID:           address.TargetRef.UID,
		Protocol:                 v1.ProtocolTCP,
	}, nil
}

func isOlderNumericResourceVersion(current, incoming string) bool {
	current, currentOK := canonicalNumericResourceVersion(current)
	incoming, incomingOK := canonicalNumericResourceVersion(incoming)
	if !currentOK || !incomingOK {
		return false
	}
	if len(current) != len(incoming) {
		return len(incoming) < len(current)
	}
	return incoming < current
}

func canonicalNumericResourceVersion(version string) (string, bool) {
	if version == "" || version[0] < '1' || version[0] > '9' {
		return "", false
	}
	for i := 1; i < len(version); i++ {
		if version[i] < '0' || version[i] > '9' {
			return "", false
		}
	}
	return version, true
}

func portalKey(serviceUID types.UID, port string) string {
	return string(serviceUID) + ":" + port
}

func (s *Store) hasPortalLocked(serviceUID types.UID) bool {
	prefix := string(serviceUID) + ":"
	for key := range s.portalApplied {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func (s *Store) hasCurrentManagedIncarnationLocked(key string, serviceUID types.UID) bool {
	service := s.services[key]
	if service != nil && service.UID != serviceUID && s.managed[key] {
		return true
	}
	for _, route := range s.snapshot.Load().(*Snapshot).Routes {
		if route.Key() == key && route.ServiceUID != serviceUID {
			return true
		}
	}
	return service != nil && service.UID == serviceUID && service.Labels[LabelMeshManaged] == "true"
}

func identityMatches(serviceLabels, endpointsLabels map[string]string) bool {
	for _, key := range identityLabels {
		if serviceLabels[key] == "" || serviceLabels[key] != endpointsLabels[key] {
			return false
		}
	}
	return true
}

func (s *Store) publishLocked(next *Snapshot) {
	next.servicePorts = make(map[string]string, len(next.Routes))
	next.serviceUIDs = make(map[string]string, len(next.Routes))
	for runtimeUID, route := range next.Routes {
		next.servicePorts[servicePortKey(route.ServicePortName())] = runtimeUID
		next.serviceUIDs[string(route.ServiceUID)] = runtimeUID
	}
	next.Sequence++
	next.UpdatedAt = time.Now().UTC()
	if s.projectionError != "" {
		next.SourceState = SourceSynced
		next.SourceMessage = "Service, Endpoints and managed DNS projection synced"
		for runtimeUID, route := range next.Routes {
			if s.routeAppliedLocked(route) {
				route.ProxyApplied = true
				route.Phase = PhaseApplied
				route.Reason = ""
			} else if s.verified[route.Key()] == string(route.ServiceUID) {
				route.ProxyApplied = false
				route.Phase = PhaseReady
				route.Reason = "waiting for userspace portal"
			}
			next.Routes[runtimeUID] = route
		}
	}
	if err := writeManagedHostsFile(s.managedHostsPath, next); err != nil {
		s.projectionError = err.Error()
		failed := cloneSnapshot(next)
		failed.SourceState = SourceDegraded
		failed.SourceMessage = "managed runtime DNS projection failed: " + err.Error()
		s.snapshot.Store(failed)
		klog.ErrorS(err, "Publish managed runtime DNS projection",
			"path", s.managedHostsPath, "sequence", next.Sequence)
		return
	}
	s.projectionError = ""
	s.snapshot.Store(next)
}

func writeManagedHostsFile(path string, snapshot *Snapshot) error {
	if path == "" {
		return nil
	}
	if snapshot == nil {
		return fmt.Errorf("managed route snapshot is nil")
	}

	lines := make([]string, 0, len(snapshot.Routes))
	if snapshot.SourceState == SourceSynced {
		for _, route := range snapshot.Routes {
			if route.Phase != PhaseApplied || net.ParseIP(route.ServiceIP) == nil || route.FQDN == "" {
				continue
			}
			lines = append(lines, route.ServiceIP+" "+route.FQDN)
		}
	}
	sort.Strings(lines)

	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tempPath)
		}
	}()

	if err := file.Chmod(0644); err != nil {
		_ = file.Close()
		return err
	}
	writer := bufio.NewWriter(file)
	for _, line := range lines {
		if _, err := writer.WriteString(line + "\n"); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	renamed = true
	return nil
}
