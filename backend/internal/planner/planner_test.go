package planner

import "testing"

func TestRouteScorePrioritizesPavedThresholdOverExtraDistance(t *testing.T) {
	targetM := 10000.0
	paved := 90.0
	unpaved := 70.0

	longerPavedRoute := CandidateRoute{
		DistanceM:    12000,
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

func TestRouteScorePrefersCloserPavedMatch(t *testing.T) {
	targetM := 10000.0
	closeMatch := 74.0
	overPaved := 100.0

	routeNearRequestedPavedTarget := CandidateRoute{
		DistanceM:    11000,
		PavedPercent: &closeMatch,
	}
	routeFarAboveRequestedPavedTarget := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &overPaved,
	}

	if routeScore(routeNearRequestedPavedTarget, targetM, 70) >= routeScore(routeFarAboveRequestedPavedTarget, targetM, 70) {
		t.Fatal("expected route scoring to prefer the route closer to the requested paved percentage")
	}
}

func TestGoodEnoughRequiresPavedMatchWithinFivePercentagePoints(t *testing.T) {
	targetM := 10000.0
	closeMatch := 74.0
	farMatch := 80.5

	closeRoute := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &closeMatch,
	}
	farRoute := CandidateRoute{
		DistanceM:    10500,
		PavedPercent: &farMatch,
	}

	if !isGoodEnough(closeRoute, targetM, 70) {
		t.Fatal("expected route within five paved percentage points to be good enough")
	}
	if isGoodEnough(farRoute, targetM, 70) {
		t.Fatal("expected route outside five paved percentage points not to be good enough")
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
		DistanceM:    11000,
		PavedPercent: &paved,
	}

	if routeScore(shortRoute, targetM, 90) <= routeScore(longRoute, targetM, 90) {
		t.Fatal("expected route scoring to prefer a longer route over a route shorter than requested")
	}
}
