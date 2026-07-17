package meshstate

import (
	"fmt"
	"net"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/proxy"
)

const (
	DefaultStatusListenAddress = "127.0.0.1:10551"

	LabelMeshManaged        = "dayu.io/mesh-managed"
	LabelInstallID          = "dayu.io/install-id"
	LabelDeploymentRevision = "dayu.io/deployment-revision"
	LabelRuntimeID          = "dayu.io/runtime-id"
	LabelComponent          = "dayu.io/component"
	LabelRuntimeServiceUID  = "dayu.io/runtime-service-uid"

	AnnotationLogicalService = "dayu.io/logical-service"
	AnnotationTargetNode     = "dayu.io/target-node"

	PhaseReady    = "READY"
	PhaseApplied  = "APPLIED"
	PhaseDegraded = "DEGRADED"

	SourceSyncing  = "SYNCING"
	SourceSynced   = "SYNCED"
	SourceDegraded = "DEGRADED"
)

var identityLabels = []string{
	LabelMeshManaged,
	LabelInstallID,
	LabelDeploymentRevision,
	LabelRuntimeID,
	LabelComponent,
	LabelRuntimeServiceUID,
}

// Route is the immutable projection consumed by the proxy and local status API.
// RuntimeServiceUID is the external stable key; ServiceUID identifies one
// Kubernetes Service incarnation and is used for stale replay protection.
type Route struct {
	RuntimeServiceUID        string      `json:"runtimeServiceUID"`
	ServiceUID               types.UID   `json:"serviceUID"`
	ServiceResourceVersion   string      `json:"serviceResourceVersion"`
	EndpointsResourceVersion string      `json:"endpointsResourceVersion"`
	Namespace                string      `json:"namespace"`
	ServiceName              string      `json:"serviceName"`
	LogicalService           string      `json:"logicalService"`
	InstallID                string      `json:"installID"`
	DeploymentRevision       int64       `json:"deploymentRevision"`
	RuntimeID                string      `json:"runtimeID"`
	Component                string      `json:"component"`
	TargetNode               string      `json:"targetNode"`
	FQDN                     string      `json:"fqdn"`
	PortName                 string      `json:"portName"`
	ServiceIP                string      `json:"serviceIP"`
	ServicePort              int32       `json:"servicePort"`
	EndpointIP               string      `json:"endpointIP"`
	EndpointPort             int32       `json:"endpointPort"`
	EndpointPod              string      `json:"endpointPod"`
	EndpointPodUID           types.UID   `json:"endpointPodUID"`
	Protocol                 v1.Protocol `json:"protocol"`
	Phase                    string      `json:"phase"`
	Reason                   string      `json:"reason,omitempty"`
	ProxyApplied             bool        `json:"proxyApplied"`
	UpdatedAt                time.Time   `json:"updatedAt"`
}

func (r Route) Key() string {
	return namespacedKey(r.Namespace, r.ServiceName)
}

func (r Route) ServicePortName() proxy.ServicePortName {
	return proxy.ServicePortName{
		NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: r.ServiceName},
		Port:           r.PortName,
	}
}

func (r Route) Endpoint() string {
	pod := r.EndpointPod
	if pod == "" {
		pod = "EMPTY_POD_NAME"
	}
	return fmt.Sprintf("%s:%s:%s", r.TargetNode, pod, net.JoinHostPort(r.EndpointIP, strconv.Itoa(int(r.EndpointPort))))
}

// Snapshot is immutable after publication. All maps are deep-copied by Store.
type Snapshot struct {
	Sequence      uint64           `json:"sequence"`
	SourceState   string           `json:"sourceState"`
	SourceMessage string           `json:"sourceMessage,omitempty"`
	UpdatedAt     time.Time        `json:"updatedAt"`
	Routes        map[string]Route `json:"routes"` // valid routes are keyed by RuntimeService UID
	servicePorts  map[string]string
	serviceUIDs   map[string]string
}

func namespacedKey(namespace, name string) string {
	return namespace + "/" + name
}

func servicePortKey(name proxy.ServicePortName) string {
	return namespacedKey(name.NamespacedName.Namespace, name.NamespacedName.Name) + ":" + name.Port
}

func fqdn(service, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", service, namespace)
}
