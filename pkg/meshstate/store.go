package meshstate

import (
	"fmt"
	"net"
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
	portalApplied map[string]bool   // keyed by Service UID and named port
	verified      map[string]string // namespaced Service key -> current Service UID

	snapshot atomic.Value // *Snapshot
}

func NewStore() *Store {
	s := &Store{
		services:      make(map[string]*v1.Service),
		endpoints:     make(map[string]*v1.Endpoints),
		managed:       make(map[string]bool),
		portalApplied: make(map[string]bool),
		verified:      make(map[string]string),
	}
	s.snapshot.Store(&Snapshot{
		SourceState:   SourceSyncing,
		SourceMessage: "waiting for Service and Endpoints informer caches to sync",
		UpdatedAt:     time.Now().UTC(),
		Routes:        make(map[string]Route),
		servicePorts:  make(map[string]string),
		serviceUIDs:   make(map[string]string),
	})
	return s
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
	if service.Labels[LabelMeshManaged] != "true" {
		// An informer Update to a non-managed current object supersedes any
		// previously observed managed incarnation at this namespaced key.
		if current := s.services[key]; current != nil {
			return s.retireServiceLocked(current, "Service replaced or mesh-managed label removed")
		}
		return nil
	}
	if current := s.services[key]; current != nil && current.UID != service.UID {
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
	return s.retireServiceLocked(service, "managed Service deleted")
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
		RuntimeServiceUID:  service.Labels[LabelRuntimeServiceUID],
		ServiceUID:         service.UID,
		Namespace:          service.Namespace,
		ServiceName:        service.Name,
		LogicalService:     service.Annotations[AnnotationLogicalService],
		InstallID:          service.Labels[LabelInstallID],
		DeploymentRevision: revision,
		RuntimeID:          service.Labels[LabelRuntimeID],
		Component:          service.Labels[LabelComponent],
		TargetNode:         service.Annotations[AnnotationTargetNode],
		FQDN:               fqdn(service.Name, service.Namespace),
		Phase:              PhaseDegraded,
		Reason:             reason,
		UpdatedAt:          time.Now().UTC(),
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
		RuntimeServiceUID:  service.Labels[LabelRuntimeServiceUID],
		ServiceUID:         service.UID,
		Namespace:          service.Namespace,
		ServiceName:        service.Name,
		LogicalService:     service.Annotations[AnnotationLogicalService],
		InstallID:          service.Labels[LabelInstallID],
		DeploymentRevision: revision,
		RuntimeID:          service.Labels[LabelRuntimeID],
		Component:          service.Labels[LabelComponent],
		TargetNode:         targetNode,
		FQDN:               fqdn(service.Name, service.Namespace),
		PortName:           servicePort.Name,
		ServiceIP:          service.Spec.ClusterIP,
		ServicePort:        servicePort.Port,
		EndpointIP:         address.IP,
		EndpointPort:       endpointPort.Port,
		EndpointPod:        address.TargetRef.Name,
		EndpointPodUID:     address.TargetRef.UID,
		Protocol:           v1.ProtocolTCP,
	}, nil
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
	s.snapshot.Store(next)
}
