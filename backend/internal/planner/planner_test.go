package planner

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

var errTestProvider = errors.New("test provider failed")

type failingProvider struct {
	calls *int
}

func (p failingProvider) GenerateRoundTrip(_ *http.Request, _ CandidateRequest) (CandidateRoute, error) {
	(*p.calls)++
	return CandidateRoute{}, errTestProvider
}

type singleAttemptProvider struct {
	failingProvider
}

func (p singleAttemptProvider) PlannerAttempts() int {
	return 1
}

func TestRouteScorePrioritizesPavedThresholdOverExtraDistance(t *testing.T) {
	targetM := 10000.0
	paved := 90.0
	unpaved := 70.0

	longerPavedRoute := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &paved,
	}
	shorterUnpavedRoute := CandidateRoute{
		DistanceM:    10000,
		PavedPercent: &unpaved,
	}

	if routeScore(longerPavedRoute, targetM, 90) >= routeScore(shorterUnpavedRoute, targetM, 90) {
		t.Fatal("expected route scoring to prefer the longer route that satisfies the paved threshold")
	}
}

func TestBestCandidateUsesDefaultProviderAttemptCount(t *testing.T) {
	calls := 0
	planner := Planner{provider: failingProvider{calls: &calls}}

	_, err := planner.bestCandidate(testHTTPRequest(), testGenerateRouteRequest(), 5000, 1)
	if err == nil {
		t.Fatal("expected failing provider to return an error")
	}
	if calls != 10 {
		t.Fatalf("provider calls = %d, want 10", calls)
	}
}

func TestBestCandidateUsesProviderAttemptOverride(t *testing.T) {
	calls := 0
	planner := Planner{provider: singleAttemptProvider{failingProvider{calls: &calls}}}

	_, err := planner.bestCandidate(testHTTPRequest(), testGenerateRouteRequest(), 5000, 1)
	if err == nil {
		t.Fatal("expected failing provider to return an error")
	}
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

func testHTTPRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/routes", nil)
}

func testGenerateRouteRequest() GenerateRouteRequest {
	return GenerateRouteRequest{
		Home:             Coordinate{Lat: 50.9950381, Lon: 4.7699273},
		TargetDistanceKm: 5,
		PreferPaved:      true,
		MinPavedPercent:  100,
	}
}

func TestRouteScorePrefersMorePavedRoadsAboveMinimum(t *testing.T) {
	targetM := 10000.0
	meetsMinimum := 82.0
	morePaved := 100.0

	routeThatMeetsMinimum := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &meetsMinimum,
	}
	routeWithMorePavedRoads := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &morePaved,
	}

	if routeScore(routeWithMorePavedRoads, targetM, 80) >= routeScore(routeThatMeetsMinimum, targetM, 80) {
		t.Fatal("expected route scoring to prefer more paved roads above the minimum")
	}
}

func TestGoodEnoughAllowsRoutesAbovePavedMinimum(t *testing.T) {
	targetM := 10000.0
	aboveMinimum := 95.0
	belowMinimum := 74.0

	aboveMinimumRoute := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &aboveMinimum,
	}
	belowMinimumRoute := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &belowMinimum,
	}

	if !isGoodEnough(aboveMinimumRoute, targetM, 80) {
		t.Fatal("expected route above the paved minimum to be good enough")
	}
	if isGoodEnough(belowMinimumRoute, targetM, 80) {
		t.Fatal("expected route more than five percentage points below the paved minimum not to be good enough")
	}
}

func TestRouteScorePenalizesRoutesMoreThanHalfKilometerLong(t *testing.T) {
	targetM := 10000.0
	paved := 70.0

	withinLimit := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &paved,
	}
	overLimit := CandidateRoute{
		DistanceM:    10600,
		PavedPercent: &paved,
	}

	if routeScore(withinLimit, targetM, 70) >= routeScore(overLimit, targetM, 70) {
		t.Fatal("expected route scoring to penalize routes more than 0.5 km longer than requested")
	}
}

func TestRouteScorePenalizesShortRoutes(t *testing.T) {
	targetM := 10000.0
	paved := 95.0

	shortRoute := CandidateRoute{
		DistanceM:    9500,
		PavedPercent: &paved,
	}
	longRoute := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &paved,
	}

	if routeScore(shortRoute, targetM, 90) <= routeScore(longRoute, targetM, 90) {
		t.Fatal("expected route scoring to prefer a longer route over a route shorter than requested")
	}
}

func TestValidateRejectsUnknownSurfacePolicy(t *testing.T) {
	req := testGenerateRouteRequest()
	req.SurfacePolicy = "guessy"

	if err := validate(req); err == nil {
		t.Fatal("expected unknown surface policy to be rejected")
	}
}

func TestValidateAllowsAssumePavedSurfacePolicy(t *testing.T) {
	req := testGenerateRouteRequest()
	req.SurfacePolicy = "assume_paved"

	if err := validate(req); err != nil {
		t.Fatalf("validate() returned error: %v", err)
	}
}
