package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/avoidance"
	"github.com/EDessin/RouteRoulette/backend/internal/config"
	"github.com/EDessin/RouteRoulette/backend/internal/history"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/EDessin/RouteRoulette/backend/internal/strava"
)

type RoutePlanner interface {
	Generate(r *http.Request, req planner.GenerateRouteRequest) (planner.RouteResponse, error)
}

type Geocoder interface {
	SearchAddress(r *http.Request, text string) (planner.GeocodeResponse, error)
}

type Server struct {
	cfg            config.Config
	planner        RoutePlanner
	geocoder       Geocoder
	stravaClient   strava.Client
	historyStore   *history.Store
	avoidanceStore *avoidance.Store
}

func NewServer(cfg config.Config, routePlanner RoutePlanner, geocoder Geocoder, stravaClient strava.Client, historyStore *history.Store, avoidanceStore *avoidance.Store) Server {
	return Server{
		cfg:            cfg,
		planner:        routePlanner,
		geocoder:       geocoder,
		stravaClient:   stravaClient,
		historyStore:   historyStore,
		avoidanceStore: avoidanceStore,
	}
}

func (s Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/geocode", s.handleGeocode)
	mux.HandleFunc("POST /api/routes/generate", s.handleGenerateRoute)
	mux.HandleFunc("GET /api/strava/connect", s.handleStravaConnect)
	mux.HandleFunc("GET /api/strava/callback", s.handleStravaCallback)
	mux.HandleFunc("POST /api/strava/sync", s.handleStravaSync)
	mux.HandleFunc("GET /api/history/status", s.handleHistoryStatus)
	mux.HandleFunc("DELETE /api/history", s.handleHistoryDelete)
	mux.HandleFunc("GET /api/avoidance", s.handleAvoidanceList)
	mux.HandleFunc("POST /api/avoidance", s.handleAvoidanceAdd)
	mux.HandleFunc("DELETE /api/avoidance/{id}", s.handleAvoidanceDelete)

	return s.withCORS(mux)
}

func (s Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s Server) handleGeocode(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var req planner.GeocodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "The geocode request body is not valid JSON.")
		return
	}

	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "Enter a home address.")
		return
	}

	result, err := s.geocoder.SearchAddress(r, text)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
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

func (s Server) handleStravaConnect(w http.ResponseWriter, r *http.Request) {
	authURL, err := s.stravaClient.AuthorizationURL()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s Server) handleStravaCallback(w http.ResponseWriter, r *http.Request) {
	if errText := strings.TrimSpace(r.URL.Query().Get("error")); errText != "" {
		writeError(w, http.StatusBadRequest, "Strava authorization failed: "+errText)
		return
	}
	if _, err := s.stravaClient.ExchangeCode(r.Context(), r.URL.Query().Get("code")); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<!doctype html><title>RouteRoulette</title><p>Strava connected. You can close this tab and return to RouteRoulette.</p>"))
}

func (s Server) handleStravaSync(w http.ResponseWriter, r *http.Request) {
	result, err := history.SyncStrava(r.Context(), s.stravaClient, s.historyStore)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s Server) handleHistoryStatus(w http.ResponseWriter, _ *http.Request) {
	status, err := s.historyStore.Status(s.stravaClient.Connected())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s Server) handleHistoryDelete(w http.ResponseWriter, _ *http.Request) {
	if err := s.historyStore.Clear(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s Server) handleAvoidanceList(w http.ResponseWriter, _ *http.Request) {
	roads, err := s.avoidanceStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, roads)
}

func (s Server) handleAvoidanceAdd(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var req avoidance.AddRoadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "The avoided road request body is not valid JSON.")
		return
	}
	road, err := s.avoidanceStore.Add(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, road)
}

func (s Server) handleAvoidanceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := url.PathUnescape(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Avoided road ID is not valid.")
		return
	}
	if err := s.avoidanceStore.Delete(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "Avoided road not found.")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := s.cfg.CORSAllowedOrigin
		if origin == "" {
			origin = "http://localhost:4200"
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
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
