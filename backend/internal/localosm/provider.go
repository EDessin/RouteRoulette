package localosm

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/history"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
)

const (
	SurfacePaved = iota
	SurfaceUnpaved
	SurfaceUnknown

	SurfacePolicyStrict      = "strict"
	SurfacePolicyAssumePaved = "assume_paved"

	routeGenerationBudget   = 10 * time.Second
	recentRoadPenaltyWeight = 3
	sharedHomeConnectorM    = 200
)

var errRouteSearchTimedOut = errors.New("route search timed out")

type Config struct {
	DataDir       string
	ExtractPath   string
	ExtractURL    string
	RadiusKm      float64
	AllowDownload bool
	HistoryStore  *history.Store
}

type Provider struct {
	cfg    Config
	client *http.Client
	cache  *graphMemoryCache
}

type graphMemoryCache struct {
	mu           sync.Mutex
	graphs       map[string]*Graph
	historyEdges map[string]cachedHistoryEdges
}

type cachedHistoryEdges struct {
	LastSyncAt       string
	SyncedActivities int
	Overlay          historyOverlay
}

type historyOverlay struct {
	AllEdges         map[edgeKey]struct{}
	RecentEdges      map[edgeKey]struct{}
	RecentActivities int
}

func NewProvider(cfg Config) Provider {
	if cfg.DataDir == "" {
		cfg.DataDir = "data/osm"
	}
	if cfg.ExtractPath == "" {
		cfg.ExtractPath = filepath.Join(cfg.DataDir, "belgium-latest.osm.pbf")
	}
	if cfg.ExtractURL == "" {
		cfg.ExtractURL = "https://download.geofabrik.de/europe/belgium-latest.osm.pbf"
	}
	if cfg.RadiusKm <= 0 {
		cfg.RadiusKm = 20
	}

	return Provider{
		cfg: cfg,
		client: &http.Client{
			Timeout: 20 * time.Minute,
		},
		cache: &graphMemoryCache{
			graphs:       make(map[string]*Graph),
			historyEdges: make(map[string]cachedHistoryEdges),
		},
	}
}

func (p Provider) GenerateRoundTrip(ctxReq *http.Request, req planner.CandidateRequest) (planner.CandidateRoute, error) {
	started := time.Now()
	graphLoadStarted := time.Now()
	graph, err := p.loadGraph(ctxReq.Context(), req.Home)
	if err != nil {
		return planner.CandidateRoute{}, err
	}
	graphLoadDuration := time.Since(graphLoadStarted)

	subgraphStarted := time.Now()
	pavedOnly := pavedOnly(req)
	policy := surfacePolicy(req)
	routeGraph, subgraphRadiusKm, err := graph.routeSubgraph(req.Start, req.TargetDistanceM, pavedOnly, policy)
	if err != nil {
		return planner.CandidateRoute{}, err
	}
	subgraphDuration := time.Since(subgraphStarted)

	routeHistory := emptyHistoryOverlay()
	historyDuration := time.Duration(0)
	if req.PreferUnrunRoads && p.cfg.HistoryStore != nil {
		historyStarted := time.Now()
		routeHistory, err = p.loadHistoryEdges(routeGraph, p.historyCacheKey(req, subgraphRadiusKm, pavedOnly, policy))
		if err != nil {
			return planner.CandidateRoute{}, err
		}
		historyDuration = time.Since(historyStarted)
	}

	loopStarted := time.Now()
	route, err := routeGraph.GenerateLoop(req, routeHistory)
	if err != nil {
		log.Printf("local-osm route failed: total=%s graph_load=%s subgraph=%s history=%s loop=%s full_nodes=%d full_directed_edges=%d route_nodes=%d route_directed_edges=%d subgraph_radius_km=%.1f route_history_edges=%d recent_history_edges=%d recent_activities=%d err=%v",
			time.Since(started).Round(time.Millisecond),
			graphLoadDuration.Round(time.Millisecond),
			subgraphDuration.Round(time.Millisecond),
			historyDuration.Round(time.Millisecond),
			time.Since(loopStarted).Round(time.Millisecond),
			len(graph.Nodes),
			graph.directedEdgeCount(),
			len(routeGraph.Nodes),
			routeGraph.directedEdgeCount(),
			subgraphRadiusKm,
			len(routeHistory.AllEdges),
			len(routeHistory.RecentEdges),
			routeHistory.RecentActivities,
			err,
		)
		return planner.CandidateRoute{}, err
	}
	log.Printf("local-osm route generated: total=%s graph_load=%s subgraph=%s history=%s loop=%s full_nodes=%d full_directed_edges=%d route_nodes=%d route_directed_edges=%d subgraph_radius_km=%.1f route_history_edges=%d recent_history_edges=%d recent_activities=%d distance_km=%.2f",
		time.Since(started).Round(time.Millisecond),
		graphLoadDuration.Round(time.Millisecond),
		subgraphDuration.Round(time.Millisecond),
		historyDuration.Round(time.Millisecond),
		time.Since(loopStarted).Round(time.Millisecond),
		len(graph.Nodes),
		graph.directedEdgeCount(),
		len(routeGraph.Nodes),
		routeGraph.directedEdgeCount(),
		subgraphRadiusKm,
		len(routeHistory.AllEdges),
		len(routeHistory.RecentEdges),
		routeHistory.RecentActivities,
		route.DistanceM/1000,
	)
	return route, nil
}

func (p Provider) PlannerAttempts() int {
	return 1
}

func (p Provider) loadGraph(ctx context.Context, home planner.Coordinate) (*Graph, error) {
	cachePath := p.graphCachePath(home)
	p.cache.mu.Lock()
	defer p.cache.mu.Unlock()

	if graph := p.cache.graphs[cachePath]; graph != nil {
		return graph, nil
	}

	if graph, err := loadGraphCache(cachePath); err == nil {
		p.cache.graphs[cachePath] = graph
		return graph, nil
	}

	if err := p.ensureExtract(ctx); err != nil {
		return nil, err
	}

	graph, err := BuildGraphFromPBF(ctx, p.cfg.ExtractPath, home, p.cfg.RadiusKm)
	if err != nil {
		return nil, err
	}
	if len(graph.Nodes) == 0 {
		return nil, errors.New("local OSM graph contains no usable roads near home")
	}

	if err := saveGraphCache(cachePath, graph); err != nil {
		return nil, err
	}
	p.cache.graphs[cachePath] = graph
	return graph, nil
}

func (p Provider) loadHistoryEdges(graph *Graph, cacheKey string) (historyOverlay, error) {
	status, err := p.cfg.HistoryStore.Status(false)
	if err != nil {
		return historyOverlay{}, err
	}

	p.cache.mu.Lock()
	cached := p.cache.historyEdges[cacheKey]
	if cached.Overlay.AllEdges != nil && cached.LastSyncAt == status.LastSyncAt && cached.SyncedActivities == status.SyncedActivities {
		p.cache.mu.Unlock()
		return cached.Overlay, nil
	}
	p.cache.mu.Unlock()

	overlay, err := graph.historyEdges(p.cfg.HistoryStore)
	if err != nil {
		return historyOverlay{}, err
	}

	p.cache.mu.Lock()
	p.cache.historyEdges[cacheKey] = cachedHistoryEdges{
		LastSyncAt:       status.LastSyncAt,
		SyncedActivities: status.SyncedActivities,
		Overlay:          overlay,
	}
	p.cache.mu.Unlock()
	return overlay, nil
}

