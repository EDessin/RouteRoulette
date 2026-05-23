package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/avoidance"
	"github.com/EDessin/RouteRoulette/backend/internal/config"
	"github.com/EDessin/RouteRoulette/backend/internal/history"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/EDessin/RouteRoulette/backend/internal/strava"
	"github.com/EDessin/RouteRoulette/backend/internal/surfacemarks"
)

type fakePlanner struct{}

func (fakePlanner) Generate(_ *http.Request, _ planner.GenerateRouteRequest) (planner.RouteResponse, error) {
	return planner.RouteResponse{}, nil
}

func (fakePlanner) ImportRoute(_ *http.Request, _ planner.ImportRouteRequest) (planner.RouteResponse, error) {
	return planner.RouteResponse{
		RouteID:    "imported",
		DistanceKm: 1.23,
		Provider:   "local-osm-import",
	}, nil
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
	avoidanceStore := avoidance.NewStore(t.TempDir())
	surfaceStore := surfacemarks.NewStore(t.TempDir())
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
	server := NewServer(config.Config{}, fakePlanner{}, fakeGeocoder{}, stravaClient, &historyStore, &avoidanceStore, &surfaceStore)

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

func TestAvoidanceEndpoints(t *testing.T) {
	historyStore := history.NewStore(t.TempDir())
	avoidanceStore := avoidance.NewStore(t.TempDir())
	surfaceStore := surfacemarks.NewStore(t.TempDir())
	server := NewServer(config.Config{}, fakePlanner{}, fakeGeocoder{}, strava.Client{}, &historyStore, &avoidanceStore, &surfaceStore)

	addResp := httptest.NewRecorder()
	addReq := httptest.NewRequest(http.MethodPost, "/api/avoidance", strings.NewReader(`{"osmWayId":42,"name":"Busy Lane","reason":"busy_road","coordinate":{"lat":50.1,"lon":4.7}}`))
	server.Routes().ServeHTTP(addResp, addReq)
	if addResp.Code != http.StatusCreated {
		t.Fatalf("avoidance add code = %d, want 201; body %s", addResp.Code, addResp.Body.String())
	}

	listResp := httptest.NewRecorder()
	server.Routes().ServeHTTP(listResp, httptest.NewRequest(http.MethodGet, "/api/avoidance", nil))
	if listResp.Code != http.StatusOK {
		t.Fatalf("avoidance list code = %d, want 200", listResp.Code)
	}
	var roads []avoidance.Road
	if err := json.NewDecoder(listResp.Body).Decode(&roads); err != nil {
		t.Fatalf("decode roads: %v", err)
	}
	if len(roads) != 1 || roads[0].ID != "way:42" {
		t.Fatalf("roads = %+v, want way:42", roads)
	}

	deleteResp := httptest.NewRecorder()
	server.Routes().ServeHTTP(deleteResp, httptest.NewRequest(http.MethodDelete, "/api/avoidance/way%3A42", nil))
	if deleteResp.Code != http.StatusOK {
		t.Fatalf("avoidance delete code = %d, want 200", deleteResp.Code)
	}
}

func TestImportRouteEndpoint(t *testing.T) {
	historyStore := history.NewStore(t.TempDir())
	avoidanceStore := avoidance.NewStore(t.TempDir())
	surfaceStore := surfacemarks.NewStore(t.TempDir())
	server := NewServer(config.Config{}, fakePlanner{}, fakeGeocoder{}, strava.Client{}, &historyStore, &avoidanceStore, &surfaceStore)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/routes/import", strings.NewReader(`{"coordinates":[{"lat":50.1,"lon":4.7},{"lat":50.2,"lon":4.8}]}`))
	server.Routes().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("import route code = %d, want 200; body %s", resp.Code, resp.Body.String())
	}

	var route planner.RouteResponse
	if err := json.NewDecoder(resp.Body).Decode(&route); err != nil {
		t.Fatalf("decode imported route: %v", err)
	}
	if route.RouteID != "imported" || route.Provider != "local-osm-import" {
		t.Fatalf("imported route = %+v, want fake imported route", route)
	}
}

func TestSurfaceMarkEndpoints(t *testing.T) {
	historyStore := history.NewStore(t.TempDir())
	avoidanceStore := avoidance.NewStore(t.TempDir())
	surfaceStore := surfacemarks.NewStore(t.TempDir())
	server := NewServer(config.Config{}, fakePlanner{}, fakeGeocoder{}, strava.Client{}, &historyStore, &avoidanceStore, &surfaceStore)

	addResp := httptest.NewRecorder()
	addReq := httptest.NewRequest(http.MethodPost, "/api/surface-marks", strings.NewReader(`{"osmWayId":42,"name":"Mystery Lane","surface":"paved","coordinate":{"lat":50.1,"lon":4.7}}`))
	server.Routes().ServeHTTP(addResp, addReq)
	if addResp.Code != http.StatusCreated {
		t.Fatalf("surface mark add code = %d, want 201; body %s", addResp.Code, addResp.Body.String())
	}

	listResp := httptest.NewRecorder()
	server.Routes().ServeHTTP(listResp, httptest.NewRequest(http.MethodGet, "/api/surface-marks", nil))
	if listResp.Code != http.StatusOK {
		t.Fatalf("surface mark list code = %d, want 200", listResp.Code)
	}
	var roads []surfacemarks.Road
	if err := json.NewDecoder(listResp.Body).Decode(&roads); err != nil {
		t.Fatalf("decode marked roads: %v", err)
	}
	if len(roads) != 1 || roads[0].ID != "way:42" || roads[0].Surface != surfacemarks.SurfacePaved {
		t.Fatalf("marked roads = %+v, want paved way:42", roads)
	}

	deleteResp := httptest.NewRecorder()
	server.Routes().ServeHTTP(deleteResp, httptest.NewRequest(http.MethodDelete, "/api/surface-marks/way%3A42", nil))
	if deleteResp.Code != http.StatusOK {
		t.Fatalf("surface mark delete code = %d, want 200", deleteResp.Code)
	}
}
