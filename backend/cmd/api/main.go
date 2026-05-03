package main

import (
	"log"
	"net/http"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/api"
	"github.com/EDessin/RouteRoulette/backend/internal/config"
	"github.com/EDessin/RouteRoulette/backend/internal/ors"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

func main() {
	cfg := config.Load()

	orsClient := ors.NewClient(cfg.ORSBaseURL, cfg.ORSAPIKey, 20*time.Second)
	routePlanner := planner.New(orsClient, cfg.AllowMockRoutes)
	server := api.NewServer(cfg, routePlanner)

	log.Printf("RouteRoulette API listening on :%s", cfg.Port)
	if cfg.ORSAPIKey == "" {
		log.Printf("ORS_API_KEY is not set; mock routes are %t", cfg.AllowMockRoutes)
	}

	if err := http.ListenAndServe(":"+cfg.Port, server.Routes()); err != nil {
		log.Fatal(err)
	}
}