func (p Provider) historyCacheKey(req planner.CandidateRequest, subgraphRadiusKm float64, pavedOnly bool, surfacePolicy string) string {
	return fmt.Sprintf("%s|start=%.5f,%.5f|target=%.0fm|radius=%.1fkm|paved=%t|surface=%s",
		p.graphCachePath(req.Home),
		req.Start.Lat,
		req.Start.Lon,
		req.TargetDistanceM,
		subgraphRadiusKm,
		pavedOnly,
		surfacePolicy,
	)
}

func (p Provider) ensureExtract(ctx context.Context) error {
	if _, err := os.Stat(p.cfg.ExtractPath); err == nil {
		return nil
	}
	if !p.cfg.AllowDownload {
		return fmt.Errorf("OSM extract not found at %s", p.cfg.ExtractPath)
	}

	if err := os.MkdirAll(filepath.Dir(p.cfg.ExtractPath), 0o755); err != nil {
		return err
	}

	tmpPath := p.cfg.ExtractPath + ".download"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.ExtractURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download OSM extract returned %s", resp.Status)
	}

	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, p.cfg.ExtractPath)
}

func (p Provider) graphCachePath(home planner.Coordinate) string {
	name := fmt.Sprintf("roads_%.5f_%.5f_%.0fkm.json", home.Lat, home.Lon, p.cfg.RadiusKm)
	name = strings.NewReplacer("-", "m", ".", "p").Replace(name)
	return filepath.Join(p.cfg.DataDir, "road-cache", name)
}

type Graph struct {
	Home      planner.Coordinate `json:"home"`
	RadiusKm  float64            `json:"radiusKm"`
	CreatedAt string             `json:"createdAt"`
	Nodes     []GraphNode        `json:"nodes"`
	Edges     [][]GraphEdge      `json:"edges"`
}

type GraphNode struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type GraphEdge struct {
	To       int     `json:"to"`
	Distance float64 `json:"distance"`
	Surface  int     `json:"surface"`
}

func loadGraphCache(path string) (*Graph, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var graph Graph
	if err := json.NewDecoder(file).Decode(&graph); err != nil {
		return nil, err
	}
	if len(graph.Nodes) == 0 || len(graph.Edges) != len(graph.Nodes) {
		return nil, errors.New("invalid graph cache")
	}
	return &graph, nil
}

func saveGraphCache(path string, graph *Graph) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(graph); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

type osmCoord struct {
	Lat float64
	Lon float64
}

