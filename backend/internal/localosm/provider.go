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

	"github.com/EDessin/RouteRoulette/backend/internal/avoidance"
	"github.com/EDessin/RouteRoulette/backend/internal/history"
	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/EDessin/RouteRoulette/backend/internal/surfacemarks"
	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
)

const (
	SurfacePaved = iota
	SurfaceUnpaved
	SurfaceUnknown

	SurfacePolicyStrict      = "strict"
	SurfacePolicyAssumePaved = "assume_paved"

	defaultRouteGenerationBudget = 10 * time.Second
	surfaceRouteGenerationBudget = 25 * time.Second
	preferredKnownPavedTarget    = 95.0
	preferredKnownUnpavedTarget  = 50.0
	maxPavedWhenPreferUnpaved    = 30.0
	lowKnownSurfaceDataThreshold = 20.0
	recentRoadPenaltyWeight      = 3
	avoidedRoadPenaltyWeight     = 50
	maxAllowedAvoidedRoadM       = 50
	sharedHomeConnectorM         = 200
	graphCacheVersion            = 2
)

var errRouteSearchTimedOut = errors.New("route search timed out")

var preferUnpavedFallbackTargets = []float64{preferredKnownUnpavedTarget, 40, 30, 20}

type Config struct {
	DataDir        string
	ExtractPath    string
	ExtractURL     string
	RadiusKm       float64
	AllowDownload  bool
	HistoryStore   *history.Store
	AvoidanceStore *avoidance.Store
	SurfaceStore   *surfacemarks.Store
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

type avoidanceOverlay struct {
	RoadsByWayID map[int64]avoidance.Road
}

type surfaceOverlay struct {
	RoadsByWayID map[int64]surfacemarks.Road
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
	routeSurfaceMarks := emptySurfaceOverlay()
	if p.cfg.SurfaceStore != nil {
		routeSurfaceMarks, err = p.loadSurfaceMarks()
		if err != nil {
			return planner.CandidateRoute{}, err
		}
		routeGraph.applySurfaceMarks(routeSurfaceMarks)
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
	routeAvoidance := emptyAvoidanceOverlay()
	avoidanceDuration := time.Duration(0)
	if p.cfg.AvoidanceStore != nil {
		avoidanceStarted := time.Now()
		routeAvoidance, err = p.loadAvoidedRoads()
		if err != nil {
			return planner.CandidateRoute{}, err
		}
		avoidanceDuration = time.Since(avoidanceStarted)
	}

	loopStarted := time.Now()
	route, err := routeGraph.GenerateLoop(req, routeHistory, routeAvoidance)
	if err != nil {
		log.Printf("local-osm route failed: total=%s graph_load=%s subgraph=%s history=%s avoidance=%s loop=%s full_nodes=%d full_directed_edges=%d route_nodes=%d route_directed_edges=%d subgraph_radius_km=%.1f route_history_edges=%d recent_history_edges=%d recent_activities=%d avoided_roads=%d err=%v",
			time.Since(started).Round(time.Millisecond),
			graphLoadDuration.Round(time.Millisecond),
			subgraphDuration.Round(time.Millisecond),
			historyDuration.Round(time.Millisecond),
			avoidanceDuration.Round(time.Millisecond),
			time.Since(loopStarted).Round(time.Millisecond),
			len(graph.Nodes),
			graph.directedEdgeCount(),
			len(routeGraph.Nodes),
			routeGraph.directedEdgeCount(),
			subgraphRadiusKm,
			len(routeHistory.AllEdges),
			len(routeHistory.RecentEdges),
			routeHistory.RecentActivities,
			len(routeAvoidance.RoadsByWayID),
			err,
		)
		return planner.CandidateRoute{}, err
	}
	log.Printf("local-osm route generated: total=%s graph_load=%s subgraph=%s history=%s avoidance=%s loop=%s full_nodes=%d full_directed_edges=%d route_nodes=%d route_directed_edges=%d subgraph_radius_km=%.1f route_history_edges=%d recent_history_edges=%d recent_activities=%d avoided_roads=%d distance_km=%.2f",
		time.Since(started).Round(time.Millisecond),
		graphLoadDuration.Round(time.Millisecond),
		subgraphDuration.Round(time.Millisecond),
		historyDuration.Round(time.Millisecond),
		avoidanceDuration.Round(time.Millisecond),
		time.Since(loopStarted).Round(time.Millisecond),
		len(graph.Nodes),
		graph.directedEdgeCount(),
		len(routeGraph.Nodes),
		routeGraph.directedEdgeCount(),
		subgraphRadiusKm,
		len(routeHistory.AllEdges),
		len(routeHistory.RecentEdges),
		routeHistory.RecentActivities,
		len(routeAvoidance.RoadsByWayID),
		route.DistanceM/1000,
	)
	return route, nil
}

func (p Provider) ImportRoute(ctxReq *http.Request, req planner.ImportRouteRequest) (planner.CandidateRoute, error) {
	started := time.Now()
	home := req.Coordinates[0]
	graph, err := p.loadGraph(ctxReq.Context(), home)
	if err != nil {
		return planner.CandidateRoute{}, err
	}

	rawDistanceM := coordinatePathDistanceM(req.Coordinates)
	routeGraph, subgraphRadiusKm, err := graph.routeSubgraph(home, rawDistanceM, false, SurfacePolicyAssumePaved)
	if err != nil {
		return planner.CandidateRoute{}, err
	}
	if p.cfg.SurfaceStore != nil {
		marks, err := p.loadSurfaceMarks()
		if err != nil {
			return planner.CandidateRoute{}, err
		}
		routeGraph.applySurfaceMarks(marks)
	}

	route, err := routeGraph.importCoordinateRoute(req.Coordinates)
	if err != nil {
		return planner.CandidateRoute{}, err
	}
	log.Printf("local-osm route imported: total=%s full_nodes=%d full_directed_edges=%d route_nodes=%d route_directed_edges=%d subgraph_radius_km=%.1f raw_distance_km=%.2f snapped_distance_km=%.2f",
		time.Since(started).Round(time.Millisecond),
		len(graph.Nodes),
		graph.directedEdgeCount(),
		len(routeGraph.Nodes),
		routeGraph.directedEdgeCount(),
		subgraphRadiusKm,
		rawDistanceM/1000,
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

func (p Provider) loadAvoidedRoads() (avoidanceOverlay, error) {
	roads, err := p.cfg.AvoidanceStore.List()
	if err != nil {
		return avoidanceOverlay{}, err
	}
	return avoidanceOverlay{RoadsByWayID: avoidance.ByWayID(roads)}, nil
}

func (p Provider) loadSurfaceMarks() (surfaceOverlay, error) {
	roads, err := p.cfg.SurfaceStore.List()
	if err != nil {
		return surfaceOverlay{}, err
	}
	return surfaceOverlay{RoadsByWayID: surfacemarks.ByWayID(roads)}, nil
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
	Version   int                `json:"version"`
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
	OSMWayID int64   `json:"osmWayId,omitempty"`
	Name     string  `json:"name,omitempty"`
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
	if graph.Version != graphCacheVersion {
		return nil, errors.New("graph cache version is outdated")
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
		Version:   graphCacheVersion,
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
		name := strings.TrimSpace(tags["name"])
		wayID := int64(way.ID)
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
			graph.Edges[a] = append(graph.Edges[a], GraphEdge{To: b, Distance: dist, Surface: surface, OSMWayID: wayID, Name: name})
			graph.Edges[b] = append(graph.Edges[b], GraphEdge{To: a, Distance: dist, Surface: surface, OSMWayID: wayID, Name: name})
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
		Version:   graphCacheVersion,
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
				OSMWayID: edge.OSMWayID,
				Name:     edge.Name,
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

func (g *Graph) GenerateLoop(req planner.CandidateRequest, history historyOverlay, avoided avoidanceOverlay) (planner.CandidateRoute, error) {
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
	attempts := routeCandidateAttempts(req.TargetDistanceM, hasSurfacePreference(req))
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
	mediumCycleAnchors := waypointSet{}
	if useMediumLoopGenerator(req.TargetDistanceM) {
		mediumCycleAnchors = g.newMediumCycleAnchorSet(start, req.TargetDistanceM, pavedOnly, policy)
	}
	shortCycleAnchors := waypointSet{}
	if useShortLoopGenerator(req.TargetDistanceM) {
		shortCycleAnchors = g.newShortCycleAnchorSet(start, req.TargetDistanceM, pavedOnly, policy)
	}
	search := g.newSearchWorkspace()
	stats := &routeSearchStats{}
	search.Stats = stats
	budget := routeGenerationBudget(req)
	deadline := started.Add(budget)
	search.Deadline = deadline
	successes := 0
	acceptedUnpavedTarget := preferredKnownUnpavedTarget
	for i := 0; i < attempts; i++ {
		if i > 0 && time.Now().After(deadline) {
			stats.TimedOut = true
			break
		}
		unpavedTarget := unpavedTargetForAttempt(req, i, attempts)
		if len(best.Path) > 0 && best.DistanceM >= req.TargetDistanceM && best.DistanceM <= req.TargetDistanceM+500 && surfacePreferenceSatisfied(best, req, unpavedTarget) {
			acceptedUnpavedTarget = unpavedTarget
			break
		}
		var candidate localCandidate
		var err error
		if useShortLoopGenerator(req.TargetDistanceM) && len(shortCycleAnchors.Nodes) > 0 && i%3 == 0 {
			candidate, err = g.cycleCandidate(start, req.TargetDistanceM, req.MinPavedPercent, req.PreferUnpaved, pavedOnly, policy, rng, shortCycleAnchors, history, avoided, search)
		} else if useShortLoopGenerator(req.TargetDistanceM) && len(shortCycleAnchors.Nodes) > 0 && i%3 == 1 {
			candidate, err = g.blockLoopCandidate(start, req.TargetDistanceM, req.MinPavedPercent, req.PreferUnpaved, pavedOnly, policy, rng, shortCycleAnchors, history, avoided, search)
		} else if useMediumLoopGenerator(req.TargetDistanceM) && len(mediumCycleAnchors.Nodes) > 0 && i%3 == 0 {
			candidate, err = g.cycleCandidate(start, req.TargetDistanceM, req.MinPavedPercent, req.PreferUnpaved, pavedOnly, policy, rng, mediumCycleAnchors, history, avoided, search)
		} else if useMediumLoopGenerator(req.TargetDistanceM) && len(mediumCycleAnchors.Nodes) > 0 && i%3 == 1 {
			candidate, err = g.blockLoopCandidate(start, req.TargetDistanceM, req.MinPavedPercent, req.PreferUnpaved, pavedOnly, policy, rng, mediumCycleAnchors, history, avoided, search)
		} else if useCycleGenerator(req.TargetDistanceM) && len(cycleAnchors.Nodes) > 0 {
			candidate, err = g.cycleCandidate(start, req.TargetDistanceM, req.MinPavedPercent, req.PreferUnpaved, pavedOnly, policy, rng, cycleAnchors, history, avoided, search)
		} else {
			candidate, err = g.loopCandidate(start, req.TargetDistanceM, req.MinPavedPercent, req.PreferUnpaved, pavedOnly, policy, rng, waypoints, history, avoided, search)
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
		score := localScore(candidate, req.TargetDistanceM, req.MinPavedPercent, req.PreferUnrunRoads, req.PreferUnpaved)
		if score < bestScore {
			best = candidate
			bestScore = score
		}
		if candidate.DistanceM >= req.TargetDistanceM && candidate.DistanceM <= req.TargetDistanceM+500 && surfacePreferenceSatisfied(candidate, req, unpavedTarget) {
			acceptedUnpavedTarget = unpavedTarget
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
	log.Printf("local-osm loop stats: total=%s start_lookup=%s waypoint_build=%s waypoints=%d attempts=%d successes=%d failures=%d timed_out=%t search_calls=%d settled=%d touched=%d stale_skips=%d closed_skips=%d bound_skips=%d cost_skips=%d max_queue=%d best_distance_km=%.2f paved=%.1f known_paved=%.1f known_unpaved=%.1f previously_run=%.1f recent_previously_run=%.1f",
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
		best.KnownPavedPercent,
		best.KnownUnpavedPercent,
		best.PreviouslyRunPercent,
		best.RecentPreviouslyRunPercent,
	)

	paved := round(best.TaggedPavedPercent, 1)
	unpaved := round(best.UnpavedPercent, 1)
	unknown := round(best.UnknownPercent, 1)
	knownSurface := round(best.KnownSurfacePercent, 1)
	knownPaved := round(best.KnownPavedPercent, 1)
	knownUnpaved := round(best.KnownUnpavedPercent, 1)
	var unrun *float64
	var previouslyRun *float64
	if req.PreferUnrunRoads {
		unrunValue := round(best.UnrunPercent, 1)
		previouslyRunValue := round(best.PreviouslyRunPercent, 1)
		unrun = &unrunValue
		previouslyRun = &previouslyRunValue
	}
	avoidedDistance := round(best.AvoidedRoadDistanceM, 1)
	warnings := []string{}
	if best.AvoidedRoadDistanceM > 0 {
		warnings = append(warnings, fmt.Sprintf("This route uses %.0f meters of an avoided road to cross or connect briefly.", best.AvoidedRoadDistanceM))
	}
	if policy == SurfacePolicyAssumePaved && unknown > 0 && !hasSurfacePreference(req) {
		warnings = append(warnings, fmt.Sprintf("%.0f%% of this route uses roads without OSM surface tags and treats them as paved because of the selected surface-data mode.", unknown))
	}
	if stats.TimedOut {
		warnings = append(warnings, fmt.Sprintf("Route generation stopped after %.0f seconds and returned the best route found so far.", budget.Seconds()))
	}
	if req.PreferUnpaved && acceptedUnpavedTarget < preferredKnownUnpavedTarget && best.KnownUnpavedPercent < preferredKnownUnpavedTarget {
		warnings = append(warnings, fmt.Sprintf("Could not find a route at the %.0f%% known-unpaved target, so RouteRoulette accepted a fallback target of %.0f%% while keeping the paved-road and distance limits unchanged.", preferredKnownUnpavedTarget, acceptedUnpavedTarget))
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
		KnownSurfacePercent:  &knownSurface,
		KnownPavedPercent:    &knownPaved,
		KnownUnpavedPercent:  &knownUnpaved,
		UnrunPercent:         unrun,
		PreviouslyRunPercent: previouslyRun,
		AvoidedRoadDistanceM: &avoidedDistance,
		Segments:             routeSegments(best.Path, best.Edges),
		Provider:             "local-osm",
		Warnings:             warnings,
	}, nil
}

type localCandidate struct {
	Edges                      []GraphEdge
	Path                       []int
	DistanceM                  float64
	PavedPercent               float64
	TaggedPavedPercent         float64
	UnpavedPercent             float64
	UnknownPercent             float64
	KnownSurfacePercent        float64
	KnownPavedPercent          float64
	KnownUnpavedPercent        float64
	UnrunPercent               float64
	PreviouslyRunPercent       float64
	RecentPreviouslyRunPercent float64
	AvoidedRoadDistanceM       float64
}

func pavedOnly(req planner.CandidateRequest) bool {
	return false
}

func hasSurfacePreference(req planner.CandidateRequest) bool {
	return req.PreferPaved || req.PreferUnpaved
}

func routeGenerationBudget(req planner.CandidateRequest) time.Duration {
	if hasSurfacePreference(req) {
		return surfaceRouteGenerationBudget
	}
	return defaultRouteGenerationBudget
}

func surfacePolicy(req planner.CandidateRequest) string {
	if req.SurfacePolicy == "" || req.SurfacePolicy == SurfacePolicyAssumePaved {
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
	loopRadiusM := loopRadiusForTarget(targetM)
	minWaypointM := 250.0
	minRangeM := 250.0
	if useShortLoopGenerator(targetM) {
		minWaypointM = 120
		minRangeM = 120
	}
	minDistM := math.Max(minWaypointM, loopRadiusM*0.45)
	maxDistM := math.Max(minDistM+minRangeM, loopRadiusM*1.75)
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

func (g *Graph) newMediumCycleAnchorSet(start int, targetM float64, pavedOnly bool, surfacePolicy string) waypointSet {
	inComponent := g.connectedComponent(start, pavedOnly, surfacePolicy)
	minDistM := math.Max(800, targetM*0.14)
	maxDistM := math.Max(minDistM+500, targetM*0.58)
	nodes := g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, 3)
	if len(nodes) == 0 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, 2)
	}
	if len(nodes) == 0 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM*0.75, maxDistM*1.15, 1)
	}
	return waypointSet{Nodes: nodes}
}

func (g *Graph) newShortCycleAnchorSet(start int, targetM float64, pavedOnly bool, surfacePolicy string) waypointSet {
	inComponent := g.connectedComponent(start, pavedOnly, surfacePolicy)
	minDistM := math.Max(180, targetM*0.12)
	maxDistM := math.Max(minDistM+180, targetM*0.55)
	nodes := g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, 3)
	if len(nodes) == 0 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM, maxDistM, 2)
	}
	if len(nodes) == 0 {
		nodes = g.waypointNodes(start, inComponent, pavedOnly, surfacePolicy, minDistM*0.5, maxDistM*1.25, 1)
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

func (g *Graph) loopCandidate(start int, targetM float64, minPavedPercent float64, preferUnpaved bool, pavedOnly bool, surfacePolicy string, rng *rand.Rand, waypointSet waypointSet, history historyOverlay, avoided avoidanceOverlay, search *searchWorkspace) (localCandidate, error) {
	radiusM := loopRadiusForTarget(targetM)
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
	bounds := g.newRouteSearchBounds(points, targetM, minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy)

	fullPath := []int{}
	var edges []GraphEdge
	usedEdges := make(map[edgeKey]struct{})
	sharedConnectorEdges := make(map[edgeKey]struct{})
	for i := 1; i < len(points); i++ {
		search.Bounds = bounds
		path, pathEdges, err := g.shortestPath(points[i-1], points[i], minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy, usedEdges, history.RecentEdges, avoided.RoadsByWayID, search)
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

	return buildLocalCandidate(fullPath, edges, targetM, surfacePolicy, sharedConnectorEdges, history, avoided)
}

func (g *Graph) cycleCandidate(start int, targetM float64, minPavedPercent float64, preferUnpaved bool, pavedOnly bool, surfacePolicy string, rng *rand.Rand, anchors waypointSet, history historyOverlay, avoided avoidanceOverlay, search *searchWorkspace) (localCandidate, error) {
	startNode := g.Nodes[start]
	desiredBearing := rng.Float64() * 2 * math.Pi
	desiredRatio := 0.28 + rng.Float64()*0.24
	if useShortLoopGenerator(targetM) {
		desiredRatio = 0.20 + rng.Float64()*0.30
	} else if useMediumLoopGenerator(targetM) {
		desiredRatio = 0.18 + rng.Float64()*0.30
	}
	desiredDistanceM := targetM * desiredRatio
	anchor, err := anchors.pick(desiredBearing, desiredDistanceM, map[int]struct{}{start: {}}, rng)
	if err != nil {
		return localCandidate{}, err
	}

	bounds := g.newRouteSearchBounds([]int{start, anchor}, targetM, minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy)
	startBearing := bearingRadians(startNode.Lat, startNode.Lon, g.Nodes[anchor].Lat, g.Nodes[anchor].Lon)
	expandM := targetM * 0.35
	if useShortLoopGenerator(targetM) {
		expandM = targetM * 0.2
	} else if useMediumLoopGenerator(targetM) {
		expandM = targetM * 0.28
	}
	bounds = bounds.expandToward(startNode, startBearing, expandM)

	search.Bounds = bounds
	outPath, outEdges, err := g.shortestPath(start, anchor, minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy, nil, history.RecentEdges, avoided.RoadsByWayID, search)
	if err != nil {
		return localCandidate{}, err
	}

	sharedConnectorEdges := make(map[edgeKey]struct{})
	g.addHomeConnectorEdgesNearStart(sharedConnectorEdges, start, outPath, sharedHomeConnectorM)
	blocked := make(map[edgeKey]struct{})
	addBlockedPathEdgesExcept(blocked, outPath, sharedConnectorEdges)

	search.Bounds = bounds
	backPath, backEdges, err := g.shortestPath(anchor, start, minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy, blocked, history.RecentEdges, avoided.RoadsByWayID, search)
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

	return buildLocalCandidate(fullPath, edges, targetM, surfacePolicy, sharedConnectorEdges, history, avoided)
}

func (g *Graph) blockLoopCandidate(start int, targetM float64, minPavedPercent float64, preferUnpaved bool, pavedOnly bool, surfacePolicy string, rng *rand.Rand, anchors waypointSet, history historyOverlay, avoided avoidanceOverlay, search *searchWorkspace) (localCandidate, error) {
	startNode := g.Nodes[start]
	desiredBearing := rng.Float64() * 2 * math.Pi
	desiredDistanceM := targetM * (0.15 + rng.Float64()*0.25)
	if useMediumLoopGenerator(targetM) {
		desiredDistanceM = targetM * (0.18 + rng.Float64()*0.30)
	}
	anchor, err := anchors.pick(desiredBearing, desiredDistanceM, map[int]struct{}{start: {}}, rng)
	if err != nil {
		return localCandidate{}, err
	}

	bounds := g.newRouteSearchBounds([]int{start, anchor}, targetM, minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy)
	expandM := targetM * 0.12
	if useMediumLoopGenerator(targetM) {
		expandM = targetM * 0.22
	}
	bounds = bounds.expandToward(startNode, bearingRadians(startNode.Lat, startNode.Lon, g.Nodes[anchor].Lat, g.Nodes[anchor].Lon), expandM)

	search.Bounds = bounds
	outPath, outEdges, err := g.shortestPath(start, anchor, minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy, nil, history.RecentEdges, avoided.RoadsByWayID, search)
	if err != nil {
		return localCandidate{}, err
	}

	sharedConnectorEdges := make(map[edgeKey]struct{})
	g.addHomeConnectorEdgesNearStart(sharedConnectorEdges, start, outPath, sharedHomeConnectorM)
	blocked := make(map[edgeKey]struct{})
	addBlockedPathEdgesExcept(blocked, outPath, sharedConnectorEdges)

	search.Bounds = bounds
	backPath, backEdges, err := g.shortestPath(anchor, start, minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy, blocked, history.RecentEdges, avoided.RoadsByWayID, search)
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

	return buildLocalCandidate(fullPath, edges, targetM, surfacePolicy, sharedConnectorEdges, history, avoided)
}

func buildLocalCandidate(path []int, edges []GraphEdge, targetM float64, surfacePolicy string, allowedRepeatedEdges map[edgeKey]struct{}, history historyOverlay, avoided avoidanceOverlay) (localCandidate, error) {
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
	avoidedDistance, err := avoidedRoadDistance(edges, avoided.RoadsByWayID)
	if err != nil {
		return localCandidate{}, err
	}
	effectivePaved := taggedPaved
	if surfacePolicy == SurfacePolicyAssumePaved {
		effectivePaved += unknown
	}
	knownSurface := taggedPaved + unpaved
	knownPavedPercent := 0.0
	knownUnpavedPercent := 0.0
	if knownSurface > 0 {
		knownPavedPercent = taggedPaved / knownSurface * 100
		knownUnpavedPercent = unpaved / knownSurface * 100
	}
	previouslyRun := historyDistance(path, edges, history.AllEdges)
	recentlyRun := historyDistance(path, edges, history.RecentEdges)
	return localCandidate{
		Edges:                      edges,
		Path:                       path,
		DistanceM:                  total,
		PavedPercent:               effectivePaved / total * 100,
		TaggedPavedPercent:         taggedPaved / total * 100,
		UnpavedPercent:             unpaved / total * 100,
		UnknownPercent:             unknown / total * 100,
		KnownSurfacePercent:        knownSurface / total * 100,
		KnownPavedPercent:          knownPavedPercent,
		KnownUnpavedPercent:        knownUnpavedPercent,
		UnrunPercent:               math.Max(0, total-previouslyRun) / total * 100,
		PreviouslyRunPercent:       previouslyRun / total * 100,
		RecentPreviouslyRunPercent: recentlyRun / total * 100,
		AvoidedRoadDistanceM:       avoidedDistance,
	}, nil
}

func useCycleGenerator(targetM float64) bool {
	return targetM > 12000
}

func useShortLoopGenerator(targetM float64) bool {
	return targetM <= 5000
}

func useMediumLoopGenerator(targetM float64) bool {
	return targetM > 5000 && targetM <= 12000
}

func loopRadiusForTarget(targetM float64) float64 {
	radius := targetM / (2 * math.Pi)
	if useShortLoopGenerator(targetM) {
		return math.Max(220, radius)
	}
	return math.Max(600, radius)
}

func waypointCountForTarget(targetM float64, rng *rand.Rand) int {
	if useShortLoopGenerator(targetM) {
		return 2
	}
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

func routeCandidateAttempts(targetM float64, surfacePreference bool) int {
	multiplier := 1
	if surfacePreference {
		multiplier = 2
	}
	if useCycleGenerator(targetM) {
		return 600 * multiplier
	}
	if useMediumLoopGenerator(targetM) {
		return 250 * multiplier
	}
	return 100 * multiplier
}

func surfacePreferenceSatisfied(candidate localCandidate, req planner.CandidateRequest, unpavedTarget float64) bool {
	if req.PreferPaved {
		return candidate.KnownSurfacePercent >= lowKnownSurfaceDataThreshold && candidate.KnownPavedPercent >= preferredKnownPavedTarget
	}
	if req.PreferUnpaved {
		return candidate.KnownSurfacePercent >= lowKnownSurfaceDataThreshold &&
			candidate.KnownUnpavedPercent >= unpavedTarget &&
			candidate.TaggedPavedPercent <= maxPavedWhenPreferUnpaved
	}
	if req.MinPavedPercent > 0 {
		return candidate.PavedPercent >= req.MinPavedPercent-5
	}
	return true
}

func unpavedTargetForAttempt(req planner.CandidateRequest, attempt int, attempts int) float64 {
	if !req.PreferUnpaved || attempts <= 0 {
		return preferredKnownUnpavedTarget
	}
	targets := preferUnpavedFallbackTargets
	if len(targets) == 0 {
		return preferredKnownUnpavedTarget
	}
	index := attempt * len(targets) / attempts
	if index >= len(targets) {
		index = len(targets) - 1
	}
	return targets[index]
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

func (g *Graph) importCoordinateRoute(coords []planner.Coordinate) (planner.CandidateRoute, error) {
	sampled := downsampleImportCoordinates(coords)
	if len(sampled) < 2 {
		return planner.CandidateRoute{}, errors.New("imported route has too few usable points")
	}

	points := make([]int, 0, len(sampled))
	lastNode := -1
	for _, coord := range sampled {
		node := g.nearestNode(coord)
		if node < 0 {
			return planner.CandidateRoute{}, errors.New("could not snap imported route to local OSM roads")
		}
		if node == lastNode {
			continue
		}
		points = append(points, node)
		lastNode = node
	}
	if len(points) < 2 {
		return planner.CandidateRoute{}, errors.New("imported route snapped to too few local OSM road points")
	}

	search := g.newSearchWorkspace()
	search.Deadline = time.Now().Add(defaultRouteGenerationBudget)
	fullPath := []int{}
	edges := []GraphEdge{}
	for i := 1; i < len(points); i++ {
		from := points[i-1]
		to := points[i]
		if from == to {
			continue
		}
		directM := g.nodeDistanceM(from, to)
		boundsTargetM := math.Max(1000, directM*4)
		search.Bounds = g.newRouteSearchBounds([]int{from, to}, boundsTargetM, 0, false, false, SurfacePolicyAssumePaved)
		path, pathEdges, err := g.shortestPath(from, to, 0, false, false, SurfacePolicyAssumePaved, nil, nil, nil, search)
		if err != nil {
			return planner.CandidateRoute{}, fmt.Errorf("could not follow imported GPX on local OSM roads: %w", err)
		}
		if i > 1 && len(path) > 0 {
			path = path[1:]
		}
		fullPath = append(fullPath, path...)
		edges = append(edges, pathEdges...)
	}
	if len(fullPath) < 2 {
		return planner.CandidateRoute{}, errors.New("imported route path is too short")
	}
	return buildImportedRoute(g, fullPath, edges)
}

func buildImportedRoute(g *Graph, path []int, edges []GraphEdge) (planner.CandidateRoute, error) {
	total, taggedPaved, unpaved, unknown := edgeStats(edges)
	if total == 0 {
		return planner.CandidateRoute{}, errors.New("imported route has zero distance")
	}

	coords := make([][]float64, 0, len(path))
	for _, idx := range path {
		node := g.Nodes[idx]
		coords = append(coords, []float64{node.Lon, node.Lat})
	}
	startNode := g.Nodes[path[0]]
	paved := round(taggedPaved/total*100, 1)
	unpavedPercent := round(unpaved/total*100, 1)
	unknownPercent := round(unknown/total*100, 1)
	knownSurface := taggedPaved + unpaved
	knownSurfacePercent := round(knownSurface/total*100, 1)
	knownPavedPercent := 0.0
	knownUnpavedPercent := 0.0
	if knownSurface > 0 {
		knownPavedPercent = round(taggedPaved/knownSurface*100, 1)
		knownUnpavedPercent = round(unpaved/knownSurface*100, 1)
	}
	avoidedDistance := 0.0
	return planner.CandidateRoute{
		Start:           planner.Coordinate{Lat: startNode.Lat, Lon: startNode.Lon},
		DistanceM:       total,
		DurationSeconds: (total / 1000) * 360,
		Geometry: planner.GeoJSONLine{
			Type:        "LineString",
			Coordinates: coords,
		},
		PavedPercent:         &paved,
		UnpavedPercent:       &unpavedPercent,
		UnknownPercent:       &unknownPercent,
		KnownSurfacePercent:  &knownSurfacePercent,
		KnownPavedPercent:    &knownPavedPercent,
		KnownUnpavedPercent:  &knownUnpavedPercent,
		AvoidedRoadDistanceM: &avoidedDistance,
		Segments:             routeSegments(path, edges),
		Provider:             "local-osm-import",
	}, nil
}

func downsampleImportCoordinates(coords []planner.Coordinate) []planner.Coordinate {
	if len(coords) <= 2 {
		return append([]planner.Coordinate{}, coords...)
	}
	totalM := coordinatePathDistanceM(coords)
	minSpacingM := math.Max(60, totalM/500)
	sampled := []planner.Coordinate{coords[0]}
	last := coords[0]
	for _, coord := range coords[1 : len(coords)-1] {
		if distanceM(last.Lat, last.Lon, coord.Lat, coord.Lon) < minSpacingM {
			continue
		}
		sampled = append(sampled, coord)
		last = coord
	}
	lastCoord := coords[len(coords)-1]
	if sampled[len(sampled)-1] != lastCoord {
		sampled = append(sampled, lastCoord)
	}
	return sampled
}

func coordinatePathDistanceM(coords []planner.Coordinate) float64 {
	total := 0.0
	for i := 1; i < len(coords); i++ {
		total += distanceM(coords[i-1].Lat, coords[i-1].Lon, coords[i].Lat, coords[i].Lon)
	}
	return total
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

func emptyAvoidanceOverlay() avoidanceOverlay {
	return avoidanceOverlay{RoadsByWayID: map[int64]avoidance.Road{}}
}

func emptySurfaceOverlay() surfaceOverlay {
	return surfaceOverlay{RoadsByWayID: map[int64]surfacemarks.Road{}}
}

func (g *Graph) applySurfaceMarks(marks surfaceOverlay) {
	if len(marks.RoadsByWayID) == 0 {
		return
	}
	for from := range g.Edges {
		for idx := range g.Edges[from] {
			mark, ok := marks.RoadsByWayID[g.Edges[from][idx].OSMWayID]
			if !ok {
				continue
			}
			switch mark.Surface {
			case surfacemarks.SurfacePaved:
				g.Edges[from][idx].Surface = SurfacePaved
			case surfacemarks.SurfaceUnpaved:
				g.Edges[from][idx].Surface = SurfaceUnpaved
			}
		}
	}
}

func avoidedRoadDistance(edges []GraphEdge, avoidedWays map[int64]avoidance.Road) (float64, error) {
	if len(avoidedWays) == 0 {
		return 0, nil
	}
	byWay := make(map[int64]float64)
	total := 0.0
	for _, edge := range edges {
		if edge.OSMWayID == 0 {
			continue
		}
		if _, ok := avoidedWays[edge.OSMWayID]; !ok {
			continue
		}
		byWay[edge.OSMWayID] += edge.Distance
		total += edge.Distance
	}
	for _, distance := range byWay {
		if distance >= maxAllowedAvoidedRoadM {
			return total, errors.New("route uses an avoided road for too long")
		}
	}
	return total, nil
}

func routeSegments(path []int, edges []GraphEdge) []planner.RouteSegment {
	segments := make([]planner.RouteSegment, 0, len(edges))
	for idx, edge := range edges {
		if idx+1 >= len(path) {
			break
		}
		segments = append(segments, planner.RouteSegment{
			FromIndex: idx,
			ToIndex:   idx + 1,
			OSMWayID:  edge.OSMWayID,
			Name:      edge.Name,
			DistanceM: round(edge.Distance, 1),
			Surface:   routeSegmentSurface(edge.Surface),
		})
	}
	return segments
}

func routeSegmentSurface(surface int) string {
	switch surface {
	case SurfacePaved:
		return "paved"
	case SurfaceUnpaved:
		return "unpaved"
	case SurfaceUnknown:
		return "unknown"
	default:
		return ""
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

func (g *Graph) newRouteSearchBounds(points []int, targetM float64, minPavedPercent float64, preferUnpaved bool, pavedOnly bool, surfacePolicy string) *routeSearchBounds {
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
	bounds.MaxCostM = (targetM + 500) * maxAllowedSurfaceWeight(minPavedPercent, preferUnpaved, pavedOnly, surfacePolicy)
	return bounds
}

func (b *routeSearchBounds) contains(node GraphNode) bool {
	if b == nil {
		return true
	}
	return node.Lat >= b.MinLat && node.Lat <= b.MaxLat && node.Lon >= b.MinLon && node.Lon <= b.MaxLon
}

func maxAllowedSurfaceWeight(minPavedPercent float64, preferUnpaved bool, pavedOnly bool, surfacePolicy string) float64 {
	if !pavedOnly {
		return math.Max(surfaceWeight(SurfacePaved, minPavedPercent, preferUnpaved), math.Max(surfaceWeight(SurfaceUnpaved, minPavedPercent, preferUnpaved), surfaceWeight(SurfaceUnknown, minPavedPercent, preferUnpaved)))
	}
	if surfacePolicy == SurfacePolicyAssumePaved {
		return surfaceWeight(SurfaceUnknown, minPavedPercent, preferUnpaved)
	}
	return surfaceWeight(SurfacePaved, minPavedPercent, preferUnpaved)
}

func (g *Graph) shortestPath(start int, goal int, minPavedPercent float64, preferUnpaved bool, pavedOnly bool, surfacePolicy string, blockedEdges map[edgeKey]struct{}, avoidEdges map[edgeKey]struct{}, avoidedWays map[int64]avoidance.Road, search *searchWorkspace) ([]int, []GraphEdge, error) {
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
			cost := edge.Distance * surfaceWeight(edge.Surface, minPavedPercent, preferUnpaved)
			if _, avoid := avoidEdges[newEdgeKey(item.Node, edge.To)]; avoid {
				cost *= recentRoadPenaltyWeight
			}
			if _, avoid := avoidedWays[edge.OSMWayID]; avoid {
				cost *= avoidedRoadPenaltyWeight
			}
			next := search.Dist[item.Node] + cost
			maxCostM := searchMaxCostM(search.Bounds, len(avoidEdges) > 0 || len(avoidedWays) > 0)
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
		return bounds.MaxCostM * avoidedRoadPenaltyWeight
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

func surfaceWeight(surface int, minPavedPercent float64, preferUnpaved bool) float64 {
	if preferUnpaved {
		switch surface {
		case SurfaceUnpaved:
			return 1
		case SurfaceUnknown:
			return 1.05
		case SurfacePaved:
			return 12
		default:
			return 1.05
		}
	}
	if minPavedPercent > 0 {
		switch surface {
		case SurfacePaved:
			return 1
		case SurfaceUnknown:
			return 1.05
		case SurfaceUnpaved:
			return 12
		default:
			return 1.05
		}
	}
	if minPavedPercent <= 0 {
		return 1
	}
	return 1
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

func localScore(candidate localCandidate, targetM float64, minPavedPercent float64, preferUnrunRoads bool, preferUnpaved bool) float64 {
	shortPenalty := math.Max(0, targetM-candidate.DistanceM) / targetM * 1000
	extraMeters := math.Max(0, candidate.DistanceM-targetM)
	extraPenalty := extraMeters / targetM
	if extraMeters > 500 {
		extraPenalty += (extraMeters - 500) / 10
	}
	pavedPenalty := 0.0
	if minPavedPercent > 0 {
		pavedPenalty = surfacePreferencePenalty(candidate.KnownSurfacePercent, candidate.KnownPavedPercent, preferredKnownPavedTarget)
	}
	historyPenalty := 0.0
	if preferUnrunRoads {
		historyPenalty = candidate.PreviouslyRunPercent*8 + candidate.RecentPreviouslyRunPercent*45
	}
	unpavedPenalty := 0.0
	if preferUnpaved {
		unpavedPenalty = surfacePreferencePenalty(candidate.KnownSurfacePercent, candidate.KnownUnpavedPercent, preferredKnownUnpavedTarget)
		unpavedPenalty += math.Max(0, candidate.TaggedPavedPercent-maxPavedWhenPreferUnpaved) * 100
	}
	avoidancePenalty := candidate.AvoidedRoadDistanceM * 20
	return shortPenalty + extraPenalty + pavedPenalty + historyPenalty + unpavedPenalty + avoidancePenalty
}

func surfacePreferencePenalty(knownSurfacePercent float64, preferredKnownSurfacePercent float64, target float64) float64 {
	lowKnownSurfacePenalty := math.Max(0, lowKnownSurfaceDataThreshold-knownSurfacePercent)
	if knownSurfacePercent < 0.1 {
		return lowKnownSurfacePenalty
	}
	shortfall := math.Max(0, target-preferredKnownSurfacePercent)
	return shortfall*100 + lowKnownSurfacePenalty
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
