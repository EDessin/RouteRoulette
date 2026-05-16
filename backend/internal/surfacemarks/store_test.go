package surfacemarks

import "testing"

func TestStoreMarksListsAndDeletesRoadSurfaces(t *testing.T) {
	store := NewStore(t.TempDir())

	road, err := store.Mark(MarkRoadRequest{
		OSMWayID: 42,
		Name:     "Mystery Lane",
		Surface:  SurfacePaved,
	})
	if err != nil {
		t.Fatalf("Mark() returned error: %v", err)
	}
	if road.ID != "way:42" || road.Surface != SurfacePaved {
		t.Fatalf("Mark() = %+v, want paved way:42", road)
	}

	updated, err := store.Mark(MarkRoadRequest{
		OSMWayID: 42,
		Name:     "Mystery Lane",
		Surface:  SurfaceUnpaved,
	})
	if err != nil {
		t.Fatalf("second Mark() returned error: %v", err)
	}
	if updated.Surface != SurfaceUnpaved || updated.CreatedAt != road.CreatedAt {
		t.Fatalf("updated road = %+v, want unpaved with original creation time", updated)
	}

	roads, err := store.List()
	if err != nil {
		t.Fatalf("List() returned error: %v", err)
	}
	if len(roads) != 1 || roads[0].Surface != SurfaceUnpaved {
		t.Fatalf("List() = %+v, want one unpaved road", roads)
	}

	if err := store.Delete("way:42"); err != nil {
		t.Fatalf("Delete() returned error: %v", err)
	}
	roads, err = store.List()
	if err != nil {
		t.Fatalf("List() after delete returned error: %v", err)
	}
	if len(roads) != 0 {
		t.Fatalf("List() after delete = %+v, want empty", roads)
	}
}

func TestStoreRejectsUnknownSurface(t *testing.T) {
	store := NewStore(t.TempDir())

	if _, err := store.Mark(MarkRoadRequest{OSMWayID: 42, Surface: "sand"}); err == nil {
		t.Fatal("expected unknown surface mark to be rejected")
	}
}
