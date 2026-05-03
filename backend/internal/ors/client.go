package ors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewClient(baseURL string, apiKey string, timeout time.Duration) Client {
	return Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c Client) SearchAddress(ctxReq *http.Request, text string) (planner.GeocodeResponse, error) {
	trimmed := strings.TrimSpace(text)
	if coordinate, ok := parseCoordinateText(trimmed); ok {
		return planner.GeocodeResponse{
			Label: trimmed,
			Home:  coordinate,
		}, nil
	}

	if c.apiKey == "" {
		return planner.GeocodeResponse{}, fmt.Errorf("ORS_API_KEY is required to search addresses")
	}

	endpoint, err := url.Parse(c.baseURL + "/geocode/search")
	if err != nil {
		return planner.GeocodeResponse{}, err
	}

	query := endpoint.Query()
	query.Set("api_key", c.apiKey)
	query.Set("text", trimmed)
	query.Set("size", "1")
	endpoint.RawQuery = query.Encode()

	httpReq, err := http.NewRequestWithContext(ctxReq.Context(), http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return planner.GeocodeResponse{}, err
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return planner.GeocodeResponse{}, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return planner.GeocodeResponse{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return planner.GeocodeResponse{}, fmt.Errorf("openrouteservice geocoding returned %s: %s", resp.Status, string(responseBody))
	}

	var parsed orsGeocodeResponse
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return planner.GeocodeResponse{}, err
	}
	if len(parsed.Features) == 0 {
		return planner.GeocodeResponse{}, fmt.Errorf("no location found for that address")
	}

	feature := parsed.Features[0]
	if len(feature.Geometry.Coordinates) < 2 {
		return planner.GeocodeResponse{}, fmt.Errorf("geocoding result did not include coordinates")
	}

	label := feature.Properties.Label
	if label == "" {
		label = trimmed
	}

	return planner.GeocodeResponse{
		Label: label,
		Home: planner.Coordinate{
			Lat: feature.Geometry.Coordinates[1],
			Lon: feature.Geometry.Coordinates[0],
		},
	}, nil
}

func (c Client) GenerateRoundTrip(ctxReq *http.Request, req planner.CandidateRequest) (planner.CandidateRoute, error) {
	if c.apiKey == "" {
		return planner.CandidateRoute{}, fmt.Errorf("ORS_API_KEY is not configured")
	}

	body := orsRouteRequest{
		Coordinates:  [][]float64{{req.Start.Lon, req.Start.Lat}},
		Instructions: false,
		Geometry:     true,
		Units:        "m",
		Options: orsOptions{
			RoundTrip: orsRoundTrip{
				Length: req.TargetDistanceM,
				Points: 4,
				Seed:   req.Seed,
			},
			AvoidFeatures: []string{"ferries", "fords", "steps"},
		},
		ExtraInfo: []string{"surface", "waytype"},
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return planner.CandidateRoute{}, err
	}

	endpoint := c.baseURL + "/v2/directions/foot-walking/geojson"
	httpReq, err := http.NewRequestWithContext(ctxReq.Context(), http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return planner.CandidateRoute{}, err
	}

	httpReq.Header.Set("Authorization", c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("Accept", "application/geo+json, application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return planner.CandidateRoute{}, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return planner.CandidateRoute{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return planner.CandidateRoute{}, fmt.Errorf("openrouteservice returned %s: %s", resp.Status, string(responseBody))
	}

	var parsed orsGeoJSONResponse
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return planner.CandidateRoute{}, err
	}
	if len(parsed.Features) == 0 {
		return planner.CandidateRoute{}, fmt.Errorf("openrouteservice returned no route features")
	}

	feature := parsed.Features[0]
	if feature.Geometry.Type != "LineString" || len(feature.Geometry.Coordinates) == 0 {
		return planner.CandidateRoute{}, fmt.Errorf("openrouteservice returned an unsupported geometry")
	}

	pavedPercent := pavedPercentFromExtras(feature.Properties.Extras)
	warnings := []string{}
	if (req.PreferPaved || req.MinPavedPercent > 0) && pavedPercent == nil {
		warnings = append(warnings, "Surface data was not available for paved-route scoring.")
	}

	return planner.CandidateRoute{
		Start:           req.Start,
		DistanceM:       feature.Properties.Summary.Distance,
		DurationSeconds: feature.Properties.Summary.Duration,
		Geometry: planner.GeoJSONLine{
			Type:        "LineString",
			Coordinates: feature.Geometry.Coordinates,
		},
		PavedPercent: pavedPercent,
		Provider:     "openrouteservice",
		Warnings:     warnings,
	}, nil
}

