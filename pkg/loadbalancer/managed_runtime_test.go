package loadbalancer

import (
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/pkg/proxy"

	"github.com/kubeedge/edgemesh/pkg/meshstate"
)

func TestParseManagedEndpointSupportsIPv4AndIPv6(t *testing.T) {
	tests := []struct {
		name              string
		endpoint          string
		wantNode, wantPod string
		wantIP, wantPort  string
		wantOK            bool
	}{
		{
			name:     "ipv4",
			endpoint: "node-a:pod-a:10.244.0.10:8080",
			wantNode: "node-a", wantPod: "pod-a", wantIP: "10.244.0.10", wantPort: "8080", wantOK: true,
		},
		{
			name:     "ipv6",
			endpoint: "node-a:pod-a:[fd00::10]:8080",
			wantNode: "node-a", wantPod: "pod-a", wantIP: "fd00::10", wantPort: "8080", wantOK: true,
		},
		{
			name:     "legacy endpoint without pod identity",
			endpoint: "node-a::10.244.0.10:8080",
			wantNode: "node-a", wantPod: "", wantIP: "10.244.0.10", wantPort: "8080", wantOK: true,
		},
		{
			name:     "legacy endpoint without node identity",
			endpoint: ":pod-a:10.244.0.10:8080",
			wantNode: "", wantPod: "pod-a", wantIP: "10.244.0.10", wantPort: "8080", wantOK: true,
		},
		{name: "unbracketed ipv6", endpoint: "node-a:pod-a:fd00::10:8080"},
		{name: "invalid ip", endpoint: "node-a:pod-a:not-an-ip:8080"},
		{name: "invalid port", endpoint: "node-a:pod-a:10.244.0.10:70000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, pod, ip, port, ok := parseEndpoint(tt.endpoint)
			if ok != tt.wantOK || node != tt.wantNode || pod != tt.wantPod || ip != tt.wantIP || port != tt.wantPort {
				t.Fatalf("parseEndpoint(%q) = (%q,%q,%q,%q,%v), want (%q,%q,%q,%q,%v)", tt.endpoint, node, pod, ip, port, ok, tt.wantNode, tt.wantPod, tt.wantIP, tt.wantPort, tt.wantOK)
			}
		})
	}
}

func TestManagedRoutePrecedesAndBlocksLegacySelection(t *testing.T) {
	store := meshstate.NewStore()
	labels := map[string]string{
		meshstate.LabelMeshManaged: "true", meshstate.LabelInstallID: "dayu",
		meshstate.LabelDeploymentRevision: "1", meshstate.LabelRuntimeID: "runtime-a",
		meshstate.LabelComponent: "processor", meshstate.LabelRuntimeServiceUID: "runtime-uid",
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "runtime-a", UID: "service-uid", Labels: labels,
			Annotations: map[string]string{meshstate.AnnotationTargetNode: "edge-a"},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeClusterIP, ClusterIP: "10.96.0.42", Ports: []v1.ServicePort{{
			Name: "runtime", Protocol: v1.ProtocolTCP, Port: 8080, TargetPort: intstr.FromInt(8080),
		}}},
	}
	node := "edge-a"
	endpoints := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Namespace: service.Namespace, Name: service.Name, UID: "endpoints-uid", Labels: labels},
		Subsets: []v1.EndpointSubset{{
			Addresses: []v1.EndpointAddress{{IP: "10.244.0.8", NodeName: &node, TargetRef: &v1.ObjectReference{Kind: "Pod", Name: "runtime-pod", UID: "pod-uid"}}},
			Ports:     []v1.EndpointPort{{Name: "runtime", Protocol: v1.ProtocolTCP, Port: 8080}},
		}},
	}
	if err := store.UpsertService(service); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatal(err)
	}
	store.MarkSourceSynced()
	name := proxy.ServicePortName{NamespacedName: types.NamespacedName{Namespace: service.Namespace, Name: service.Name}, Port: "runtime"}
	lb := &LoadBalancer{
		managedRuntime: store,
		services: map[proxy.ServicePortName]*balancerState{
			name: {endpoints: []string{"legacy-node:legacy-pod:10.244.0.99:9999"}, affinity: *newAffinityPolicy(v1.ServiceAffinityNone, 0)},
		},
	}
	if _, _, err := lb.nextEndpointWithConn(name, &net.TCPAddr{}, false, nil, nil); err == nil {
		t.Fatal("unapplied managed route fell through to the legacy endpoint")
	}
	store.MarkPortalPending(service.UID, name)
	store.MarkPortalApplied(service.UID, name, true)
	endpoint, _, err := lb.nextEndpointWithConn(name, &net.TCPAddr{}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "edge-a:runtime-pod:10.244.0.8:8080" {
		t.Fatalf("selected %q instead of exact managed endpoint", endpoint)
	}
}

func TestManagedRuntimeCoexistsWithLegacyService(t *testing.T) {
	store := meshstate.NewStore()
	name := proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: "dayu-v13", Name: "legacy-processor"},
		Port:           "tcp-0",
	}
	legacyEndpoint := "edge-v13:legacy-pod:10.244.0.13:8080"
	lb := &LoadBalancer{
		hostname:       "edge-v13",
		managedRuntime: store,
		services: map[proxy.ServicePortName]*balancerState{
			name: {
				endpoints: []string{legacyEndpoint},
				affinity:  *newAffinityPolicy(v1.ServiceAffinityNone, 0),
			},
		},
	}

	endpoint, _, err := lb.nextEndpointWithConn(name, &net.TCPAddr{}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != legacyEndpoint {
		t.Fatalf("legacy Dayu v1.3 endpoint = %q, want %q", endpoint, legacyEndpoint)
	}
}