func BuildGraphFromPBF(ctx context.Context, path string, home planner.Coordinate, radiusKm float64) (*Graph, error) {
	nodeCoords, err := readNodes(ctx, path, home, radiusKm)
	if err != nil {
		return nil, err
	}
	if len(nodeCoords) == 0 {
		return nil, errors.New("no OSM nodes found near home")
	}

	graph := &Graph{
		Home:      home,
		RadiusKm:  radiusKm,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	indexByOSMID := make(map[osm.NodeID]int)

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := osmpbf.New(ctx, file, 3)
	defer scanner.Close()
	for scanner.Scan() {
		way, ok := scanner.Object().(*osm.Way)
		if !ok || !isRunnableWay(way.Tags.Map()) {
			continue
		}
		tags := way.Tags.Map()
		surface := classifySurface(tags)
		for i := 1; i < len(way.Nodes); i++ {
			aCoord, aOK := nodeCoords[way.Nodes[i-1].ID]
			bCoord, bOK := nodeCoords[way.Nodes[i].ID]
			if !aOK || !bOK {
				continue
			}
			if distanceKm(home, planner.Coordinate{Lat: aCoord.Lat, Lon: aCoord.Lon}) > radiusKm ||
				distanceKm(home, planner.Coordinate{Lat: bCoord.Lat, Lon: bCoord.Lon}) > radiusKm {
				continue
			}
			a := graph.nodeIndex(way.Nodes[i-1].ID, aCoord, indexByOSMID)
			b := graph.nodeIndex(way.Nodes[i].ID, bCoord, indexByOSMID)
			if a == b {
				continue
			}
			dist := distanceM(aCoord.Lat, aCoord.Lon, bCoord.Lat, bCoord.Lon)
			graph.Edges[a] = append(graph.Edges[a], GraphEdge{To: b, Distance: dist, Surface: surface})
			graph.Edges[b] = append(graph.Edges[b], GraphEdge{To: a, Distance: dist, Surface: surface})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return graph, nil
}

func readNodes(ctx context.Context, path string, home planner.Coordinate, radiusKm float64) (map[osm.NodeID]osmCoord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	latDelta := radiusKm / 111.32
	lonDelta := radiusKm / (111.32 * math.Cos(degreesToRadians(home.Lat)))
	minLat, maxLat := home.Lat-latDelta, home.Lat+latDelta
	minLon, maxLon := home.Lon-lonDelta, home.Lon+lonDelta

	nodes := make(map[osm.NodeID]osmCoord)
	scanner := osmpbf.New(ctx, file, 3)
	defer scanner.Close()
	for scanner.Scan() {
		node, ok := scanner.Object().(*osm.Node)
		if !ok {
			continue
		}
		if node.Lat >= minLat && node.Lat <= maxLat && node.Lon >= minLon && node.Lon <= maxLon {
			nodes[node.ID] = osmCoord{Lat: node.Lat, Lon: node.Lon}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

func (g *Graph) nodeIndex(id osm.NodeID, coord osmCoord, indexByOSMID map[osm.NodeID]int) int {
	if idx, ok := indexByOSMID[id]; ok {
		return idx
	}
	idx := len(g.Nodes)
	indexByOSMID[id] = idx
	g.Nodes = append(g.Nodes, GraphNode{Lat: coord.Lat, Lon: coord.Lon})
	g.Edges = append(g.Edges, nil)
	return idx
}

func (g *Graph) directedEdgeCount() int {
	total := 0
	for _, edges := range g.Edges {
		total += len(edges)
	}
	return total
}

func (g *Graph) routeSubgraph(start planner.Coordinate, targetM float64, pavedOnly bool, surfacePolicy string) (*Graph, float64, error) {
	radiusKm := routeSubgraphRadiusKm(targetM, g.RadiusKm)
	if len(g.Nodes) == 0 {
		return nil, radiusKm, errors.New("local graph is empty")
	}

	include := make([]bool, len(g.Nodes))
	for idx, node := range g.Nodes {
		include[idx] = distanceKm(start, planner.Coordinate{Lat: node.Lat, Lon: node.Lon}) <= radiusKm
	}

	oldToNew := make([]int, len(g.Nodes))
	for idx := range oldToNew {
		oldToNew[idx] = -1
	}
	subgraph := &Graph{
		Home:      start,
		RadiusKm:  radiusKm,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	for oldFrom, edges := range g.Edges {
		if !include[oldFrom] {
			continue
		}
		for _, edge := range edges {
			if !include[edge.To] || !usableSurface(edge.Surface, pavedOnly, surfacePolicy) {
				continue
			}
			newFrom := subgraph.nodeFromOldIndex(g, oldFrom, oldToNew)
			newTo := subgraph.nodeFromOldIndex(g, edge.To, oldToNew)
			subgraph.Edges[newFrom] = append(subgraph.Edges[newFrom], GraphEdge{
				To:       newTo,
				Distance: edge.Distance,
				Surface:  edge.Surface,
			})
		}
	}

	if len(subgraph.Nodes) == 0 {
		return nil, radiusKm, fmt.Errorf("no usable roads found within %.1f km of the selected start", radiusKm)
	}
	return subgraph, radiusKm, nil
}

func (g *Graph) nodeFromOldIndex(source *Graph, oldIdx int, oldToNew []int) int {
	if oldToNew[oldIdx] >= 0 {
		return oldToNew[oldIdx]
	}
	newIdx := len(g.Nodes)
	oldToNew[oldIdx] = newIdx
	g.Nodes = append(g.Nodes, source.Nodes[oldIdx])
	g.Edges = append(g.Edges, nil)
	return newIdx
}

func routeSubgraphRadiusKm(targetM float64, graphRadiusKm float64) float64 {
	if graphRadiusKm <= 0 {
		graphRadiusKm = 20
	}
	radiusKm := targetM/2000 + 3
	radiusKm = math.Max(4, radiusKm)
	radiusKm = math.Min(10, radiusKm)
	return math.Min(graphRadiusKm, radiusKm)
}

func isRunnableWay(tags map[string]string) bool {
	highway := tags["highway"]
	if highway == "" {
		return false
	}
	switch tags["access"] {
	case "private", "no":
		return false
	}
	if tags["foot"] == "no" {
		return false
	}
	switch highway {
	case "motorway", "motorway_link", "trunk", "trunk_link", "construction", "proposed", "raceway":
		return false
	default:
		return true
	}
}

func classifySurface(tags map[string]string) int {
	surface := strings.ToLower(tags["surface"])
	switch surface {
	case "asphalt", "paved", "concrete", "concrete:plates", "concrete:lanes", "paving_stones", "sett", "cobblestone", "metal", "wood":
		return SurfacePaved
	case "unpaved", "gravel", "fine_gravel", "dirt", "earth", "grass", "sand", "mud", "ground", "woodchips", "pebblestone":
		return SurfaceUnpaved
	}
	switch strings.ToLower(tags["tracktype"]) {
	case "grade1":
		return SurfacePaved
	case "grade3", "grade4", "grade5":
		return SurfaceUnpaved
	default:
		return SurfaceUnknown
	}
}

func (g *Graph) GenerateLoop(req planner.CandidateRequest, history historyOverlay) (planner.CandidateRoute, error) {
	started := time.Now()
	if len(g.Nodes) == 0 {
		return planner.CandidateRoute{}, errors.New("local graph is empty")
	}
	startLookupStarted := time.Now()
	start := g.nearestNode(req.Start)
	startLookupDuration := time.Since(startLookupStarted)
	if start < 0 {
		return planner.CandidateRoute{}, errors.New("no start node found in local graph")
	}

	rng := rand.New(rand.NewSource(req.Seed))
	best := localCandidate{}
	bestScore := math.MaxFloat64
	attempts := routeCandidateAttempts(req.TargetDistanceM)
	pavedOnly := pavedOnly(req)
	policy := surfacePolicy(req)
	waypointStarted := time.Now()
	waypoints := g.newWaypointSet(start, req.TargetDistanceM, pavedOnly, policy)
	waypointDuration := time.Since(waypointStarted)
	if len(waypoints.Nodes) == 0 {
		return planner.CandidateRoute{}, errors.New("no usable local OSM waypoint nodes found near start")
	}
	cycleAnchors := waypointSet{}
	if useCycleGenerator(req.TargetDistanceM) {
		cycleAnchors = g.newCycleAnchorSet(start, req.TargetDistanceM, pavedOnly, policy)
	}
	search := g.newSearchWorkspace()
	stats := &routeSearchStats{}
	search.Stats = stats
	deadline := started.Add(routeGenerationBudget)
	search.Deadline = deadline
	successes := 0
	for i := 0; i < attempts; i++ {
		if i > 0 && time.Now().After(deadline) {
			stats.TimedOut = true
			break
		}
		var candidate localCandidate
		var err error
		if useCycleGenerator(req.TargetDistanceM) && len(cycleAnchors.Nodes) > 0 {
			candidate, err = g.cycleCandidate(start, req.TargetDistanceM, req.MinPavedPercent, pavedOnly, policy, rng, cycleAnchors, history, search)
		} else {
			candidate, err = g.loopCandidate(start, req.TargetDistanceM, req.MinPavedPercent, pavedOnly, policy, rng, waypoints, history, search)
		}
		if err != nil {
			stats.CandidateFailures++
			if errors.Is(err, errRouteSearchTimedOut) {
				stats.TimedOut = true
				break
			}
			continue
		}
		successes++
		score := localScore(candidate, req.TargetDistanceM, req.MinPavedPercent, req.PreferUnrunRoads)
		if score < bestScore {
			best = candidate
			bestScore = score
		}
		if candidate.DistanceM >= req.TargetDistanceM && candidate.DistanceM <= req.TargetDistanceM+500 && math.Abs(candidate.PavedPercent-req.MinPavedPercent) <= 5 {
			break
		}
	}
	if len(best.Path) == 0 {
		log.Printf("local-osm loop failed: total=%s start_lookup=%s waypoint_build=%s waypoints=%d attempts=%d successes=%d failures=%d timed_out=%t search_calls=%d settled=%d touched=%d stale_skips=%d closed_skips=%d bound_skips=%d cost_skips=%d max_queue=%d",
			time.Since(started).Round(time.Millisecond),
			startLookupDuration.Round(time.Millisecond),
			waypointDuration.Round(time.Millisecond),
			len(waypoints.Nodes),
			attempts,
			successes,
			stats.CandidateFailures,
			stats.TimedOut,
			stats.SearchCalls,
			stats.SettledNodes,
			stats.TouchedNodes,
			stats.StaleQueueSkips,
			stats.ClosedQueueSkips,
			stats.BoundSkips,
			stats.CostSkips,
			stats.MaxQueueLen,
		)
		return planner.CandidateRoute{}, errors.New("could not find a local OSM loop")
	}
	log.Printf("local-osm loop stats: total=%s start_lookup=%s waypoint_build=%s waypoints=%d attempts=%d successes=%d failures=%d timed_out=%t search_calls=%d settled=%d touched=%d stale_skips=%d closed_skips=%d bound_skips=%d cost_skips=%d max_queue=%d best_distance_km=%.2f paved=%.1f previously_run=%.1f recent_previously_run=%.1f",
		time.Since(started).Round(time.Millisecond),
		startLookupDuration.Round(time.Millisecond),
		waypointDuration.Round(time.Millisecond),
		len(waypoints.Nodes),
		attempts,
		successes,
		stats.CandidateFailures,
		stats.TimedOut,
		stats.SearchCalls,
		stats.SettledNodes,
		stats.TouchedNodes,
		stats.StaleQueueSkips,
		stats.ClosedQueueSkips,
		stats.BoundSkips,
		stats.CostSkips,
		stats.MaxQueueLen,
		best.DistanceM/1000,
		best.PavedPercent,
		best.PreviouslyRunPercent,
		best.RecentPreviouslyRunPercent,
	)

	paved := round(best.TaggedPavedPercent, 1)
	unpaved := round(best.UnpavedPercent, 1)
	unknown := round(best.UnknownPercent, 1)
	var unrun *float64
	var previouslyRun *float64
	if req.PreferUnrunRoads {
		unrunValue := round(best.UnrunPercent, 1)
		previouslyRunValue := round(best.PreviouslyRunPercent, 1)
		unrun = &unrunValue
		previouslyRun = &previouslyRunValue
	}
	warnings := []string{}
	if policy == SurfacePolicyAssumePaved && unknown > 0 {
		warnings = append(warnings, fmt.Sprintf("%.0f%% of this route uses roads without OSM surface tags and treats them as paved because of the selected surface-data mode.", unknown))
	}
	if stats.TimedOut {
		warnings = append(warnings, fmt.Sprintf("Route generation stopped after %.0f seconds and returned the best route found so far.", routeGenerationBudget.Seconds()))
	}
	coords := make([][]float64, 0, len(best.Path))
	for _, idx := range best.Path {
		node := g.Nodes[idx]
		coords = append(coords, []float64{node.Lon, node.Lat})
	}
	return planner.CandidateRoute{
		Start:           req.Start,
		DistanceM:       best.DistanceM,
		DurationSeconds: (best.DistanceM / 1000) * 360,
		Geometry: planner.GeoJSONLine{
			Type:        "LineString",
			Coordinates: coords,
		},
		PavedPercent:         &paved,
		UnpavedPercent:       &unpaved,
		UnknownPercent:       &unknown,
		UnrunPercent:         unrun,
		PreviouslyRunPercent: previouslyRun,
		Provider:             "local-osm",
		Warnings:             warnings,
	}, nil
}

type localCandidate struct {
	Path                       []int
	DistanceM                  float64
	PavedPercent               float64
	TaggedPavedPercent         float64
	UnpavedPercent             float64
	UnknownPercent             float64
	UnrunPercent               float64
	PreviouslyRunPercent       float64
	RecentPreviouslyRunPercent float64
}

func pavedOnly(req planner.CandidateRequest) bool {
	return req.PreferPaved || req.MinPavedPercent > 0
}

func surfacePolicy(req planner.CandidateRequest) string {
	if req.SurfacePolicy == SurfacePolicyAssumePaved {
		return SurfacePolicyAssumePaved
	}
	return SurfacePolicyStrict
}

type waypointSet struct {
	Nodes []waypointNode
}

type waypointNode struct {
	Index   int
	Bearing float64
	DistM   float64
	Degree  int
}

func (g *Graph) newWaypointSet(start int, targetM float64, pavedOnly bool, surfacePolicy string) waypointSet {
	inComponent := g.connectedComponent(start, pavedOnly, surfacePolicy)
	loopRadiusM := math.Max(600, targetM/(2*math.Pi))
	minDistM := math.Max(250, loopRadiusM*0.45)
	maxDistM := math.Max(minDistM+250, loopRadiusM*1.75)
	minDegree := 3
	if targetM >= 12000 {
		minDegree = 4
	}
	nodes := g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, minDegree)
	if len(nodes) == 0 && minDegree > 3 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, 3)
	}
	if len(nodes) == 0 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, 2)
	}
	if len(nodes) == 0 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM*0.5, maxDistM*1.5, 1)
	}
	return waypointSet{Nodes: nodes}
}

