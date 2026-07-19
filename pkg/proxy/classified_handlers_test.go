package proxy

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeedge/edgemesh/pkg/meshstate"
)

type recordingServiceHandler struct {
	events []string
	synced int
}

func objectClass(labels map[string]string) string {
	if isManaged(labels) {
		return "managed"
	}
	return "legacy"
}

func (h *recordingServiceHandler) OnServiceAdd(service *v1.Service) {
	h.events = append(h.events, "add:"+objectClass(service.Labels))
}

func (h *recordingServiceHandler) OnServiceUpdate(oldService, service *v1.Service) {
	h.events = append(h.events, fmt.Sprintf("update:%s:%s", objectClass(oldService.Labels), objectClass(service.Labels)))
}

func (h *recordingServiceHandler) OnServiceDelete(service *v1.Service) {
	h.events = append(h.events, "delete:"+objectClass(service.Labels))
}

func (h *recordingServiceHandler) OnServiceSynced() {
	h.synced++
}

type recordingEndpointsHandler struct {
	events []string
	synced int
}

func (h *recordingEndpointsHandler) OnEndpointsAdd(endpoints *v1.Endpoints) {
	h.events = append(h.events, "add:"+objectClass(endpoints.Labels))
}

func (h *recordingEndpointsHandler) OnEndpointsUpdate(oldEndpoints, endpoints *v1.Endpoints) {
	h.events = append(h.events, fmt.Sprintf("update:%s:%s", objectClass(oldEndpoints.Labels), objectClass(endpoints.Labels)))
}

func (h *recordingEndpointsHandler) OnEndpointsDelete(endpoints *v1.Endpoints) {
	h.events = append(h.events, "delete:"+objectClass(endpoints.Labels))
}

func (h *recordingEndpointsHandler) OnEndpointsSynced() {
	h.synced++
}

func TestClassifiedServiceHandlersKeepLegacyAndManagedSourcesDisjoint(t *testing.T) {
	recorder := &recordingServiceHandler{}
	legacyHandler := &classifiedServiceHandler{handler: recorder, managed: false}
	managedHandler := &classifiedServiceHandler{handler: recorder, managed: true, onSynced: recorder.OnServiceSynced}

	legacy := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "runtime"}}
	managed := legacy.DeepCopy()
	managed.Labels = map[string]string{meshstate.LabelMeshManaged: "true"}

	legacyHandler.OnServiceAdd(legacy)
	managedHandler.OnServiceAdd(legacy)
	legacyHandler.OnServiceAdd(managed)
	managedHandler.OnServiceAdd(managed)

	legacyHandler.OnServiceUpdate(legacy, legacy.DeepCopy())
	managedHandler.OnServiceUpdate(legacy, legacy.DeepCopy())
	legacyHandler.OnServiceUpdate(legacy, managed)
	managedHandler.OnServiceUpdate(legacy, managed)
	managedHandler.OnServiceUpdate(managed, legacy)
	legacyHandler.OnServiceUpdate(managed, legacy)
	managedHandler.OnServiceDelete(legacy)
	legacyHandler.OnServiceDelete(legacy)
	legacyHandler.OnServiceDelete(managed)
	managedHandler.OnServiceDelete(managed)

	want := []string{
		"add:legacy", "add:managed",
		"update:legacy:legacy",
		"update:legacy:managed",
		"update:managed:legacy",
		"delete:legacy", "delete:managed",
	}
	if fmt.Sprint(recorder.events) != fmt.Sprint(want) {
		t.Fatalf("service events = %v, want %v", recorder.events, want)
	}

	legacyHandler.OnServiceSynced()
	if recorder.synced != 0 {
		t.Fatalf("legacy recovery source published service readiness")
	}
	managedHandler.OnServiceSynced()
	if recorder.synced != 1 {
		t.Fatalf("service synced callbacks = %d, want 1", recorder.synced)
	}
}

func TestClassifiedEndpointsHandlersPreserveLabelTransitions(t *testing.T) {
	recorder := &recordingEndpointsHandler{}
	legacyHandler := &classifiedEndpointsHandler{handler: recorder, managed: false}
	managedHandler := &classifiedEndpointsHandler{handler: recorder, managed: true, onSynced: recorder.OnEndpointsSynced}

	legacy := &v1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "runtime"}}
	managed := legacy.DeepCopy()
	managed.Labels = map[string]string{meshstate.LabelMeshManaged: "true"}

	legacyHandler.OnEndpointsAdd(legacy)
	managedHandler.OnEndpointsAdd(legacy)
	legacyHandler.OnEndpointsAdd(managed)
	managedHandler.OnEndpointsAdd(managed)
	legacyHandler.OnEndpointsUpdate(legacy, managed)
	managedHandler.OnEndpointsUpdate(legacy, managed)
	managedHandler.OnEndpointsUpdate(managed, legacy)
	legacyHandler.OnEndpointsUpdate(managed, legacy)
	managedHandler.OnEndpointsDelete(legacy)
	legacyHandler.OnEndpointsDelete(legacy)
	legacyHandler.OnEndpointsDelete(managed)
	managedHandler.OnEndpointsDelete(managed)

	want := []string{
		"add:legacy", "add:managed",
		"update:legacy:managed", "update:managed:legacy",
		"delete:legacy", "delete:managed",
	}
	if fmt.Sprint(recorder.events) != fmt.Sprint(want) {
		t.Fatalf("endpoints events = %v, want %v", recorder.events, want)
	}

	legacyHandler.OnEndpointsSynced()
	if recorder.synced != 0 {
		t.Fatalf("legacy recovery source published endpoints readiness")
	}
	managedHandler.OnEndpointsSynced()
	if recorder.synced != 1 {
		t.Fatalf("endpoints synced callbacks = %d, want 1", recorder.synced)
	}
}
