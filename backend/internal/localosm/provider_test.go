package localosm

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"

	"github.com/EDessin/RouteRoulette/backend/internal/history"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

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

func TestLoadGraphKeepsDiskCacheInMemory(t *testing.T) {
	provider := NewProvider(Config{
		DataDir:       t.TempDir(),
		ExtractPath:   "missing.osm.pbf",
		AllowDownload: false,
	})
	home := planner.Coordinate{Lat: 1, Lon: 2}
	graph := &Graph{
		Home:     home,
		RadiusKm: 50,
		Nodes: []GraphNode{
			{Lat: 1, Lon: 2},
		},
		Edges: [][]GraphEdge{{}},
	}
	cachePath := provider.graphCachePath(home)
	if err := saveGraphCache(cachePath, graph); err != nil {
		t.Fatalf("saveGraphCache() returned error: %v", err)
	}

	if _, err := provider.loadGraph(context.Background(), home); err != nil {
		t.Fatalf("loadGraph() returned error: %v", err)
	}
	if err := os.Remove(cachePath); err != nil {
		t.Fatalf("os.Remove() returned error: %v", err)
	}
	if _, err := provider.loadGraph(context.Background(), home); err != nil {
		t.Fatalf("loadGraph() should use the in-memory graph after first load, got error: %v", err)
	}
}

func TestNewProviderDefaultsToTwentyKilometerRadius(t *testing.T) {
	provider := NewProvider(Config{})

	if provider.cfg.RadiusKm != 20 {
		t.Fatalf("RadiusKm = %.0f, want 20", provider.cfg.RadiusKm)
	}
}

func TestRouteSubgraphKeepsOnlyNearbyUsableRoads(t *testing.T) {
	graph := Graph{
		RadiusKm: 20,
		Nodes: []GraphNode{
			{Lat: 0, Lon: 0},
			{Lat: 0, Lon: 0.001},
			{Lat: 0, Lon: 0.002},
			{Lat: 0, Lon: 0.003},
			{Lat: 0, Lon: 0.004},
			{Lat: 0, Lon: 0.005},
			{Lat: 1, Lon: 1},
			{Lat: 1, Lon: 1.001},
		},
		Edges: make([][]GraphEdge, 8),
	}
	addTestEdge(&graph, 0, 1, SurfacePaved)
	addTestEdge(&graph, 1, 2, SurfacePaved)
	addTestEdge(&graph, 2, 3, SurfacePaved)
	addTestEdge(&graph, 3, 4, SurfacePaved)
	addTestEdge(&graph, 4, 5, SurfacePaved)
	addTestEdge(&graph, 6, 7, SurfacePaved)
	addTestEdge(&graph, 0, 2, SurfaceUnpaved)

	subgraph, radiusKm, err := graph.routeSubgraph(planner.Coordinate{Lat: 0, Lon: 0}, 4000, true, SurfacePolicyStrict)
	if err != nil {
		t.Fatalf("routeSubgraph() returned error: %v", err)
	}

	if radiusKm != 5 {
		t.Fatalf("routeSubgraph radius = %.1f, want 5.0", radiusKm)
	}
	if len(subgraph.Nodes) != 6 {
		t.Fatalf("routeSubgraph nodes = %d, want nearby 6 nodes", len(subgraph.Nodes))
	}
	for from, edges := range subgraph.Edges {
		for _, edge := range edges {
			if edge.Surface != SurfacePaved {
				t.Fatalf("routeSubgraph kept non-paved edge from %d to %d", from, edge.To)
			}
		}
	}
}

func TestRouteSubgraphRadiusIsBoundedForInteractiveSearch(t *testing.T) {
	tests := []struct {
		targetM float64
		want    float64
	}{
		{targetM: 1000, want: 4},
		{targetM: 8000, want: 7},
		{targetM: 40000, want: 10},
	}

	for _, tt := range tests {
		if got := routeSubgraphRadiusKm(tt.targetM, 20); got != tt.want {
			t.Fatalf("routeSubgraphRadiusKm(%.0f) = %.1f, want %.1f", tt.targetM, got, tt.want)
		}
	}
}

