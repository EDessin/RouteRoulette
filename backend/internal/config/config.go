package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port               string
	ORSBaseURL         string
	ORSAPIKey          string
	CORSAllowedOrigin  string
	AllowMockRoutes    bool
	RoutingProvider    string
	OSMDataDir         string
	OSMExtractPath     string
	OSMExtractURL      string
	OSMRadiusKm        float64
	AllowOSMDownload   bool
	HistoryDataDir     string
	StravaClientID     string
	StravaClientSecret string
	StravaRedirectURL  string
}

func Load() Config {
	return Config{
		Port:               getEnv("PORT", "8080"),
		ORSBaseURL:         getEnv("ORS_BASE_URL", "https://api.openrouteservice.org"),
		ORSAPIKey:          os.Getenv("ORS_API_KEY"),
		CORSAllowedOrigin:  getEnv("CORS_ALLOWED_ORIGIN", "http://localhost:4200"),
		AllowMockRoutes:    getEnvBool("ALLOW_MOCK_ROUTES", true),
		RoutingProvider:    getEnv("ROUTING_PROVIDER", "local_osm"),
		OSMDataDir:         getEnv("OSM_DATA_DIR", "data/osm"),
		OSMExtractPath:     getEnv("OSM_EXTRACT_PATH", "data/osm/belgium-latest.osm.pbf"),
		OSMExtractURL:      getEnv("OSM_EXTRACT_URL", "https://download.geofabrik.de/europe/belgium-latest.osm.pbf"),
		OSMRadiusKm:        getEnvFloat("OSM_RADIUS_KM", 20),
		AllowOSMDownload:   getEnvBool("ALLOW_OSM_DOWNLOAD", true),
		HistoryDataDir:     getEnv("HISTORY_DATA_DIR", "data/history"),
		StravaClientID:     os.Getenv("STRAVA_CLIENT_ID"),
		StravaClientSecret: os.Getenv("STRAVA_CLIENT_SECRET"),
		StravaRedirectURL:  getEnv("STRAVA_REDIRECT_URL", "http://localhost:8080/api/strava/callback"),
	}
}

func getEnv(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func getEnvBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvFloat(key string, fallback float64) float64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
