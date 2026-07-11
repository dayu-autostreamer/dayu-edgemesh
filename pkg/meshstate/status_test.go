package meshstate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouteStatusRequiresKnownExactAppliedRoute(t *testing.T) {
	store := NewStore()
	server := NewStatusServer("127.0.0.1:0", store)

	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.route(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}

	if response := request("/v1/routes/missing"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d body=%s", response.Code, response.Body.String())
	}
	if response := request("/v1/routes/"); response.Code != http.StatusBadRequest {
		t.Fatalf("empty service UID status = %d body=%s", response.Code, response.Body.String())
	}

	service, endpoints := managedObjects("service-a", "runtime-service-a")
	if err := store.UpsertService(service); err != nil {
		t.Fatalf("upsert service: %v", err)
	}
	if err := store.UpsertEndpoints(endpoints); err != nil {
		t.Fatalf("upsert endpoints: %v", err)
	}

	response := request("/v1/routes/service-a")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unapplied route status = %d body=%s", response.Code, response.Body.String())
	}
	var status routeStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if status.ServiceUID != "service-a" || status.RuntimeServiceUID != "runtime-service-a" || status.EndpointPodUID != "pod-a" {
		t.Fatalf("status lost exact identity: %#v", status)
	}
	if status.DeploymentRevision != 7 || status.State != PhaseDegraded || status.SourceState != SourceSyncing || status.LocalSequence == 0 {
		t.Fatalf("unexpected pre-apply status: %#v", status)
	}

	store.MarkPortalApplied(service.UID, servicePortName(service), true)
	store.MarkSourceSynced()
	response = request("/v1/routes/service-a")
	if response.Code != http.StatusOK {
		t.Fatalf("applied route status = %d body=%s", response.Code, response.Body.String())
	}
	status = routeStatusResponse{}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode applied status response: %v", err)
	}
	if status.State != PhaseApplied || status.SourceState != SourceSynced || status.Reason != "" {
		t.Fatalf("unexpected applied status: %#v", status)
	}
}

func TestReadyStatusTracksPrimaryInformerSource(t *testing.T) {
	store := NewStore()
	server := NewStatusServer("127.0.0.1:0", store)
	recorder := httptest.NewRecorder()
	server.ready(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-sync ready status = %d", recorder.Code)
	}

	store.MarkSourceSynced()
	recorder = httptest.NewRecorder()
	server.ready(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("synced ready status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}
