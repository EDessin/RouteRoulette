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

const lowKnownSurfaceDataWarningThreshold = 20.0
const preferredKnownPavedTarget = 95.0
const preferredKnownUnpavedTarget = 75.0

type RouteProvider interface {
	GenerateRoundTrip(ctxReq *http.Request, req CandidateRequest) (CandidateRoute, error)
}

type RouteImporter interface {
	ImportRoute(ctxReq *http.Request, req ImportRouteRequest) (CandidateRoute, error)
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

func (p Planner) ImportRoute(r *http.Request, req ImportRouteRequest) (RouteResponse, error) {
	if err := validateImport(req); err != nil {
		return RouteResponse{}, err
	}

	importer, ok := p.provider.(RouteImporter)
	if !ok {
		return RouteResponse{}, fmt.Errorf("%w: route provider cannot import GPX routes", ErrRouteUnavailable)
	}

	route, err := importer.ImportRoute(r, req)
	if err != nil {
		return RouteResponse{}, fmt.Errorf("%w: %v", ErrRouteUnavailable, err)
	}

	responseReq := GenerateRouteRequest{
		TargetDistanceKm:      route.DistanceM / 1000,
		EstimatedPaceMinPerKm: req.EstimatedPaceMinPerKm,
	}
	response := routeResponse(route, responseReq, nil)
	response.Provider = route.Provider
	return response, nil
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
			PreferUnpaved:    req.PreferUnpaved,
			MinPavedPercent:  req.MinPavedPercent,
			SurfacePolicy:    req.SurfacePolicy,
			PreferUnrunRoads: req.PreferUnrunRoads,
			Seed:             candidateSeed,
		})
		if err != nil {
			lastErr = err
			continue
		}

		score := routeScore(route, targetM, req.MinPavedPercent, req.PreferUnpaved)
		if score < bestScore {
			best = route
			bestScore = score
		}

		if isGoodEnough(route, targetM, req.MinPavedPercent, req.PreferUnpaved) {
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

func routeScore(route CandidateRoute, targetM float64, minPavedPercent float64, preferUnpaved bool) float64 {
	surfaceTarget := preferredKnownSurfaceTarget(minPavedPercent, preferUnpaved)
	surfaceShortfall := preferredKnownSurfaceShortfallPercent(route, minPavedPercent, preferUnpaved)
	surfaceGapToPerfect := preferredKnownSurfaceGapToPerfectPercent(route, minPavedPercent, preferUnpaved)
	shortRoutePenalty := math.Max(0, targetM-route.DistanceM) / targetM
	extraMeters := math.Max(0, route.DistanceM-targetM)
	extraDistancePenalty := extraMeters / targetM
	if extraMeters > 500 {
		extraDistancePenalty += (extraMeters - 500) / 10
	}

	if surfaceTarget > 0 {
		return surfaceShortfall*2 + surfaceGapToPerfect*0.2 + shortRoutePenalty*50 + extraDistancePenalty
	}

	return shortRoutePenalty*50 + extraDistancePenalty
}

func pavedShortfallRatio(route CandidateRoute, minPavedPercent float64) float64 {
	if minPavedPercent <= 0 || route.PavedPercent == nil {
		return 0
	}
	return math.Max(0, minPavedPercent-*route.PavedPercent) / 100
}

func pavedGapToPerfectRatio(route CandidateRoute, minPavedPercent float64) float64 {
	if minPavedPercent <= 0 || route.PavedPercent == nil {
		return 0
	}
	return math.Max(0, 100-*route.PavedPercent) / 100
}

func pavedShortfallPercent(route CandidateRoute, minPavedPercent float64) float64 {
	if minPavedPercent <= 0 || route.PavedPercent == nil {
		return 0
	}
	return math.Max(0, minPavedPercent-*route.PavedPercent)
}

func preferredKnownSurfaceTarget(minPavedPercent float64, preferUnpaved bool) float64 {
	if preferUnpaved {
		return preferredKnownUnpavedTarget
	}
	if minPavedPercent > 0 {
		return math.Max(preferredKnownPavedTarget, minPavedPercent)
	}
	return 0
}

func hasSurfacePreference(req GenerateRouteRequest) bool {
	return req.PreferPaved || req.PreferUnpaved
}

func preferredKnownSurfacePercent(route CandidateRoute, minPavedPercent float64, preferUnpaved bool) *float64 {
	if preferUnpaved {
		if route.KnownUnpavedPercent != nil {
			return route.KnownUnpavedPercent
		}
		return route.UnpavedPercent
	}
	if minPavedPercent > 0 {
		if route.KnownPavedPercent != nil {
			return route.KnownPavedPercent
		}
		return route.PavedPercent
	}
	return nil
}

func preferredKnownSurfaceShortfallPercent(route CandidateRoute, minPavedPercent float64, preferUnpaved bool) float64 {
	target := preferredKnownSurfaceTarget(minPavedPercent, preferUnpaved)
	percent := preferredKnownSurfacePercent(route, minPavedPercent, preferUnpaved)
	if target <= 0 || percent == nil {
		return 0
	}
	if route.KnownSurfacePercent != nil && *route.KnownSurfacePercent < lowKnownSurfaceDataWarningThreshold {
		return 0
	}
	return math.Max(0, target-*percent)
}

func preferredKnownSurfaceGapToPerfectPercent(route CandidateRoute, minPavedPercent float64, preferUnpaved bool) float64 {
	target := preferredKnownSurfaceTarget(minPavedPercent, preferUnpaved)
	percent := preferredKnownSurfacePercent(route, minPavedPercent, preferUnpaved)
	if target <= 0 || percent == nil {
		return 0
	}
	if route.KnownSurfacePercent != nil && *route.KnownSurfacePercent < lowKnownSurfaceDataWarningThreshold {
		return 0
	}
	return math.Max(0, 100-*percent)
}

func isGoodEnough(route CandidateRoute, targetM float64, minPavedPercent float64, preferUnpaved bool) bool {
	target := preferredKnownSurfaceTarget(minPavedPercent, preferUnpaved)
	if target > 0 {
		return route.DistanceM >= targetM && preferredKnownSurfaceShortfallPercent(route, minPavedPercent, preferUnpaved) <= 0
	}
	return route.DistanceM >= targetM && pavedShortfallPercent(route, minPavedPercent) <= 5
}

func routeResponse(route CandidateRoute, req GenerateRouteRequest, warnings []string) RouteResponse {
	allWarnings := append([]string{}, route.Warnings...)
	allWarnings = append(allWarnings, warnings...)

	actualKm := route.DistanceM / 1000
	if actualKm < req.TargetDistanceKm {
		allWarnings = append(allWarnings, "The best route found is shorter than requested. Try turning off Prefer paved roads or choosing a shorter route.")
	} else if actualKm > req.TargetDistanceKm+0.5 {
		allWarnings = append(allWarnings, "The generated route is more than 0.5 km longer than requested.")
	}
	if req.MinPavedPercent > 0 && preferredKnownSurfacePercent(route, req.MinPavedPercent, false) == nil {
		allWarnings = append(allWarnings, "Surface data was not available, so the paved-road preference could not be verified.")
	}
	limitedKnownSurface := route.KnownSurfacePercent != nil && *route.KnownSurfacePercent < lowKnownSurfaceDataWarningThreshold
	if hasSurfacePreference(req) && limitedKnownSurface {
		allWarnings = append(allWarnings, fmt.Sprintf("Only %.0f%% of this route has known surface data, so the surface preference is based on limited OSM and manual road markings.", *route.KnownSurfacePercent))
	}
	if req.PreferPaved {
		if percent := preferredKnownSurfacePercent(route, req.MinPavedPercent, false); !limitedKnownSurface && percent != nil && *percent < preferredKnownSurfaceTarget(req.MinPavedPercent, false) {
			allWarnings = append(allWarnings, fmt.Sprintf("The best route found is %.0f%% paved across roads with known surface data, below your %.0f%% target.", *percent, preferredKnownSurfaceTarget(req.MinPavedPercent, false)))
		}
	}
	if req.PreferUnpaved {
		if percent := preferredKnownSurfacePercent(route, req.MinPavedPercent, true); !limitedKnownSurface && percent != nil && *percent < preferredKnownSurfaceTarget(req.MinPavedPercent, true) {
			allWarnings = append(allWarnings, fmt.Sprintf("The best route found is %.0f%% unpaved across roads with known surface data, below your %.0f%% target.", *percent, preferredKnownSurfaceTarget(req.MinPavedPercent, true)))
		}
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
		KnownSurfacePercent:   route.KnownSurfacePercent,
		KnownPavedPercent:     route.KnownPavedPercent,
		KnownUnpavedPercent:   route.KnownUnpavedPercent,
		UnrunPercent:          route.UnrunPercent,
		PreviouslyRunPercent:  route.PreviouslyRunPercent,
		AvoidedRoadDistanceM:  route.AvoidedRoadDistanceM,
		Segments:              route.Segments,
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
	if req.PreferPaved && req.PreferUnpaved {
		return fmt.Errorf("%w: preferPaved and preferUnpaved cannot both be enabled", ErrInvalidRequest)
	}
	if req.PreferUnpaved && req.MinPavedPercent > 0 {
		return fmt.Errorf("%w: minPavedPercent must be 0 when preferUnpaved is enabled", ErrInvalidRequest)
	}
	if req.SurfacePolicy != "" && req.SurfacePolicy != "strict" && req.SurfacePolicy != "assume_paved" {
		return fmt.Errorf("%w: surfacePolicy must be strict or assume_paved", ErrInvalidRequest)
	}
	return nil
}

func validateImport(req ImportRouteRequest) error {
	if len(req.Coordinates) < 2 {
		return fmt.Errorf("%w: imported GPX route must contain at least two track points", ErrInvalidRequest)
	}
	if len(req.Coordinates) > 10000 {
		return fmt.Errorf("%w: imported GPX route contains too many track points", ErrInvalidRequest)
	}
	for _, coord := range req.Coordinates {
		if coord.Lat < -90 || coord.Lat > 90 || coord.Lon < -180 || coord.Lon > 180 {
			return fmt.Errorf("%w: imported GPX contains coordinates outside the valid latitude/longitude range", ErrInvalidRequest)
		}
	}
	if req.EstimatedPaceMinPerKm != nil && (*req.EstimatedPaceMinPerKm < 2 || *req.EstimatedPaceMinPerKm > 20) {
		return fmt.Errorf("%w: estimatedPaceMinPerKm must be between 2 and 20", ErrInvalidRequest)
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
		PavedPercent:        mockPavedPercent(req),
		UnpavedPercent:      mockUnpavedPercent(req),
		KnownSurfacePercent: mockKnownSurfacePercent(),
		KnownPavedPercent:   mockPavedPercent(req),
		KnownUnpavedPercent: mockUnpavedPercent(req),
		Provider:            "mock",
		Warnings: []string{
			"Mock geometry is for local UI testing only; set ORS_API_KEY for real paved-road routing.",
		},
	}
}

func mockPavedPercent(req GenerateRouteRequest) *float64 {
	if req.PreferUnpaved {
		value := 35.0
		return &value
	}
	value := math.Max(req.MinPavedPercent, 80)
	if value > 100 {
		value = 100
	}
	return &value
}

func mockUnpavedPercent(req GenerateRouteRequest) *float64 {
	value := 100 - *mockPavedPercent(req)
	return &value
}

func mockKnownSurfacePercent() *float64 {
	value := 100.0
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
