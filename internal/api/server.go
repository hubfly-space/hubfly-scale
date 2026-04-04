package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"hubfly-scale/internal/model"
	"hubfly-scale/internal/scaler"
	"hubfly-scale/internal/store"
)

type Server struct {
	store   *store.SQLiteStore
	manager *scaler.Manager
	logger  *log.Logger
}

func NewServer(st *store.SQLiteStore, manager *scaler.Manager, logger *log.Logger) *Server {
	return &Server{store: st, manager: manager, logger: logger}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/containers", s.handleContainers)
	mux.HandleFunc("/v1/containers/", s.handleContainerByName)
	return withJSON(mux)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		containers, err := s.store.ListContainers(r.Context())
		if err != nil {
			respondError(w, http.StatusInternalServerError, err)
			return
		}
		respondJSON(w, http.StatusOK, containers)
	case http.MethodPost:
		var req model.ContainerConfigAPI
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, err)
			return
		}
		if req.Name == "" {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		if req.SleepAfterSeconds <= 0 {
			req.SleepAfterSeconds = 60
		}
		if req.BusyWindowSeconds <= 0 {
			req.BusyWindowSeconds = 2
		}
		if req.InspectIntervalSecs <= 0 {
			req.InspectIntervalSecs = 5
		}
		if req.MinCPU <= 0 || req.MaxCPU <= 0 {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "min_cpu and max_cpu are required"})
			return
		}
		if req.MaxCPU < req.MinCPU {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "max_cpu must be >= min_cpu"})
			return
		}

		cfg := req.ToInternal()
		if err := s.manager.StartOrRestart(r.Context(), cfg); err != nil {
			respondError(w, http.StatusInternalServerError, err)
			return
		}
		res, err := s.store.GetContainer(r.Context(), req.Name)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err)
			return
		}
		respondJSON(w, http.StatusCreated, res)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleContainerByName(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/containers/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	name := parts[0]

	if len(parts) == 1 && r.Method == http.MethodGet {
		info, err := s.store.GetContainer(r.Context(), name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			respondError(w, http.StatusInternalServerError, err)
			return
		}
		respondJSON(w, http.StatusOK, info)
		return
	}

	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := s.manager.Unregister(r.Context(), name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			respondError(w, http.StatusInternalServerError, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}

	if len(parts) == 2 && r.Method == http.MethodPost {
		action := parts[1]
		switch action {
		case "reload":
			info, err := s.store.GetContainer(r.Context(), name)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				respondError(w, http.StatusInternalServerError, err)
				return
			}
			if err := s.manager.StartOrRestart(context.Background(), info.Config); err != nil {
				respondError(w, http.StatusInternalServerError, err)
				return
			}
			respondJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
			return
		}
	}

	w.WriteHeader(http.StatusNotFound)
}

func withJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func respondError(w http.ResponseWriter, status int, err error) {
	respondJSON(w, status, map[string]string{"error": err.Error()})
}
