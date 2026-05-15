package avoidance

import (
	"errors"
	"os"
	"testing"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

func TestStoreAddsListsAndDeletesAvoidedRoads(t *testing.T) {
	store := NewStore(t.TempDir())

	road, err := store.Add(AddRoadRequest{
		OSMWayID:   123,
		Name:       "Busy Lane",
		Reason:     ReasonBusyRoad,
		Coordinate: planner.Coordinate{Lat: 50.1, Lon: 4.7},
	})
	if err != nil {
		t.Fatalf("Add() returned error: %v", err)
	}
	if road.ID != "way:123" {
		t.Fatalf("road ID = %q, want way:123", road.ID)
	}

	roads, err := store.List()
	if err != nil {
		t.Fatalf("List() returned error: %v", err)
	}
	if len(roads) != 1 || roads[0].Name != "Busy Lane" {
		t.Fatalf("List() = %+v, want saved road", roads)
	}

	if err := store.Delete(road.ID); err != nil {
		t.Fatalf("Delete() returned error: %v", err)
	}
	roads, err = store.List()
	if err != nil {
		t.Fatalf("List() after delete returned error: %v", err)
	}
	if len(roads) != 0 {
		t.Fatalf("List() after delete = %+v, want empty list", roads)
	}
}

func TestStoreRejectsUnknownReasons(t *testing.T) {
	store := NewStore(t.TempDir())

	if _, err := store.Add(AddRoadRequest{OSMWayID: 123, Reason: "bad_reason"}); err == nil {
		t.Fatal("expected unknown reason to be rejected")
	}
}

func TestStoreDeleteMissingRoadReturnsNotExist(t *testing.T) {
	store := NewStore(t.TempDir())

	if err := store.Delete("way:missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete() err = %v, want os.ErrNotExist", err)
	}
}
