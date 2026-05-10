package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/config"
	"github.com/EDessin/RouteRoulette/backend/internal/history"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/EDessin/RouteRoulette/backend/internal/strava"
)

type fakePlanner struct{}

func (fakePlanner) Generate(_ *http.Request, _ planner.GenerateRouteRequest) (planner.RouteResponse, error) {
	return planner.RouteResponse{}, nil
}

type fakeGeocoder struct{}

func (fakeGeocoder) SearchAddress(_ *http.Request, _ string) (planner.GeocodeResponse, error) {
	return planner.GeocodeResponse{}, nil
}

func TestHistoryStatusAndStravaSyncEndpoints(t *testing.T) {
	streamCalls := 0
	stravaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/athlete/activities":
			_ = json.NewEncoder(w).Encode([]strava.Activity{
				{ID: 10, Name: "Run", SportType: "Run", StartDate: "2026-05-01T10:00:00Z", DistanceM: 5000},
			})
		case "/activities/10/streams":
			streamCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"latlng": map[string]any{
					"data": [][]float64{{50.0, 4.0}, {50.1, 4.1}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer stravaServer.Close()

	dir := t.TempDir()
	historyStore := history.NewStore(dir)
	stravaClient := strava.NewClient(strava.Config{
		ClientID:     "id",
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/callback",
		DataDir:      dir,
		APIBaseURL:   stravaServer.URL,
	})
	if err := stravaClient.SaveToken(strava.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("SaveToken() returned error: %v", err)
	}
	server := NewServer(config.Config{}, fakePlanner{}, fakeGeocoder{}, stravaClient, &historyStore)

	statusResp := httptest.NewRecorder()
	server.Routes().ServeHTTP(statusResp, httptest.NewRequest(http.MethodGet, "/api/history/status", nil))
	if statusResp.Code != http.StatusOK {
		t.Fatalf("history status code = %d, want 200", statusResp.Code)
	}
	var status history.Status
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Connected || status.SyncedActivities != 0 {
		t.Fatalf("status = %+v, want connected with no synced activities", status)
	}

	syncResp := httptest.NewRecorder()
	server.Routes().ServeHTTP(syncResp, httptest.NewRequest(http.MethodPost, "/api/strava/sync", nil))
	if syncResp.Code != http.StatusOK {
		t.Fatalf("sync code = %d, want 200; body %s", syncResp.Code, syncResp.Body.String())
	}
	if streamCalls != 1 {
		t.Fatalf("streamCalls = %d, want 1", streamCalls)
	}

	secondSyncResp := httptest.NewRecorder()
	server.Routes().ServeHTTP(secondSyncResp, httptest.NewRequest(http.MethodPost, "/api/strava/sync", nil))
	if secondSyncResp.Code != http.StatusOK {
		t.Fatalf("second sync code = %d, want 200", secondSyncResp.Code)
	}
	if streamCalls != 1 {
		t.Fatalf("streamCalls after second sync = %d, want no refetch", streamCalls)
	}
}
