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
