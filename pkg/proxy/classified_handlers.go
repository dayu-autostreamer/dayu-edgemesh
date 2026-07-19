package proxy

import (
	v1 "k8s.io/api/core/v1"
	proxyconfig "k8s.io/kubernetes/pkg/proxy/config"

	"github.com/kubeedge/edgemesh/pkg/meshstate"
)

func isManaged(labels map[string]string) bool {
	return labels[meshstate.LabelMeshManaged] == "true"
}

// classifiedServiceHandler lets the primary MetaServer informer remain the
// sole managed-runtime source while an independent Kubernetes informer owns
// only legacy Services. A label transition is forwarded atomically by the
// destination-class source; the source-class informer ignores it. This avoids
// racing delete/add events from two independent watches for the same Service.
// Only the primary handler is given onSynced, so managed readiness never
// depends on the optional legacy recovery source.
type classifiedServiceHandler struct {
	handler  proxyconfig.ServiceHandler
	managed  bool
	onSynced func()
}

func (h *classifiedServiceHandler) matches(service *v1.Service) bool {
	return service != nil && isManaged(service.Labels) == h.managed
}

func (h *classifiedServiceHandler) OnServiceAdd(service *v1.Service) {
	if h.matches(service) {
		h.handler.OnServiceAdd(service)
	}
}

func (h *classifiedServiceHandler) OnServiceUpdate(oldService, service *v1.Service) {
	if h.matches(service) {
		h.handler.OnServiceUpdate(oldService, service)
	}
}

func (h *classifiedServiceHandler) OnServiceDelete(service *v1.Service) {
	if h.matches(service) {
		h.handler.OnServiceDelete(service)
	}
}

func (h *classifiedServiceHandler) OnServiceSynced() {
	if h.onSynced != nil {
		h.onSynced()
	}
}

type classifiedEndpointsHandler struct {
	handler  proxyconfig.EndpointsHandler
	managed  bool
	onSynced func()
}

func (h *classifiedEndpointsHandler) matches(endpoints *v1.Endpoints) bool {
	return endpoints != nil && isManaged(endpoints.Labels) == h.managed
}

func (h *classifiedEndpointsHandler) OnEndpointsAdd(endpoints *v1.Endpoints) {
	if h.matches(endpoints) {
		h.handler.OnEndpointsAdd(endpoints)
	}
}

func (h *classifiedEndpointsHandler) OnEndpointsUpdate(oldEndpoints, endpoints *v1.Endpoints) {
	if h.matches(endpoints) {
		h.handler.OnEndpointsUpdate(oldEndpoints, endpoints)
	}
}

func (h *classifiedEndpointsHandler) OnEndpointsDelete(endpoints *v1.Endpoints) {
	if h.matches(endpoints) {
		h.handler.OnEndpointsDelete(endpoints)
	}
}

func (h *classifiedEndpointsHandler) OnEndpointsSynced() {
	if h.onSynced != nil {
		h.onSynced()
	}
}
