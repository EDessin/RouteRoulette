package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/config"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

type RoutePlanner interface {
	Generate(r *http.Request, req planner.GenerateRouteRequest) (planner.RouteResponse, error)
}

type Server struct {
	cfg     config.Config
	planner RoutePlanner
}

func NewServer(cfg config.Config, routePlanner RoutePlanner) Server {
	return Server{
		cfg:     cfg,
		planner: routePlanner,
	}
}

func (s Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/routes/generate", s.handleGenerateRoute)

	return s.withCORS(mux)
}

func (s Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s Server) handleGenerateRoute(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var req planner.GenerateRouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "The route request body is not valid JSON.")
		return
	}

	route, err := s.planner.Generate(r, req)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, planner.ErrInvalidRequest) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, planner.ErrRouteUnavailable) {
			status = http.StatusBadGateway
		}
		writeError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, route)
}

func (s Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := s.cfg.CORSAllowedOrigin
		if origin == "" {
			origin = "http://localhost:4200"
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{
		"error": message,
	})
}