func (g *Graph) newCycleAnchorSet(start int, targetM float64, pavedOnly bool, surfacePolicy string) waypointSet {
	inComponent := g.connectedComponent(start, pavedOnly, surfacePolicy)
	minDistM := math.Max(1500, targetM*0.18)
	maxDistM := math.Max(minDistM+500, targetM*0.55)
	nodes := g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, 4)
	if len(nodes) == 0 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, 3)
	}
	if len(nodes) == 0 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM*0.75, maxDistM*1.15, 2)
	}
	return waypointSet{Nodes: nodes}
}

func (g *Graph) waypointNodes(start int, inComponent []bool, pavedOnly bool, surfacePolicy string, minDistM float64, maxDistM float64, minDegree int) []waypointNode {
	startNode := g.Nodes[start]
	nodes := make([]waypointNode, 0)
	for idx, node := range g.Nodes {
		if idx == start || !inComponent[idx] {
			continue
		}
		degree := g.usableDegree(idx, pavedOnly, surfacePolicy)
		if degree < minDegree {
			continue
		}
		distM := distanceM(startNode.Lat, startNode.Lon, node.Lat, node.Lon)
		if distM < minDistM || distM > maxDistM {
			continue
		}
		nodes = append(nodes, waypointNode{
			Index:   idx,
			Bearing: bearingRadians(startNode.Lat, startNode.Lon, node.Lat, node.Lon),
			DistM:   distM,
			Degree:  degree,
		})
	}
	return nodes
}

func (g *Graph) connectedComponent(start int, pavedOnly bool, surfacePolicy string) []bool {
	seen := make([]bool, len(g.Nodes))
	queue := []int{start}
	seen[start] = true
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, edge := range g.Edges[node] {
			if !usableSurface(edge.Surface, pavedOnly, surfacePolicy) {
				continue
			}
			if !seen[edge.To] {
				seen[edge.To] = true
				queue = append(queue, edge.To)
			}
		}
	}
	return seen
}

func (g *Graph) usableDegree(node int, pavedOnly bool, surfacePolicy string) int {
	neighbors := make(map[int]struct{})
	for _, edge := range g.Edges[node] {
		if !usableSurface(edge.Surface, pavedOnly, surfacePolicy) {
			continue
		}
		neighbors[edge.To] = struct{}{}
	}
	return len(neighbors)
}

func (set waypointSet) pick(desiredBearing float64, desiredDistanceM float64, used map[int]struct{}, rng *rand.Rand) (int, error) {
	if len(set.Nodes) == 0 {
		return -1, errors.New("waypoint set is empty")
	}
	bestIdx := -1
	bestScore := math.MaxFloat64
	for i, node := range set.Nodes {
		if _, ok := used[node.Index]; ok {
			continue
		}
		bearingPenalty := angularDifference(node.Bearing, desiredBearing) / math.Pi
		distancePenalty := math.Abs(node.DistM-desiredDistanceM) / math.Max(1, desiredDistanceM)
		degreeBonus := math.Min(float64(node.Degree-1), 4) * 0.04
		randomJitter := rng.Float64() * 0.08
		score := bearingPenalty*2 + distancePenalty - degreeBonus + randomJitter
		if score < bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return -1, errors.New("no unused waypoint nodes available")
	}
	return set.Nodes[bestIdx].Index, nil
}

