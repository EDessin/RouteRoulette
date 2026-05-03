package localosm

import "testing"

func TestClassifySurface(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
		want int
	}{
		{
			name: "asphalt is paved",
			tags: map[string]string{"surface": "asphalt"},
			want: SurfacePaved,
		},
		{
			name: "gravel is unpaved",
			tags: map[string]string{"surface": "gravel"},
			want: SurfaceUnpaved,
		},
		{
			name: "track grade1 is paved",
			tags: map[string]string{"tracktype": "grade1"},
			want: SurfacePaved,
		},
		{
			name: "missing surface is unknown",
			tags: map[string]string{},
			want: SurfaceUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifySurface(tt.tags); got != tt.want {
				t.Fatalf("classifySurface() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLocalScorePenalizesRoutesMoreThanHalfKilometerLong(t *testing.T) {
	targetM := 10000.0

	withinLimit := localCandidate{
		DistanceM:    10500,
		PavedPercent: 70,
	}
	overLimit := localCandidate{
		DistanceM:    10600,
		PavedPercent: 70,
	}

	if localScore(withinLimit, targetM, 70) >= localScore(overLimit, targetM, 70) {
		t.Fatal("expected local scoring to penalize routes more than 0.5 km longer than requested")
	}
}

func TestHasRepeatedEdgesDetectsOutAndBack(t *testing.T) {
	if !hasRepeatedEdges([]int{1, 2, 1}) {
		t.Fatal("expected an out-and-back path to count as a repeated road segment")
	}
}

func TestHasRepeatedEdgesAllowsSimpleLoop(t *testing.T) {
	if hasRepeatedEdges([]int{1, 2, 3, 1}) {
		t.Fatal("expected a simple loop without repeated segments to be allowed")
	}
}

func TestShortestPathSkipsUnpavedEdgesWhenPavedOnly(t *testing.T) {
	graph := Graph{
		Nodes: []GraphNode{
			{Lat: 0, Lon: 0},
			{Lat: 0, Lon: 1},
			{Lat: 1, Lon: 0},
		},
		Edges: [][]GraphEdge{
			{
				{To: 1, Distance: 1, Surface: SurfaceUnpaved},
				{To: 2, Distance: 1, Surface: SurfacePaved},
			},
			{
				{To: 0, Distance: 1, Surface: SurfaceUnpaved},
				{To: 2, Distance: 1, Surface: SurfacePaved},
			},
			{
				{To: 0, Distance: 1, Surface: SurfacePaved},
				{To: 1, Distance: 1, Surface: SurfacePaved},
			},
		},
	}

	path, edges, err := graph.shortestPath(0, 1, 70, true)
	if err != nil {
		t.Fatalf("shortestPath() returned error: %v", err)
	}
	if len(path) != 3 || path[0] != 0 || path[1] != 2 || path[2] != 1 {
		t.Fatalf("shortestPath() path = %v, want [0 2 1]", path)
	}
	for _, edge := range edges {
		if edge.Surface != SurfacePaved {
			t.Fatal("expected paved-only path to contain only paved edges")
		}
	}
}

func TestShortestPathReturnsErrorWhenNoPavedPathExists(t *testing.T) {
	graph := Graph{
		Nodes: []GraphNode{
			{Lat: 0, Lon: 0},
			{Lat: 0, Lon: 1},
		},
		Edges: [][]GraphEdge{
			{{To: 1, Distance: 1, Surface: SurfaceUnknown}},
			{{To: 0, Distance: 1, Surface: SurfaceUnknown}},
		},
	}

	if _, _, err := graph.shortestPath(0, 1, 70, true); err == nil {
		t.Fatal("expected no paved-only path to return an error")
	}
}
