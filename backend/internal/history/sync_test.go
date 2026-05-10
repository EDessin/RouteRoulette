package history

import (
	"context"
	"testing"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/EDessin/RouteRoulette/backend/internal/strava"
)

type fakeStravaAPI struct {
	activities  [][]strava.Activity
	streamCalls map[int64]int
}

func (api *fakeStravaAPI) ListActivities(_ context.Context, page int, _ int) ([]strava.Activity, error) {
	if page < 1 || page > len(api.activities) {
		return nil, nil
	}
	return api.activities[page-1], nil
}

func (api *fakeStravaAPI) ActivityLatLngStream(_ context.Context, id int64) ([]planner.Coordinate, error) {
	api.streamCalls[id]++
	return []planner.Coordinate{
		{Lat: 50.0, Lon: 4.0},
		{Lat: 50.1, Lon: 4.1},
	}, nil
}

func TestSyncStravaSkipsAlreadySyncedActivities(t *testing.T) {
	store := NewStore(t.TempDir())
	api := &fakeStravaAPI{
		activities: [][]strava.Activity{
			{
				{ID: 2, Name: "New run", SportType: "Run", StartDate: "2026-05-02T10:00:00Z", DistanceM: 6000},
				{ID: 1, Name: "Old run", SportType: "Run", StartDate: "2026-05-01T10:00:00Z", DistanceM: 5000},
			},
		},
		streamCalls: make(map[int64]int),
	}

	first, err := SyncStrava(context.Background(), api, &store)
	if err != nil {
		t.Fatalf("SyncStrava() returned error: %v", err)
	}
	if first.SyncedActivities != 2 || api.streamCalls[1] != 1 || api.streamCalls[2] != 1 {
		t.Fatalf("first sync = %+v, stream calls = %+v; want 2 synced and one stream call each", first, api.streamCalls)
	}

	second, err := SyncStrava(context.Background(), api, &store)
	if err != nil {
		t.Fatalf("second SyncStrava() returned error: %v", err)
	}
	if second.SyncedActivities != 0 || second.SkippedActivities != 2 {
		t.Fatalf("second sync = %+v, want 0 synced and 2 skipped", second)
	}
	if api.streamCalls[1] != 1 || api.streamCalls[2] != 1 {
		t.Fatalf("stream calls after second sync = %+v, want no refetch", api.streamCalls)
	}
}

func TestSyncStravaIgnoresNonRunActivities(t *testing.T) {
	store := NewStore(t.TempDir())
	api := &fakeStravaAPI{
		activities: [][]strava.Activity{
			{
				{ID: 1, Name: "Ride", SportType: "Ride", StartDate: "2026-05-01T10:00:00Z", DistanceM: 5000},
			},
		},
		streamCalls: make(map[int64]int),
	}

	result, err := SyncStrava(context.Background(), api, &store)
	if err != nil {
		t.Fatalf("SyncStrava() returned error: %v", err)
	}
	if result.SyncedActivities != 0 || len(api.streamCalls) != 0 {
		t.Fatalf("sync result = %+v, stream calls = %+v; want non-runs ignored", result, api.streamCalls)
	}
}
