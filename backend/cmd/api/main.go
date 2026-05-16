package main

import (
	"log"
	"net/http"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/api"
	"github.com/EDessin/RouteRoulette/backend/internal/avoidance"
	"github.com/EDessin/RouteRoulette/backend/internal/config"
	"github.com/EDessin/RouteRoulette/backend/internal/history"
	"github.com/EDessin/RouteRoulette/backend/internal/localosm"
	"github.com/EDessin/RouteRoulette/backend/internal/ors"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/EDessin/RouteRoulette/backend/internal/strava"
	"github.com/EDessin/RouteRoulette/backend/internal/surfacemarks"
)

func main() {
	cfg := config.Load()

	historyStore := history.NewStore(cfg.HistoryDataDir)
	avoidanceStore := avoidance.NewStore(cfg.AvoidanceDataDir)
	surfaceMarksStore := surfacemarks.NewStore(cfg.SurfaceMarksDataDir)
	stravaClient := strava.NewClient(strava.Config{
		ClientID:     cfg.StravaClientID,
		ClientSecret: cfg.StravaClientSecret,
		RedirectURL:  cfg.StravaRedirectURL,
		DataDir:      cfg.HistoryDataDir,
	})
	orsClient := ors.NewClient(cfg.ORSBaseURL, cfg.ORSAPIKey, 20*time.Second)
	routeProvider := planner.RouteProvider(orsClient)
	if cfg.RoutingProvider == "local_osm" {
		routeProvider = localosm.NewProvider(localosm.Config{
			DataDir:        cfg.OSMDataDir,
			ExtractPath:    cfg.OSMExtractPath,
			ExtractURL:     cfg.OSMExtractURL,
			RadiusKm:       cfg.OSMRadiusKm,
			AllowDownload:  cfg.AllowOSMDownload,
			HistoryStore:   &historyStore,
			AvoidanceStore: &avoidanceStore,
			SurfaceStore:   &surfaceMarksStore,
		})
	}
	routePlanner := planner.New(routeProvider, cfg.AllowMockRoutes)
	server := api.NewServer(cfg, routePlanner, orsClient, stravaClient, &historyStore, &avoidanceStore, &surfaceMarksStore)

	log.Printf("RouteRoulette API listening on :%s", cfg.Port)
	log.Printf("Routing provider: %s", cfg.RoutingProvider)
	if cfg.ORSAPIKey == "" {
		log.Printf("ORS_API_KEY is not set; mock routes are %t", cfg.AllowMockRoutes)
	}

	if err := http.ListenAndServe(":"+cfg.Port, server.Routes()); err != nil {
		log.Fatal(err)
	}
}
