package planner

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	mathrand "math/rand"
	"net/http"
	"time"
)

type RouteProvider interface {
	GenerateRoundTrip(ctxReq *http.Request, req CandidateRequest) (CandidateRoute, error)
}

type Planner struct {
	provider        RouteProvider
	allowMockRoutes bool
}

func New(provider RouteProvider, allowMockRoutes bool) Planner {
	return Planner{
		provider:        provider,
		allowMockRoutes: allowMockRoutes,
	}
}

func (p Planner) Generate(r *http.Request, req GenerateRouteRequest) (RouteResponse, error) {
	if err := validate(req); err != nil {
		return RouteResponse{}, err
	}

	seed := requestSeed(req.Seed)
	targetM := req.TargetDistanceKm * 1000
	best, err := p.bestCandidate(r, req, targetM, seed)
	if err != nil {
		if p.allowMockRoutes {
			mock := buildMockRoute(req, targetM, seed)
			return routeResponse(mock, req.TargetDistanceKm, []string{
				"Using a mock route because the routing provider is not configured or unavailable.",
			}), nil
		}
		return RouteResponse{}, fmt.Errorf("%w: %v", ErrRouteUnavailable, err)
	}

	return routeResponse(best, req.TargetDistanceKm, nil), nil
}

func (p Planner) bestCandidate(r *http.Request, req GenerateRouteRequest, targetM float64, seed int64) (CandidateRoute, error) {
	const attempts = 8

	var best CandidateRoute
	bestScore := math.MaxFloat64
	var lastErr error

	for i := 0; i < attempts; i++ {
		candidateSeed := seed + int64(i*7919)
		start := req.Home
		if i > 0 && req.MaxStartDistanceKm > 0 {
			start = randomPointWithin(req.Home, req.MaxStartDistanceKm, candidateSeed)
		}

		route, err := p.provider.GenerateRoundTrip(r, CandidateRequest{
			Start:           start,
			TargetDistanceM: targetM,
			PreferPaved:     req.PreferPaved,
			Seed:            candidateSeed,
		})
		if err != nil {
			lastErr = err
			continue
		}

		score := routeScore(route, targetM, req.PreferPaved)
		if score < bestScore {
			best = route
			bestScore = score
		}

		if score <= 0.05 {
			return route, nil
		}
	}

	if best.Geometry.Type != "" {
		return best, nil
	}

	return CandidateRoute{}, lastErr
}

func routeScore(route CandidateRoute, targetM float64, preferPaved bool) float64 {
	distanceError := math.Abs(route.DistanceM-targetM) / targetM
	if preferPaved && route.PavedPercent != nil {
		return distanceError + ((100 - *route.PavedPercent) / 100)
	}
	return distanceError
}

func routeResponse(route CandidateRoute, targetDistanceKm float64, warnings []string) RouteResponse {
	allWarnings := append([]string{}, route.Warnings...)
	allWarnings = append(allWarnings, warnings...)

	actualKm := route.DistanceM / 1000
	if math.Abs(actualKm-targetDistanceKm)/targetDistanceKm > 0.10 {
		allWarnings = append(allWarnings, "The generated route is more than 10% away from the requested distance.")
	}

	return RouteResponse{
		RouteID:         fmt.Sprintf("%d", time.Now().UnixNano()),
		Start:           route.Start,
		DistanceKm:      round(actualKm, 2),
		DurationMinutes: round(route.DurationSeconds/60, 1),
		Geometry:        route.Geometry,
		PavedPercent:    route.PavedPercent,
		Provider:        route.Provider,
		Warnings:        allWarnings,
	}
}

func validate(req GenerateRouteRequest) error {
	if req.Home.Lat < -90 || req.Home.Lat > 90 || req.Home.Lon < -180 || req.Home.Lon > 180 {
		return fmt.Errorf("%w: home coordinates are outside the valid latitude/longitude range", ErrInvalidRequest)
	}
	if req.TargetDistanceKm < 1 || req.TargetDistanceKm > 100 {
		return fmt.Errorf("%w: targetDistanceKm must be between 1 and 100", ErrInvalidRequest)
	}
	if req.MaxStartDistanceKm < 0 || req.MaxStartDistanceKm > 25 {
		return fmt.Errorf("%w: maxStartDistanceKm must be between 0 and 25", ErrInvalidRequest)
	}
	return nil
}

func requestSeed(value *int64) int64 {
	if value != nil {
		return *value
	}

	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return int64(binary.LittleEndian.Uint64(buf[:]))
	}
	return time.Now().UnixNano()
}

func randomPointWithin(center Coordinate, radiusKm float64, seed int64) Coordinate {
	rng := mathrand.New(mathrand.NewSource(seed))
	distanceKm := radiusKm * math.Sqrt(rng.Float64())
	bearing := rng.Float64() * 2 * math.Pi

	earthRadiusKm := 6371.0
	lat1 := degreesToRadians(center.Lat)
	lon1 := degreesToRadians(center.Lon)
	angularDistance := distanceKm / earthRadiusKm

	lat2 := math.Asin(math.Sin(lat1)*math.Cos(angularDistance) + math.Cos(lat1)*math.Sin(angularDistance)*math.Cos(bearing))
	lon2 := lon1 + math.Atan2(math.Sin(bearing)*math.Sin(angularDistance)*math.Cos(lat1), math.Cos(angularDistance)-math.Sin(lat1)*math.Sin(lat2))

	return Coordinate{
		Lat: radiansToDegrees(lat2),
		Lon: radiansToDegrees(lon2),
	}
}

func buildMockRoute(req GenerateRouteRequest, targetM float64, seed int64) CandidateRoute {
	start := req.Home
	if req.MaxStartDistanceKm > 0 {
		start = randomPointWithin(req.Home, req.MaxStartDistanceKm, seed)
	}

	radiusKm := (targetM / 1000) / (2 * math.Pi)
	points := 96
	coordinates := make([][]float64, 0, points+1)

	for i := 0; i <= points; i++ {
		angle := 2 * math.Pi * float64(i) / float64(points)
		lat := start.Lat + (radiusKm*math.Cos(angle))/111.32
		lon := start.Lon + (radiusKm*math.Sin(angle))/(111.32*math.Cos(degreesToRadians(start.Lat)))
		coordinates = append(coordinates, []float64{lon, lat})
	}

	return CandidateRoute{
		Start:           start,
		DistanceM:       targetM,
		DurationSeconds: (targetM / 1000) * 360,
		Geometry: GeoJSONLine{
			Type:        "LineString",
			Coordinates: coordinates,
		},
		Provider: "mock",
		Warnings: []string{
			"Mock geometry is for local UI testing only; set ORS_API_KEY for real paved-road routing.",
		},
	}
}

func round(value float64, places int) float64 {
	pow := math.Pow(10, float64(places))
	return math.Round(value*pow) / pow
}

func degreesToRadians(value float64) float64 {
	return value * math.Pi / 180
}

func radiansToDegrees(value float64) float64 {
	return value * 180 / math.Pi
}
