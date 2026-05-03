package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port              string
	ORSBaseURL        string
	ORSAPIKey         string
	CORSAllowedOrigin string
	AllowMockRoutes   bool
}

func Load() Config {
	return Config{
		Port:              getEnv("PORT", "8080"),
		ORSBaseURL:        getEnv("ORS_BASE_URL", "https://api.openrouteservice.org"),
		ORSAPIKey:         os.Getenv("ORS_API_KEY"),
		CORSAllowedOrigin: getEnv("CORS_ALLOWED_ORIGIN", "http://localhost:4200"),
		AllowMockRoutes:   getEnvBool("ALLOW_MOCK_ROUTES", true),
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