func (g *Graph) loopCandidate(start int, targetM float64, minPavedPercent float64, pavedOnly bool, surfacePolicy string, rng *rand.Rand, waypointSet waypointSet, history historyOverlay, search *searchWorkspace) (localCandidate, error) {
	radiusM := math.Max(600, targetM/(2*math.Pi))
	baseBearing := rng.Float64() * 2 * math.Pi
	waypointCount := waypointCountForTarget(targetM, rng)
	waypoints := make([]int, 0, waypointCount)
	usedWaypoints := map[int]struct{}{start: {}}
	for i := 0; i < waypointCount; i++ {
		bearing := baseBearing + float64(i)*2*math.Pi/float64(waypointCount)
		distance := radiusM * (0.75 + rng.Float64()*0.75)
		waypoint, err := waypointSet.pick(normalizeBearing(bearing), distance, usedWaypoints, rng)
		if err != nil {
			return localCandidate{}, err
		}
		waypoints = append(waypoints, waypoint)
		usedWaypoints[waypoint] = struct{}{}
	}
	points := append([]int{start}, waypoints...)
	points = append(points, start)
	bounds := g.newRouteSearchBounds(points, targetM, minPavedPercent, pavedOnly, surfacePolicy)

	fullPath := []int{}
	var edges []GraphEdge
	usedEdges := make(map[edgeKey]struct{})
	sharedConnectorEdges := make(map[edgeKey]struct{})
	for i := 1; i < len(points); i++ {
		search.Bounds = bounds
		path, pathEdges, err := g.shortestPath(points[i-1], points[i], minPavedPercent, pavedOnly, surfacePolicy, usedEdges, history.RecentEdges, search)
		if err != nil {
			return localCandidate{}, err
		}
		g.addHomeConnectorEdgesNearStart(sharedConnectorEdges, start, path, sharedHomeConnectorM)
		addBlockedPathEdgesExcept(usedEdges, path, sharedConnectorEdges)
		if i > 1 && len(path) > 0 {
			path = path[1:]
		}
		fullPath = append(fullPath, path...)
		edges = append(edges, pathEdges...)
	}
	if len(fullPath) < 2 {
		return localCandidate{}, errors.New("route path is too short")
	}

	return buildLocalCandidate(fullPath, edges, targetM, surfacePolicy, sharedConnectorEdges, history)
}

func (g *Graph) cycleCandidate(start int, targetM float64, minPavedPercent float64, pavedOnly bool, surfacePolicy string, rng *rand.Rand, anchors waypointSet, history historyOverlay, search *searchWorkspace) (localCandidate, error) {
	startNode := g.Nodes[start]
	desiredBearing := rng.Float64() * 2 * math.Pi
	desiredDistanceM := targetM * (0.28 + rng.Float64()*0.24)
	anchor, err := anchors.pick(desiredBearing, desiredDistanceM, map[int]struct{}{start: {}}, rng)
	if err != nil {
		return localCandidate{}, err
	}

	bounds := g.newRouteSearchBounds([]int{start, anchor}, targetM, minPavedPercent, pavedOnly, surfacePolicy)
	startBearing := bearingRadians(startNode.Lat, startNode.Lon, g.Nodes[anchor].Lat, g.Nodes[anchor].Lon)
	bounds = bounds.expandToward(startNode, startBearing, targetM*0.35)

	search.Bounds = bounds
	outPath, outEdges, err := g.shortestPath(start, anchor, minPavedPercent, pavedOnly, surfacePolicy, nil, history.RecentEdges, search)
	if err != nil {
		return localCandidate{}, err
	}

	sharedConnectorEdges := make(map[edgeKey]struct{})
	g.addHomeConnectorEdgesNearStart(sharedConnectorEdges, start, outPath, sharedHomeConnectorM)
	blocked := make(map[edgeKey]struct{})
	addBlockedPathEdgesExcept(blocked, outPath, sharedConnectorEdges)

	search.Bounds = bounds
	backPath, backEdges, err := g.shortestPath(anchor, start, minPavedPercent, pavedOnly, surfacePolicy, blocked, history.RecentEdges, search)
	if err != nil {
		return localCandidate{}, err
	}
	g.addHomeConnectorEdgesNearStart(sharedConnectorEdges, start, backPath, sharedHomeConnectorM)

	fullPath := append([]int{}, outPath...)
	if len(backPath) > 1 {
		fullPath = append(fullPath, backPath[1:]...)
	}
	edges := append([]GraphEdge{}, outEdges...)
	edges = append(edges, backEdges...)

	return buildLocalCandidate(fullPath, edges, targetM, surfacePolicy, sharedConnectorEdges, history)
}

func buildLocalCandidate(path []int, edges []GraphEdge, targetM float64, surfacePolicy string, allowedRepeatedEdges map[edgeKey]struct{}, history historyOverlay) (localCandidate, error) {
	if len(path) < 2 {
		return localCandidate{}, errors.New("route path is too short")
	}
	total, taggedPaved, unpaved, unknown := edgeStats(edges)
	if total == 0 {
		return localCandidate{}, errors.New("route has zero distance")
	}
	if total < targetM || total > targetM+500 {
		return localCandidate{}, errors.New("route is outside requested distance bounds")
	}
	if hasRepeatedEdgesExcept(path, allowedRepeatedEdges) {
		return localCandidate{}, errors.New("route repeats a road segment")
	}
	effectivePaved := taggedPaved
	if surfacePolicy == SurfacePolicyAssumePaved {
		effectivePaved += unknown
	}
	previouslyRun := historyDistance(path, edges, history.AllEdges)
	recentlyRun := historyDistance(path, edges, history.RecentEdges)
	return localCandidate{
		Path:                       path,
		DistanceM:                  total,
		PavedPercent:               effectivePaved / total * 100,
		TaggedPavedPercent:         taggedPaved / total * 100,
		UnpavedPercent:             unpaved / total * 100,
		UnknownPercent:             unknown / total * 100,
		UnrunPercent:               math.Max(0, total-previouslyRun) / total * 100,
		PreviouslyRunPercent:       previouslyRun / total * 100,
		RecentPreviouslyRunPercent: recentlyRun / total * 100,
	}, nil
}

func useCycleGenerator(targetM float64) bool {
	return targetM >= 12000
}

func waypointCountForTarget(targetM float64, rng *rand.Rand) int {
	if targetM < 12000 {
		return 3
	}
	base := int(math.Round(targetM / 3000))
	minCount := maxInt(5, minInt(base, 8))
	maxCount := 8
	if minCount >= maxCount {
		return maxCount
	}
	return minCount + rng.Intn(maxCount-minCount+1)
}

func routeCandidateAttempts(targetM float64) int {
	if targetM >= 12000 {
		return 600
	}
	return 100
}

func historyDistance(path []int, edges []GraphEdge, historyEdges map[edgeKey]struct{}) float64 {
	if len(historyEdges) == 0 {
		return 0
	}
	total := 0.0
	for i := 1; i < len(path) && i-1 < len(edges); i++ {
		if _, ok := historyEdges[newEdgeKey(path[i-1], path[i])]; ok {
			total += edges[i-1].Distance
		}
	}
	return total
}

func hasRepeatedEdges(path []int) bool {
	return hasRepeatedEdgesExcept(path, nil)
}

func hasRepeatedEdgesExcept(path []int, allowed map[edgeKey]struct{}) bool {
	seen := make(map[edgeKey]struct{})
	for i := 1; i < len(path); i++ {
		key := newEdgeKey(path[i-1], path[i])
		if _, ok := allowed[key]; ok {
			continue
		}
		if _, ok := seen[key]; ok {
			return true
		}
		seen[key] = struct{}{}
	}
	return false
}

func addBlockedPathEdges(blocked map[edgeKey]struct{}, path []int) {
	addBlockedPathEdgesExcept(blocked, path, nil)
}

func addBlockedPathEdgesExcept(blocked map[edgeKey]struct{}, path []int, allowed map[edgeKey]struct{}) {
	for i := 1; i < len(path); i++ {
		key := newEdgeKey(path[i-1], path[i])
		if _, ok := allowed[key]; ok {
			continue
		}
		blocked[key] = struct{}{}
	}
}

