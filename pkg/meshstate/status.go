package meshstate

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

type StatusServer struct {
	address string
	store   *Store
	once    sync.Once
	server  *http.Server
}

func NewStatusServer(address string, store *Store) *StatusServer {
	return &StatusServer{address: address, store: store}
}

func (s *StatusServer) Start() error {
	var startErr error
	s.once.Do(func() {
		listener, err := net.Listen("tcp", s.address)
		if err != nil {
			startErr = fmt.Errorf("listen on managed runtime status address %s: %w", s.address, err)
			return
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", s.health)
		mux.HandleFunc("/readyz", s.ready)
		mux.HandleFunc("/v1/routes", s.routes)
		mux.HandleFunc("/v1/routes/", s.route)
		s.server = &http.Server{Addr: s.address, Handler: mux, ReadHeaderTimeout: 2 * time.Second}
		go func() {
			klog.InfoS("Start managed runtime status server", "address", s.address)
			if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
				klog.ErrorS(err, "Managed runtime status server stopped")
			}
		}()
	})
	return startErr
}

func (s *StatusServer) Close() error {
	if s == nil || s.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *StatusServer) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *StatusServer) ready(w http.ResponseWriter, _ *http.Request) {
	snapshot := s.store.Snapshot()
	if snapshot.SourceState == SourceSynced {
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, snapshot)
}

func (s *StatusServer) routes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Snapshot())
}

func (s *StatusServer) route(w http.ResponseWriter, r *http.Request) {
	serviceUID := strings.TrimPrefix(r.URL.Path, "/v1/routes/")
	if serviceUID == "" || strings.Contains(serviceUID, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serviceUID is required"})
		return
	}
	snapshot := s.store.snapshot.Load().(*Snapshot)
	runtimeUID, exists := snapshot.serviceUIDs[serviceUID]
	route, routeExists := snapshot.Routes[runtimeUID]
	exists = exists && routeExists
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("route for Service UID %s not found", serviceUID)})
		return
	}
	state := route.Phase
	if snapshot.SourceState != SourceSynced {
		state = PhaseDegraded
	}
	response := routeStatusResponse{
		ServiceUID:               string(route.ServiceUID),
		RuntimeServiceUID:        route.RuntimeServiceUID,
		ServiceResourceVersion:   route.ServiceResourceVersion,
		EndpointsResourceVersion: route.EndpointsResourceVersion,
		DeploymentRevision:       route.DeploymentRevision,
		RuntimeID:                route.RuntimeID,
		State:                    state,
		LocalSequence:            snapshot.Sequence,
		SourceState:              snapshot.SourceState,
		TargetNode:               route.TargetNode,
		EndpointPodUID:           string(route.EndpointPodUID),
		Reason:                   route.Reason,
		ObservedAt:               route.UpdatedAt,
	}
	if snapshot.SourceState != SourceSynced {
		response.Reason = snapshot.SourceMessage
	}
	status := http.StatusOK
	if route.Phase != PhaseApplied || snapshot.SourceState != SourceSynced {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, response)
}

type routeStatusResponse struct {
	ServiceUID               string    `json:"serviceUID"`
	RuntimeServiceUID        string    `json:"runtimeServiceUID"`
	ServiceResourceVersion   string    `json:"serviceResourceVersion"`
	EndpointsResourceVersion string    `json:"endpointsResourceVersion"`
	DeploymentRevision       int64     `json:"deploymentRevision"`
	RuntimeID                string    `json:"runtimeID"`
	State                    string    `json:"state"`
	LocalSequence            uint64    `json:"localSequence"`
	SourceState              string    `json:"sourceState"`
	TargetNode               string    `json:"targetNode"`
	EndpointPodUID           string    `json:"endpointPodUID"`
	Reason                   string    `json:"reason,omitempty"`
	ObservedAt               time.Time `json:"observedAt"`
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
