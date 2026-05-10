package history

import (
	"testing"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

func TestStoreSavesAndLoadsActivity(t *testing.T) {
	store := NewStore(t.TempDir())
	activity := Activity{
		ID:        123,
		StartDate: "2026-05-01T10:00:00Z",
		SportType: "Run",
		DistanceM: 5000,
		Coordinates: []planner.Coordinate{
			{Lat: 50.0, Lon: 4.0},
			{Lat: 50.1, Lon: 4.1},
		},
	}

	if err := store.SaveActivity(activity); err != nil {
		t.Fatalf("SaveActivity() returned error: %v", err)
	}
	exists, err := store.HasActivity(123)
	if err != nil {
		t.Fatalf("HasActivity() returned error: %v", err)
	}
	if !exists {
		t.Fatal("expected saved activity to exist")
	}
	activities, err := store.Activities()
	if err != nil {
		t.Fatalf("Activities() returned error: %v", err)
	}
	if len(activities) != 1 || activities[0].ID != 123 || len(activities[0].Coordinates) != 2 {
		t.Fatalf("Activities() = %+v, want saved activity with coordinates", activities)
	}
}

func TestStoreClearRemovesHistory(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SaveActivity(Activity{
		ID:        123,
		StartDate: "2026-05-01T10:00:00Z",
		SportType: "Run",
		Coordinates: []planner.Coordinate{
			{Lat: 50.0, Lon: 4.0},
			{Lat: 50.1, Lon: 4.1},
		},
	}); err != nil {
		t.Fatalf("SaveActivity() returned error: %v", err)
	}

	if err := store.Clear(); err != nil {
		t.Fatalf("Clear() returned error: %v", err)
	}
	status, err := store.Status(true)
	if err != nil {
		t.Fatalf("Status() returned error: %v", err)
	}
	if status.SyncedActivities != 0 {
		t.Fatalf("SyncedActivities = %d, want 0", status.SyncedActivities)
	}
}