func (g *Graph) addHomeConnectorEdgesNearStart(allowed map[edgeKey]struct{}, start int, path []int, maxDistanceM float64) {
	for i := 1; i < len(path); i++ {
		if g.nodeDistanceM(start, path[i-1]) > maxDistanceM || g.nodeDistanceM(start, path[i]) > maxDistanceM {
			continue
		}
		allowed[newEdgeKey(path[i-1], path[i])] = struct{}{}
	}
}

func (g *Graph) nodeDistanceM(a int, b int) float64 {
	from := g.Nodes[a]
	to := g.Nodes[b]
	return distanceM(from.Lat, from.Lon, to.Lat, to.Lon)
}

type edgeKey struct {
	A int
	B int
}

func newEdgeKey(a int, b int) edgeKey {
	if a > b {
		a, b = b, a
	}
	return edgeKey{A: a, B: b}
}

func (g *Graph) nearestNode(coord planner.Coordinate) int {
	bestIdx := -1
	bestDist := math.MaxFloat64
	for i, node := range g.Nodes {
		dist := distanceM(coord.Lat, coord.Lon, node.Lat, node.Lon)
		if dist < bestDist {
			bestDist = dist
			bestIdx = i
		}
	}
	return bestIdx
}

func emptyHistoryOverlay() historyOverlay {
	return historyOverlay{
		AllEdges:    make(map[edgeKey]struct{}),
		RecentEdges: make(map[edgeKey]struct{}),
	}
}

func (g *Graph) historyEdges(store *history.Store) (historyOverlay, error) {
	started := time.Now()
	activities, err := store.Activities()
	if err != nil {
		return historyOverlay{}, err
	}
	overlay := emptyHistoryOverlay()
	if len(activities) == 0 || len(g.Nodes) == 0 {
		return overlay, nil
	}

	index := newNodeGridIndex(g.Nodes)
	bounds := g.coordinateBounds(0.002)
	recentCount := minInt(10, len(activities))
	totalPoints := 0
	inBoundsPoints := 0
	sampledPoints := 0
	snappedPoints := 0
	recentSnappedPoints := 0
	for activityIdx, activity := range activities {
		recent := activityIdx < recentCount
		var lastSample planner.Coordinate
		hasLastSample := false
		for _, coord := range activity.Coordinates {
			totalPoints++
			if !bounds.containsCoordinate(coord) {
				continue
			}
			inBoundsPoints++
			if hasLastSample && distanceM(lastSample.Lat, lastSample.Lon, coord.Lat, coord.Lon) < 25 {
				continue
			}
			lastSample = coord
			hasLastSample = true
			sampledPoints++
			node := index.nearest(g, coord)
			if node < 0 {
				continue
			}
			beforeAll := len(overlay.AllEdges)
			beforeRecent := len(overlay.RecentEdges)
			g.markEdgesNearCoordinate(overlay.AllEdges, node, coord, 45)
			if recent {
				g.markEdgesNearCoordinate(overlay.RecentEdges, node, coord, 45)
			}
			if len(overlay.AllEdges) > beforeAll {
				snappedPoints++
			}
			if len(overlay.RecentEdges) > beforeRecent {
				recentSnappedPoints++
			}
		}
	}
	overlay.RecentActivities = recentCount
	log.Printf("local-osm history overlay built: total=%s activities=%d recent_activities=%d total_points=%d in_bounds=%d sampled=%d snapped=%d recent_snapped=%d used_edges=%d recent_used_edges=%d route_nodes=%d route_directed_edges=%d",
		time.Since(started).Round(time.Millisecond),
		len(activities),
		recentCount,
		totalPoints,
		inBoundsPoints,
		sampledPoints,
		snappedPoints,
		recentSnappedPoints,
		len(overlay.AllEdges),
		len(overlay.RecentEdges),
		len(g.Nodes),
		g.directedEdgeCount(),
	)
	return overlay, nil
}

type coordinateBounds struct {
	MinLat float64
	MaxLat float64
	MinLon float64
	MaxLon float64
}

func (g *Graph) coordinateBounds(margin float64) coordinateBounds {
	bounds := coordinateBounds{
		MinLat: math.MaxFloat64,
		MaxLat: -math.MaxFloat64,
		MinLon: math.MaxFloat64,
		MaxLon: -math.MaxFloat64,
	}
	for _, node := range g.Nodes {
		bounds.MinLat = math.Min(bounds.MinLat, node.Lat)
		bounds.MaxLat = math.Max(bounds.MaxLat, node.Lat)
		bounds.MinLon = math.Min(bounds.MinLon, node.Lon)
		bounds.MaxLon = math.Max(bounds.MaxLon, node.Lon)
	}
	bounds.MinLat -= margin
	bounds.MaxLat += margin
	bounds.MinLon -= margin
	bounds.MaxLon += margin
	return bounds
}

func (b coordinateBounds) containsCoordinate(coord planner.Coordinate) bool {
	return coord.Lat >= b.MinLat && coord.Lat <= b.MaxLat && coord.Lon >= b.MinLon && coord.Lon <= b.MaxLon
}

func (g *Graph) markEdgesNearCoordinate(used map[edgeKey]struct{}, node int, coord planner.Coordinate, maxDistanceM float64) {
	bestKey := edgeKey{}
	bestDist := math.MaxFloat64
	for _, edge := range g.Edges[node] {
		from := g.Nodes[node]
		to := g.Nodes[edge.To]
		dist := pointSegmentDistanceM(coord.Lat, coord.Lon, from.Lat, from.Lon, to.Lat, to.Lon)
		if dist < bestDist {
			bestDist = dist
			bestKey = newEdgeKey(node, edge.To)
		}
	}
	if bestDist <= maxDistanceM {
		used[bestKey] = struct{}{}
	}
}

type nodeGridIndex struct {
	cellSize float64
	cells    map[gridCell][]int
}

type gridCell struct {
	Lat int
	Lon int
}

func newNodeGridIndex(nodes []GraphNode) nodeGridIndex {
	index := nodeGridIndex{
		cellSize: 0.002,
		cells:    make(map[gridCell][]int),
	}
	for idx, node := range nodes {
		cell := index.cell(node.Lat, node.Lon)
		index.cells[cell] = append(index.cells[cell], idx)
	}
	return index
}

func (idx nodeGridIndex) nearest(g *Graph, coord planner.Coordinate) int {
	base := idx.cell(coord.Lat, coord.Lon)
	bestIdx := -1
	bestDist := math.MaxFloat64
	for radius := 0; radius <= 4; radius++ {
		for lat := base.Lat - radius; lat <= base.Lat+radius; lat++ {
			for lon := base.Lon - radius; lon <= base.Lon+radius; lon++ {
				if radius > 0 && lat > base.Lat-radius && lat < base.Lat+radius && lon > base.Lon-radius && lon < base.Lon+radius {
					continue
				}
				for _, nodeIdx := range idx.cells[gridCell{Lat: lat, Lon: lon}] {
					node := g.Nodes[nodeIdx]
					dist := distanceM(coord.Lat, coord.Lon, node.Lat, node.Lon)
					if dist < bestDist {
						bestDist = dist
						bestIdx = nodeIdx
					}
				}
			}
		}
		if bestIdx >= 0 {
			return bestIdx
		}
	}
	return g.nearestNode(coord)
}

func (idx nodeGridIndex) cell(lat float64, lon float64) gridCell {
	return gridCell{
		Lat: int(math.Floor(lat / idx.cellSize)),
		Lon: int(math.Floor(lon / idx.cellSize)),
	}
}