func TestWaypointSetUsesSamePavedComponentAndPrefersIntersections(t *testing.T) {
	graph := Graph{
		Nodes: []GraphNode{
			{Lat: 0, Lon: 0},
			{Lat: 0, Lon: 0.005},
			{Lat: 0.005, Lon: 0},
			{Lat: 0, Lon: -0.005},
			{Lat: -0.005, Lon: 0},
			{Lat: 0.005, Lon: 0.005},
			{Lat: 0.005, Lon: 0.01},
			{Lat: 0.01, Lon: 0.005},
			{Lat: 0.005, Lon: 0},
		},
		Edges: make([][]GraphEdge, 9),
	}
	addTestEdge(&graph, 0, 1, SurfacePaved)
	addTestEdge(&graph, 1, 2, SurfacePaved)
	addTestEdge(&graph, 1, 3, SurfacePaved)
	addTestEdge(&graph, 1, 4, SurfacePaved)
	addTestEdge(&graph, 5, 6, SurfacePaved)
	addTestEdge(&graph, 5, 7, SurfacePaved)
	addTestEdge(&graph, 5, 8, SurfacePaved)

	set := graph.newWaypointSet(0, 4000, true, SurfacePolicyStrict)

	if len(set.Nodes) == 0 {
		t.Fatal("expected waypoint set to contain connected paved waypoint nodes")
	}
	for _, node := range set.Nodes {
		if node.Index == 5 || node.Index == 6 || node.Index == 7 || node.Index == 8 {
			t.Fatalf("waypoint set included disconnected node %d", node.Index)
		}
		if node.Degree < 2 {
			t.Fatalf("waypoint node degree = %d, want at least fallback intersection degree 2", node.Degree)
		}
	}
}

func TestWaypointPickSkipsUsedNodes(t *testing.T) {
	set := waypointSet{
		Nodes: []waypointNode{
			{Index: 1, Bearing: 0, DistM: 500, Degree: 3},
			{Index: 2, Bearing: 0.1, DistM: 550, Degree: 3},
		},
	}
	used := map[int]struct{}{1: {}}

	idx, err := set.pick(0, 500, used, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("pick() returned error: %v", err)
	}
	if idx != 2 {
		t.Fatalf("pick() = %d, want 2", idx)
	}
}

func addTestEdge(graph *Graph, from int, to int, surface int) {
	distance := distanceM(graph.Nodes[from].Lat, graph.Nodes[from].Lon, graph.Nodes[to].Lat, graph.Nodes[to].Lon)
	graph.Edges[from] = append(graph.Edges[from], GraphEdge{To: to, Distance: distance, Surface: surface})
	graph.Edges[to] = append(graph.Edges[to], GraphEdge{To: from, Distance: distance, Surface: surface})
}

func twoDigit(value int) string {
	return fmt.Sprintf("%02d", value)
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

	if localScore(withinLimit, targetM, 70, false) >= localScore(overLimit, targetM, 70, false) {
		t.Fatal("expected local scoring to penalize routes more than 0.5 km longer than requested")
	}
}

