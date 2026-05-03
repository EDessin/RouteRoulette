package planner

import "errors"

var (
	ErrInvalidRequest   = errors.New("invalid route request")
	ErrRouteUnavailable = errors.New("route could not be generated")
)

type Coordinate struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type GeocodeRequest struct {
	Text string `json:"text"`
}

type GeocodeResponse struct {
	Label string     `json:"label"`
	Home  Coordinate `json:"home"`
}

type GenerateRouteRequest struct {
	Home                  Coordinate `json:"home"`
	TargetDistanceKm      float64    `json:"targetDistanceKm"`
	MaxStartDistanceKm    float64    `json:"maxStartDistanceKm"`
	EstimatedPaceMinPerKm *float64   `json:"estimatedPaceMinPerKm,omitempty"`
	PreferPaved           bool       `json:"preferPaved"`
	MinPavedPercent       float64    `json:"minPavedPercent"`
	Seed                  *int64     `json:"seed,omitempty"`
}

type RouteResponse struct {
	RouteID         string      `json:"routeId"`
	Start           Coordinate  `json:"start"`
	DistanceKm      float64     `json:"distanceKm"`
	DurationMinutes float64     `json:"durationMinutes"`
	Geometry        GeoJSONLine `json:"geometry"`
	PavedPercent    *float64    `json:"pavedPercent,omitempty"`
	Provider        string      `json:"provider"`
	Warnings        []string    `json:"warnings,omitempty"`
}

type GeoJSONLine struct {
	Type        string      `json:"type"`
	Coordinates [][]float64 `json:"coordinates"`
}

type CandidateRequest struct {
	Start           Coordinate
	TargetDistanceM float64
	PreferPaved     bool
	MinPavedPercent float64
	Seed            int64
}

type CandidateRoute struct {
	Start           Coordinate
	DistanceM       float64
	DurationSeconds float64
	Geometry        GeoJSONLine
	PavedPercent    *float64
	Provider        string
	Warnings        []string
}