type routeSearchBounds struct {
	MinLat   float64
	MaxLat   float64
	MinLon   float64
	MaxLon   float64
	MaxCostM float64
}

func (b *routeSearchBounds) expandToward(origin GraphNode, bearing float64, distanceM float64) *routeSearchBounds {
	if b == nil {
		return nil
	}
	lat, lon := project(origin.Lat, origin.Lon, bearing, distanceM)
	b.MinLat = math.Min(b.MinLat, lat)
	b.MaxLat = math.Max(b.MaxLat, lat)
	b.MinLon = math.Min(b.MinLon, lon)
	b.MaxLon = math.Max(b.MaxLon, lon)
	return b
}

func (g *Graph) newRouteSearchBounds(points []int, targetM float64, minPavedPercent float64, pavedOnly bool, surfacePolicy string) *routeSearchBounds {
	if len(points) == 0 {
		return nil
	}
	bounds := &routeSearchBounds{
		MinLat: math.MaxFloat64,
		MaxLat: -math.MaxFloat64,
		MinLon: math.MaxFloat64,
		MaxLon: -math.MaxFloat64,
	}
	sumLat := 0.0
	for _, point := range points {
		node := g.Nodes[point]
		bounds.MinLat = math.Min(bounds.MinLat, node.Lat)
		bounds.MaxLat = math.Max(bounds.MaxLat, node.Lat)
		bounds.MinLon = math.Min(bounds.MinLon, node.Lon)
		bounds.MaxLon = math.Max(bounds.MaxLon, node.Lon)
		sumLat += node.Lat
	}

	marginM := math.Max(2000, targetM*0.5)
	latMargin := marginM / 111_320
	avgLat := sumLat / float64(len(points))
	lonMeters := 111_320 * math.Cos(degreesToRadians(avgLat))
	if math.Abs(lonMeters) < 1 {
		lonMeters = 1
	}
	lonMargin := marginM / lonMeters

	bounds.MinLat -= latMargin
	bounds.MaxLat += latMargin
	bounds.MinLon -= lonMargin
	bounds.MaxLon += lonMargin
	bounds.MaxCostM = (targetM + 500) * maxAllowedSurfaceWeight(minPavedPercent, pavedOnly, surfacePolicy)
	return bounds
}

func (b *routeSearchBounds) contains(node GraphNode) bool {
	if b == nil {
		return true
	}
	return node.Lat >= b.MinLat && node.Lat <= b.MaxLat && node.Lon >= b.MinLon && node.Lon <= b.MaxLon
}

func maxAllowedSurfaceWeight(minPavedPercent float64, pavedOnly bool, surfacePolicy string) float64 {
	if !pavedOnly {
		return surfaceWeight(SurfaceUnpaved, minPavedPercent)
	}
	if surfacePolicy == SurfacePolicyAssumePaved {
		return surfaceWeight(SurfaceUnknown, minPavedPercent)
	}
	return surfaceWeight(SurfacePaved, minPavedPercent)
}

func (g *Graph) shortestPath(start int, goal int, minPavedPercent float64, pavedOnly bool, surfacePolicy string, blockedEdges map[edgeKey]struct{}, avoidEdges map[edgeKey]struct{}, search *searchWorkspace) ([]int, []GraphEdge, error) {
	if start == goal {
		return []int{start}, nil, nil
	}
	stats := search.Stats
	if stats != nil {
		stats.SearchCalls++
	}
	search.Reset()
	search.Dist[start] = 0
	search.Prev[start] = -1
	search.Touched = append(search.Touched, start)
	pq := priorityQueue{{Node: start, Priority: heuristicM(g, start, goal)}}
	heap.Init(&pq)

	for pq.Len() > 0 {
		if stats != nil && pq.Len() > stats.MaxQueueLen {
			stats.MaxQueueLen = pq.Len()
		}
		item := heap.Pop(&pq).(queueItem)
		expectedPriority := search.Dist[item.Node] + heuristicM(g, item.Node, goal)
		if item.Priority > expectedPriority+0.000001 {
			if stats != nil {
				stats.StaleQueueSkips++
			}
			continue
		}
		if search.Closed[item.Node] {
			if stats != nil {
				stats.ClosedQueueSkips++
			}
			continue
		}
		search.Closed[item.Node] = true
		if stats != nil {
			stats.SettledNodes++
			if stats.SettledNodes%1024 == 0 && search.deadlineExpired() {
				stats.TimedOut = true
				return nil, nil, errRouteSearchTimedOut
			}
		}
		if item.Node == goal {
			break
		}
		for _, edge := range g.Edges[item.Node] {
			if !usableSurface(edge.Surface, pavedOnly, surfacePolicy) {
				continue
			}
			if _, blocked := blockedEdges[newEdgeKey(item.Node, edge.To)]; blocked {
				continue
			}
			if search.Closed[edge.To] {
				continue
			}
			if search.Bounds != nil && !search.Bounds.contains(g.Nodes[edge.To]) {
				if stats != nil {
					stats.BoundSkips++
				}
				continue
			}
			cost := edge.Distance * surfaceWeight(edge.Surface, minPavedPercent)
			if _, avoid := avoidEdges[newEdgeKey(item.Node, edge.To)]; avoid {
				cost *= recentRoadPenaltyWeight
			}
			next := search.Dist[item.Node] + cost
			maxCostM := searchMaxCostM(search.Bounds, len(avoidEdges) > 0)
			if maxCostM > 0 && next > maxCostM {
				if stats != nil {
					stats.CostSkips++
				}
				continue
			}
			if next < search.Dist[edge.To] {
				if search.Dist[edge.To] == math.MaxFloat64 {
					search.Touched = append(search.Touched, edge.To)
				}
				if stats != nil {
					stats.TouchedNodes++
				}
				search.Dist[edge.To] = next
				search.Prev[edge.To] = item.Node
				search.PrevEdge[edge.To] = edge
				heap.Push(&pq, queueItem{Node: edge.To, Priority: next + heuristicM(g, edge.To, goal)})
			}
		}
	}
	if search.Prev[goal] == -1 {
		return nil, nil, errors.New("no path between selected local OSM waypoints")
	}

	var path []int
	var edges []GraphEdge
	for at := goal; at != -1; at = search.Prev[at] {
		path = append(path, at)
		if at != start {
			edges = append(edges, search.PrevEdge[at])
		}
		if at == start {
			break
		}
	}
	reverseInts(path)
	reverseEdges(edges)
	return path, edges, nil
}

func searchMaxCostM(bounds *routeSearchBounds, avoidingRecentRoads bool) float64 {
	if bounds == nil || bounds.MaxCostM <= 0 {
		return 0
	}
	if avoidingRecentRoads {
		return bounds.MaxCostM * recentRoadPenaltyWeight
	}
	return bounds.MaxCostM
}

type searchWorkspace struct {
	Dist     []float64
	Prev     []int
	PrevEdge []GraphEdge
	Closed   []bool
	Touched  []int
	Bounds   *routeSearchBounds
	Deadline time.Time
	Stats    *routeSearchStats
}

func (g *Graph) newSearchWorkspace() *searchWorkspace {
	search := &searchWorkspace{
		Dist:     make([]float64, len(g.Nodes)),
		Prev:     make([]int, len(g.Nodes)),
		PrevEdge: make([]GraphEdge, len(g.Nodes)),
		Closed:   make([]bool, len(g.Nodes)),
	}
	for i := range search.Dist {
		search.Dist[i] = math.MaxFloat64
		search.Prev[i] = -1
	}
	return search
}