func TestLocalScorePenalizesPreviouslyRunRoadsWhenPreferred(t *testing.T) {
	targetM := 10000.0

	unrunRoute := localCandidate{
		DistanceM:            10000,
		PavedPercent:         90,
		PreviouslyRunPercent: 0,
	}
	previouslyRunRoute := localCandidate{
		DistanceM:            10000,
		PavedPercent:         90,
		PreviouslyRunPercent: 50,
	}

	if localScore(unrunRoute, targetM, 90, true) >= localScore(previouslyRunRoute, targetM, 90, true) {
		t.Fatal("expected local scoring to prefer unrun roads")
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

func TestRepeatedEdgesAllowsShortHomeConnector(t *testing.T) {
	allowed := map[edgeKey]struct{}{
		newEdgeKey(1, 2): {},
	}

	if hasRepeatedEdgesExcept([]int{1, 2, 3, 4, 2, 1}, allowed) {
		t.Fatal("expected repeated home connector to be allowed")
	}
	if !hasRepeatedEdgesExcept([]int{1, 2, 3, 2, 4}, allowed) {
		t.Fatal("expected non-connector repeated edge to be rejected")
	}
}

func TestHomeConnectorEdgesUseSegmentsNearStart(t *testing.T) {
	graph := Graph{
		Nodes: []GraphNode{
			{Lat: 0, Lon: 0},
			{Lat: 0, Lon: 0.001},
			{Lat: 0, Lon: 0.0017},
			{Lat: 0, Lon: 0.004},
		},
	}
	path := []int{0, 1, 2, 3}

	allowed := make(map[edgeKey]struct{})
	graph.addHomeConnectorEdgesNearStart(allowed, 0, path, 200)

	if len(allowed) != 2 {
		t.Fatalf("addHomeConnectorEdgesNearStart() allowed %d edges, want 2", len(allowed))
	}
	if _, ok := allowed[newEdgeKey(2, 3)]; ok {
		t.Fatal("expected edge beyond connector limit to be excluded")
	}
}

func TestWaypointCountUsesMoreStopsForLongRoutes(t *testing.T) {
	if got := waypointCountForTarget(8000, rand.New(rand.NewSource(1))); got != 3 {
		t.Fatalf("waypointCountForTarget(8000) = %d, want 3", got)
	}

	for _, targetM := range []float64{12000, 16000, 22000, 40000} {
		for seed := int64(0); seed < 10; seed++ {
			got := waypointCountForTarget(targetM, rand.New(rand.NewSource(seed)))
			if got < 5 || got > 8 {
				t.Fatalf("waypointCountForTarget(%.0f) = %d, want 5..8", targetM, got)
			}
		}
	}
}

func TestRouteCandidateAttemptsIncreaseForLongRoutes(t *testing.T) {
	if got := routeCandidateAttempts(8000); got != 100 {
		t.Fatalf("routeCandidateAttempts(8000) = %d, want 100", got)
	}
	if got := routeCandidateAttempts(16000); got != 600 {
		t.Fatalf("routeCandidateAttempts(16000) = %d, want 600", got)
	}
}

func TestHistoryDistanceCountsPreviouslyRunEdges(t *testing.T) {
	historyEdges := map[edgeKey]struct{}{
		newEdgeKey(1, 2): {},
	}
	distance := historyDistance([]int{0, 1, 2}, []GraphEdge{
		{To: 1, Distance: 40},
		{To: 2, Distance: 60},
	}, historyEdges)

	if distance != 60 {
		t.Fatalf("historyDistance() = %.0f, want 60", distance)
	}
}

func TestHistoryOverlayTracksLastTenActivitiesSeparately(t *testing.T) {
	graph := Graph{
		Nodes: make([]GraphNode, 0, 22),
		Edges: make([][]GraphEdge, 22),
	}
	for i := 0; i < 11; i++ {
		lon := float64(i) * 0.01
		graph.Nodes = append(graph.Nodes,
			GraphNode{Lat: 0, Lon: lon},
			GraphNode{Lat: 0, Lon: lon + 0.001},
		)
		addTestEdge(&graph, i*2, i*2+1, SurfacePaved)
	}
	store := history.NewStore(t.TempDir())
	for i := 0; i < 11; i++ {
		lon := float64(i)*0.01 + 0.0005
		if err := store.SaveActivity(history.Activity{
			ID:        int64(i + 1),
			StartDate: "2026-05-" + twoDigit(i+1) + "T08:00:00Z",
			SportType: "Run",
			DistanceM: 1000,
			Coordinates: []planner.Coordinate{
				{Lat: 0, Lon: lon},
			},
		}); err != nil {
			t.Fatalf("SaveActivity() returned error: %v", err)
		}
	}

	overlay, err := graph.historyEdges(&store)
	if err != nil {
		t.Fatalf("historyEdges() returned error: %v", err)
	}
	if overlay.RecentActivities != 10 {
		t.Fatalf("RecentActivities = %d, want 10", overlay.RecentActivities)
	}
	if len(overlay.AllEdges) != 11 {
		t.Fatalf("AllEdges = %d, want 11", len(overlay.AllEdges))
	}
	if len(overlay.RecentEdges) != 10 {
		t.Fatalf("RecentEdges = %d, want 10", len(overlay.RecentEdges))
	}
	if _, ok := overlay.RecentEdges[newEdgeKey(0, 1)]; ok {
		t.Fatal("expected oldest activity edge to be excluded from recent history")
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

	path, edges, err := graph.shortestPath(0, 1, 70, true, SurfacePolicyStrict, nil, nil, graph.newSearchWorkspace())
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

func TestShortestPathAvoidsBlockedRoadSegments(t *testing.T) {
	graph := Graph{
		Nodes: []GraphNode{
			{Lat: 0, Lon: 0},
			{Lat: 0, Lon: 0.001},
			{Lat: 0.001, Lon: 0},
			{Lat: 0.001, Lon: 0.001},
		},
		Edges: make([][]GraphEdge, 4),
	}
	addTestEdge(&graph, 0, 1, SurfacePaved)
	addTestEdge(&graph, 0, 2, SurfacePaved)
	addTestEdge(&graph, 2, 3, SurfacePaved)
	addTestEdge(&graph, 3, 1, SurfacePaved)
	blocked := map[edgeKey]struct{}{
		newEdgeKey(0, 1): {},
	}

	path, _, err := graph.shortestPath(0, 1, 70, true, SurfacePolicyStrict, blocked, nil, graph.newSearchWorkspace())
	if err != nil {
		t.Fatalf("shortestPath() returned error: %v", err)
	}
	if hasRepeatedEdges(path) {
		t.Fatalf("shortestPath() returned repeated path %v", path)
	}
	if len(path) != 4 || path[0] != 0 || path[1] != 2 || path[2] != 3 || path[3] != 1 {
		t.Fatalf("shortestPath() path = %v, want [0 2 3 1]", path)
	}
}

func TestShortestPathPenalizesRecentlyRunRoadSegments(t *testing.T) {
	graph := Graph{
		Nodes: []GraphNode{
			{Lat: 0, Lon: 0},
			{Lat: 0, Lon: 0.001},
			{Lat: 0.001, Lon: 0},
			{Lat: 0.001, Lon: 0.001},
		},
		Edges: make([][]GraphEdge, 4),
	}
	addTestEdge(&graph, 0, 1, SurfacePaved)
	addTestEdge(&graph, 0, 2, SurfacePaved)
	addTestEdge(&graph, 2, 3, SurfacePaved)
	addTestEdge(&graph, 3, 1, SurfacePaved)
	recent := map[edgeKey]struct{}{
		newEdgeKey(0, 1): {},
	}

	path, _, err := graph.shortestPath(0, 1, 70, true, SurfacePolicyStrict, nil, recent, graph.newSearchWorkspace())
	if err != nil {
		t.Fatalf("shortestPath() returned error: %v", err)
	}
	if len(path) != 4 || path[0] != 0 || path[1] != 2 || path[2] != 3 || path[3] != 1 {
		t.Fatalf("shortestPath() path = %v, want [0 2 3 1]", path)
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

	if _, _, err := graph.shortestPath(0, 1, 70, true, SurfacePolicyStrict, nil, nil, graph.newSearchWorkspace()); err == nil {
		t.Fatal("expected no paved-only path to return an error")
	}
}

func TestShortestPathAllowsUnknownSurfaceWhenAssumingNormalRoadsPaved(t *testing.T) {
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

	path, _, err := graph.shortestPath(0, 1, 70, true, SurfacePolicyAssumePaved, nil, nil, graph.newSearchWorkspace())
	if err != nil {
		t.Fatalf("shortestPath() returned error: %v", err)
	}
	if len(path) != 2 || path[0] != 0 || path[1] != 1 {
		t.Fatalf("shortestPath() path = %v, want [0 1]", path)
	}
}

func TestEdgeStatsKeepsSurfaceBucketsExclusive(t *testing.T) {
	total, paved, _, unknown := edgeStats([]GraphEdge{
		{Distance: 40, Surface: SurfacePaved},
		{Distance: 60, Surface: SurfaceUnknown},
	})

	if total != 100 || paved != 40 || unknown != 60 {
		t.Fatalf("edgeStats() = total %.0f, paved %.0f, unknown %.0f; want 100, 40, 60", total, paved, unknown)
	}
}
