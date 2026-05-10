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

type plannerAttemptProvider interface {
	PlannerAttempts() int
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
			return routeResponse(mock, req, []string{
				"Using a mock route because the routing provider is not configured or unavailable.",
			}), nil
		}
		return RouteResponse{}, fmt.Errorf("%w: %v", ErrRouteUnavailable, err)
	}

	return routeResponse(best, req, nil), nil
}

func (p Planner) bestCandidate(r *http.Request, req GenerateRouteRequest, targetM float64, seed int64) (CandidateRoute, error) {
	attempts := p.providerAttempts()

	lengthMultipliers := []float64{1, 1.02, 1.04, 1.06, 1.08}

	var best CandidateRoute
	bestScore := math.MaxFloat64
	var lastErr error

	for i := 0; i < attempts; i++ {
		candidateSeed := seed + int64(i*7919)
		requestedDistanceM := math.Min(targetM+500, targetM*lengthMultipliers[i%len(lengthMultipliers)])
		start := req.Home
		if i > 0 && req.MaxStartDistanceKm > 0 {
			start = randomPointWithin(req.Home, req.MaxStartDistanceKm, candidateSeed)
		}

		route, err := p.provider.GenerateRoundTrip(r, CandidateRequest{
			Start:            start,
			Home:             req.Home,
			TargetDistanceM:  requestedDistanceM,
			PreferPaved:      req.PreferPaved,
			MinPavedPercent:  req.MinPavedPercent,
			SurfacePolicy:    req.SurfacePolicy,
			PreferUnrunRoads: req.PreferUnrunRoads,
			Seed:             candidateSeed,
		})
		if err != nil {
			lastErr = err
			continue
		}

		score := routeScore(route, targetM, req.MinPavedPercent)
		if score < bestScore {
			best = route
			bestScore = score
		}

		if isGoodEnough(route, targetM, req.MinPavedPercent) {
			return route, nil
		}
	}

	if best.Geometry.Type != "" {
		return best, nil
	}

	return CandidateRoute{}, lastErr
}

func (p Planner) providerAttempts() int {
	const defaultAttempts = 10

	attemptProvider, ok := p.provider.(plannerAttemptProvider)
	if !ok {
		return defaultAttempts
	}
	attempts := attemptProvider.PlannerAttempts()
	if attempts <= 0 {
		return defaultAttempts
	}
	return attempts
}

func routeScore(route CandidateRoute, targetM float64, minPavedPercent float64) float64 {
	pavedShortfall := pavedShortfallRatio(route, minPavedPercent)
	pavedDifference := pavedDifferenceRatio(route, minPavedPercent)
	shortRoutePenalty := math.Max(0, targetM-route.DistanceM) / targetM
	extraMeters := math.Max(0, route.DistanceM-targetM)
	extraDistancePenalty := extraMeters / targetM
	if extraMeters > 500 {
		extraDistancePenalty += (extraMeters - 500) / 10
	}

	if minPavedPercent > 0 {
		return pavedShortfall*120 + pavedDifference*30 + shortRoutePenalty*50 + extraDistancePenalty
	}

	return shortRoutePenalty*50 + extraDistancePenalty
}

func pavedShortfallRatio(route CandidateRoute, minPavedPercent float64) float64 {
	if minPavedPercent <= 0 || route.PavedPercent == nil {
		return 0
	}
	return math.Max(0, minPavedPercent-*route.PavedPercent) / 100
}

func pavedDifferenceRatio(route CandidateRoute, minPavedPercent float64) float64 {
	if minPavedPercent <= 0 || route.PavedPercent == nil {
		return 0
	}
	return math.Abs(minPavedPercent-*route.PavedPercent) / 100
}

func pavedDifferencePercent(route CandidateRoute, minPavedPercent float64) float64 {
	if minPavedPercent <= 0 || route.PavedPercent == nil {
		return 0
	}
	return math.Abs(minPavedPercent - *route.PavedPercent)
}

func isGoodEnough(route CandidateRoute, targetM float64, minPavedPercent float64) bool {
	return route.DistanceM >= targetM && pavedDifferencePercent(route, minPavedPercent) <= 5
}

func routeResponse(route CandidateRoute, req GenerateRouteRequest, warnings []string) RouteResponse {
	allWarnings := append([]string{}, route.Warnings...)
	allWarnings = append(allWarnings, warnings...)

	actualKm := route.DistanceM / 1000
	if actualKm < req.TargetDistanceKm {
		allWarnings = append(allWarnings, "The best route found is shorter than requested. Try lowering the minimum paved percentage or increasing the start radius.")
	} else if actualKm > req.TargetDistanceKm+0.5 {
		allWarnings = append(allWarnings, "The generated route is more than 0.5 km longer than requested.")
	}
	if req.MinPavedPercent > 0 && route.PavedPercent == nil {
		allWarnings = append(allWarnings, "Surface data was not available, so the minimum paved percentage could not be verified.")
	}
	if req.MinPavedPercent > 0 && route.PavedPercent != nil && *route.PavedPercent < req.MinPavedPercent {
		allWarnings = append(allWarnings, fmt.Sprintf("The best route found is %.0f%% paved, below your %.0f%% minimum.", *route.PavedPercent, req.MinPavedPercent))
	}
	if req.MinPavedPercent > 0 && route.PavedPercent != nil && pavedDifferencePercent(route, req.MinPavedPercent) > 5 {
		allWarnings = append(allWarnings, fmt.Sprintf("The best route found differs from your paved-road target by %.0f percentage points.", pavedDifferencePercent(route, req.MinPavedPercent)))
	}

	durationMinutes := route.DurationSeconds / 60
	if req.EstimatedPaceMinPerKm != nil {
		durationMinutes = actualKm * *req.EstimatedPaceMinPerKm
	}

	return RouteResponse{
		RouteID:               fmt.Sprintf("%d", time.Now().UnixNano()),
		Start:                 route.Start,
		DistanceKm:            round(actualKm, 2),
		DurationMinutes:       round(durationMinutes, 1),
		Geometry:              route.Geometry,
		PavedPercent:          route.PavedPercent,
		UnpavedPercent:        route.UnpavedPercent,
		UnknownSurfacePercent: route.UnknownPercent,
		UnrunPercent:          route.UnrunPercent,
		PreviouslyRunPercent:  route.PreviouslyRunPercent,
		Provider:              route.Provider,
		Warnings:              allWarnings,
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
	if req.EstimatedPaceMinPerKm != nil && (*req.EstimatedPaceMinPerKm < 2 || *req.EstimatedPaceMinPerKm > 20) {
		return fmt.Errorf("%w: estimatedPaceMinPerKm must be between 2 and 20", ErrInvalidRequest)
	}
	if req.MinPavedPercent < 0 || req.MinPavedPercent > 100 {
		return fmt.Errorf("%w: minPavedPercent must be between 0 and 100", ErrInvalidRequest)
	}
	if req.SurfacePolicy != "" && req.SurfacePolicy != "strict" && req.SurfacePolicy != "assume_paved" {
		return fmt.Errorf("%w: surfacePolicy must be strict or assume_paved", ErrInvalidRequest)
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
		PavedPercent: mockPavedPercent(req),
		Provider:     "mock",
		Warnings: []string{
			"Mock geometry is for local UI testing only; set ORS_API_KEY for real paved-road routing.",
		},
	}
}

func mockPavedPercent(req GenerateRouteRequest) *float64 {
	value := math.Max(req.MinPavedPercent, 80)
	if value > 100 {
		value = 100
	}
	return &value
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