func (s *searchWorkspace) Reset() {
	for _, idx := range s.Touched {
		s.Dist[idx] = math.MaxFloat64
		s.Prev[idx] = -1
		s.PrevEdge[idx] = GraphEdge{}
		s.Closed[idx] = false
	}
	s.Touched = s.Touched[:0]
}

func (s *searchWorkspace) deadlineExpired() bool {
	return !s.Deadline.IsZero() && time.Now().After(s.Deadline)
}

type routeSearchStats struct {
	CandidateFailures int
	TimedOut          bool
	SearchCalls       int
	SettledNodes      int
	TouchedNodes      int
	StaleQueueSkips   int
	ClosedQueueSkips  int
	BoundSkips        int
	CostSkips         int
	MaxQueueLen       int
}

func heuristicM(g *Graph, from int, to int) float64 {
	a := g.Nodes[from]
	b := g.Nodes[to]
	return distanceM(a.Lat, a.Lon, b.Lat, b.Lon)
}

func surfaceWeight(surface int, minPavedPercent float64) float64 {
	if minPavedPercent <= 0 {
		return 1
	}
	switch surface {
	case SurfacePaved:
		return 1
	case SurfaceUnknown:
		return 1 + minPavedPercent/80
	case SurfaceUnpaved:
		return 1 + minPavedPercent/25
	default:
		return 1
	}
}

func usableSurface(surface int, pavedOnly bool, surfacePolicy string) bool {
	if !pavedOnly {
		return true
	}
	if surface == SurfacePaved {
		return true
	}
	return surface == SurfaceUnknown && surfacePolicy == SurfacePolicyAssumePaved
}

func localScore(candidate localCandidate, targetM float64, minPavedPercent float64, preferUnrunRoads bool) float64 {
	shortPenalty := math.Max(0, targetM-candidate.DistanceM) / targetM * 1000
	extraMeters := math.Max(0, candidate.DistanceM-targetM)
	extraPenalty := extraMeters / targetM
	if extraMeters > 500 {
		extraPenalty += (extraMeters - 500) / 10
	}
	pavedPenalty := math.Abs(candidate.PavedPercent-minPavedPercent) * 10
	if candidate.PavedPercent < minPavedPercent {
		pavedPenalty *= 4
	}
	historyPenalty := 0.0
	if preferUnrunRoads {
		historyPenalty = candidate.PreviouslyRunPercent*8 + candidate.RecentPreviouslyRunPercent*45
	}
	return shortPenalty + extraPenalty + pavedPenalty + historyPenalty
}

func edgeStats(edges []GraphEdge) (total float64, paved float64, unpaved float64, unknown float64) {
	for _, edge := range edges {
		total += edge.Distance
		switch edge.Surface {
		case SurfacePaved:
			paved += edge.Distance
		case SurfaceUnpaved:
			unpaved += edge.Distance
		default:
			unknown += edge.Distance
		}
	}
	return total, paved, unpaved, unknown
}

type queueItem struct {
	Node     int
	Priority float64
}

type priorityQueue []queueItem

func (pq priorityQueue) Len() int           { return len(pq) }
func (pq priorityQueue) Less(i, j int) bool { return pq[i].Priority < pq[j].Priority }
func (pq priorityQueue) Swap(i, j int)      { pq[i], pq[j] = pq[j], pq[i] }

func (pq *priorityQueue) Push(x any) {
	*pq = append(*pq, x.(queueItem))
}

func (pq *priorityQueue) Pop() any {
	old := *pq
	item := old[len(old)-1]
	*pq = old[:len(old)-1]
	return item
}

func reverseInts(values []int) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

func reverseEdges(values []GraphEdge) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

func distanceKm(a planner.Coordinate, b planner.Coordinate) float64 {
	return distanceM(a.Lat, a.Lon, b.Lat, b.Lon) / 1000
}

func distanceM(lat1 float64, lon1 float64, lat2 float64, lon2 float64) float64 {
	const earthRadiusM = 6371000
	phi1 := degreesToRadians(lat1)
	phi2 := degreesToRadians(lat2)
	dPhi := degreesToRadians(lat2 - lat1)
	dLambda := degreesToRadians(lon2 - lon1)
	a := math.Sin(dPhi/2)*math.Sin(dPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(dLambda/2)*math.Sin(dLambda/2)
	return earthRadiusM * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func project(lat float64, lon float64, bearing float64, distanceM float64) (float64, float64) {
	const earthRadiusM = 6371000
	lat1 := degreesToRadians(lat)
	lon1 := degreesToRadians(lon)
	angularDistance := distanceM / earthRadiusM
	lat2 := math.Asin(math.Sin(lat1)*math.Cos(angularDistance) + math.Cos(lat1)*math.Sin(angularDistance)*math.Cos(bearing))
	lon2 := lon1 + math.Atan2(math.Sin(bearing)*math.Sin(angularDistance)*math.Cos(lat1), math.Cos(angularDistance)-math.Sin(lat1)*math.Sin(lat2))
	return radiansToDegrees(lat2), radiansToDegrees(lon2)
}

func pointSegmentDistanceM(pointLat float64, pointLon float64, aLat float64, aLon float64, bLat float64, bLon float64) float64 {
	const earthRadiusM = 6371000
	originLat := degreesToRadians(pointLat)
	toXY := func(lat float64, lon float64) (float64, float64) {
		x := degreesToRadians(lon-pointLon) * math.Cos(originLat) * earthRadiusM
		y := degreesToRadians(lat-pointLat) * earthRadiusM
		return x, y
	}
	ax, ay := toXY(aLat, aLon)
	bx, by := toXY(bLat, bLon)
	dx := bx - ax
	dy := by - ay
	lengthSquared := dx*dx + dy*dy
	if lengthSquared == 0 {
		return math.Hypot(ax, ay)
	}
	t := -(ax*dx + ay*dy) / lengthSquared
	t = math.Max(0, math.Min(1, t))
	closestX := ax + t*dx
	closestY := ay + t*dy
	return math.Hypot(closestX, closestY)
}

func bearingRadians(lat1 float64, lon1 float64, lat2 float64, lon2 float64) float64 {
	phi1 := degreesToRadians(lat1)
	phi2 := degreesToRadians(lat2)
	dLambda := degreesToRadians(lon2 - lon1)
	y := math.Sin(dLambda) * math.Cos(phi2)
	x := math.Cos(phi1)*math.Sin(phi2) - math.Sin(phi1)*math.Cos(phi2)*math.Cos(dLambda)
	return normalizeBearing(math.Atan2(y, x))
}

func normalizeBearing(value float64) float64 {
	value = math.Mod(value, 2*math.Pi)
	if value < 0 {
		value += 2 * math.Pi
	}
	return value
}

func angularDifference(a float64, b float64) float64 {
	diff := math.Abs(normalizeBearing(a) - normalizeBearing(b))
	if diff > math.Pi {
		return 2*math.Pi - diff
	}
	return diff
}

func degreesToRadians(value float64) float64 {
	return value * math.Pi / 180
}

func radiansToDegrees(value float64) float64 {
	return value * 180 / math.Pi
}

func round(value float64, places int) float64 {
	pow := math.Pow(10, float64(places))
	return math.Round(value*pow) / pow
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}
