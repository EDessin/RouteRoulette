package config

import "testing"

func TestLoadDefaultsToTwentyKilometerOSMRadius(t *testing.T) {
	t.Setenv("OSM_RADIUS_KM", "")
	t.Setenv("HISTORY_DATA_DIR", "")
	t.Setenv("STRAVA_REDIRECT_URL", "")

	cfg := Load()
	if cfg.OSMRadiusKm != 20 {
		t.Fatalf("OSMRadiusKm = %.0f, want 20", cfg.OSMRadiusKm)
	}
	if cfg.HistoryDataDir != "data/history" {
		t.Fatalf("HistoryDataDir = %q, want data/history", cfg.HistoryDataDir)
	}
	if cfg.StravaRedirectURL != "http://localhost:8080/api/strava/callback" {
		t.Fatalf("StravaRedirectURL = %q, want default callback", cfg.StravaRedirectURL)
	}
}