type orsRouteRequest struct {
	Coordinates  [][]float64 `json:"coordinates"`
	Instructions bool        `json:"instructions"`
	Geometry     bool        `json:"geometry"`
	Units        string      `json:"units"`
	Options      orsOptions  `json:"options"`
	ExtraInfo    []string    `json:"extra_info,omitempty"`
}

type orsOptions struct {
	RoundTrip     orsRoundTrip `json:"round_trip"`
	AvoidFeatures []string     `json:"avoid_features,omitempty"`
}

type orsRoundTrip struct {
	Length float64 `json:"length"`
	Points int     `json:"points"`
	Seed   int64   `json:"seed,omitempty"`
}

type orsGeoJSONResponse struct {
	Features []struct {
		Geometry struct {
			Type        string      `json:"type"`
			Coordinates [][]float64 `json:"coordinates"`
		} `json:"geometry"`
		Properties struct {
			Summary struct {
				Distance float64 `json:"distance"`
				Duration float64 `json:"duration"`
			} `json:"summary"`
			Extras map[string]json.RawMessage `json:"extras"`
		} `json:"properties"`
	} `json:"features"`
}

type orsGeocodeResponse struct {
	Features []struct {
		Geometry struct {
			Coordinates []float64 `json:"coordinates"`
		} `json:"geometry"`
		Properties struct {
			Label string `json:"label"`
		} `json:"properties"`
	} `json:"features"`
}

func parseCoordinateText(text string) (planner.Coordinate, bool) {
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	})
	values := make([]string, 0, 2)
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	if len(values) != 2 {
		return planner.Coordinate{}, false
	}

	lat, latErr := strconv.ParseFloat(values[0], 64)
	lon, lonErr := strconv.ParseFloat(values[1], 64)
	if latErr != nil || lonErr != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return planner.Coordinate{}, false
	}

	return planner.Coordinate{Lat: lat, Lon: lon}, true
}

func pavedPercentFromExtras(extras map[string]json.RawMessage) *float64 {
	if len(extras) == 0 {
		return nil
	}

	rawSurface, ok := extras["surface"]
	if !ok {
		return nil
	}

	var surface struct {
		Summary []struct {
			Value    any     `json:"value"`
			Distance float64 `json:"distance"`
			Amount   float64 `json:"amount"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rawSurface, &surface); err != nil || len(surface.Summary) == 0 {
		return nil
	}

	total := 0.0
	paved := 0.0
	for _, item := range surface.Summary {
		total += item.Distance
		if isPavedSurface(item.Value) {
			paved += item.Distance
		}
	}

	if total == 0 {
		return nil
	}
	value := (paved / total) * 100
	return &value
}

func isPavedSurface(value any) bool {
	switch v := value.(type) {
	case string:
		normalized := strings.ToLower(v)
		return strings.Contains(normalized, "paved") ||
			strings.Contains(normalized, "asphalt") ||
			strings.Contains(normalized, "concrete") ||
			strings.Contains(normalized, "cobblestone") ||
			strings.Contains(normalized, "paving")
	case float64:
		switch int(v) {
		case 1, 3, 4, 5, 6, 7, 14, 18:
			return true
		default:
			return false
		}
	default:
		return false
	}
}
