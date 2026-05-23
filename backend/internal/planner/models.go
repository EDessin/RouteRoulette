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
	SurfacePolicy         string     `json:"surfacePolicy,omitempty"`
	PreferUnrunRoads      bool       `json:"preferUnrunRoads"`
	Seed                  *int64     `json:"seed,omitempty"`
}

type ImportRouteRequest struct {
	Coordinates           []Coordinate `json:"coordinates"`
	EstimatedPaceMinPerKm *float64     `json:"estimatedPaceMinPerKm,omitempty"`
}

type RouteResponse struct {
	RouteID               string         `json:"routeId"`
	Start                 Coordinate     `json:"start"`
	DistanceKm            float64        `json:"distanceKm"`
	DurationMinutes       float64        `json:"durationMinutes"`
	Geometry              GeoJSONLine    `json:"geometry"`
	PavedPercent          *float64       `json:"pavedPercent,omitempty"`
	UnpavedPercent        *float64       `json:"unpavedPercent,omitempty"`
	UnknownSurfacePercent *float64       `json:"unknownSurfacePercent,omitempty"`
	UnrunPercent          *float64       `json:"unrunPercent,omitempty"`
	PreviouslyRunPercent  *float64       `json:"previouslyRunPercent,omitempty"`
	AvoidedRoadDistanceM  *float64       `json:"avoidedRoadDistanceM,omitempty"`
	Segments              []RouteSegment `json:"segments,omitempty"`
	Provider              string         `json:"provider"`
	Warnings              []string       `json:"warnings,omitempty"`
}

type GeoJSONLine struct {
	Type        string      `json:"type"`
	Coordinates [][]float64 `json:"coordinates"`
}

type CandidateRequest struct {
	Start            Coordinate
	Home             Coordinate
	TargetDistanceM  float64
	PreferPaved      bool
	MinPavedPercent  float64
	SurfacePolicy    string
	PreferUnrunRoads bool
	Seed             int64
}

type CandidateRoute struct {
	Start                Coordinate
	DistanceM            float64
	DurationSeconds      float64
	Geometry             GeoJSONLine
	PavedPercent         *float64
	UnpavedPercent       *float64
	UnknownPercent       *float64
	UnrunPercent         *float64
	PreviouslyRunPercent *float64
	AvoidedRoadDistanceM *float64
	Segments             []RouteSegment
	Provider             string
	Warnings             []string
}

type RouteSegment struct {
	FromIndex int     `json:"fromIndex"`
	ToIndex   int     `json:"toIndex"`
	OSMWayID  int64   `json:"osmWayId,omitempty"`
	Name      string  `json:"name,omitempty"`
	DistanceM float64 `json:"distanceM"`
	Surface   string  `json:"surface,omitempty"`
}
