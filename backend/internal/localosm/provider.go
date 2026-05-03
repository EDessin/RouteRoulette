package localosm

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
)

const (
	SurfacePaved = iota
	SurfaceUnpaved
	SurfaceUnknown
)

type Config struct {
	DataDir       string
	ExtractPath   string
	ExtractURL    string
	RadiusKm      float64
	AllowDownload bool
}

type Provider struct {
	cfg    Config
	client *http.Client
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
		cfg.RadiusKm = 50
	}

	return Provider{
		cfg: cfg,
		client: &http.Client{
			Timeout: 20 * time.Minute,
		},
	}
}

func (p Provider) GenerateRoundTrip(ctxReq *http.Request, req planner.CandidateRequest) (planner.CandidateRoute, error) {
	graph, err := p.loadGraph(ctxReq.Context(), req.Home)
	if err != nil {
		return planner.CandidateRoute{}, err
	}

	route, err := graph.GenerateLoop(req)
	if err != nil {
		return planner.CandidateRoute{}, err
	}
	return route, nil
}

func (p Provider) loadGraph(ctx context.Context, home planner.Coordinate) (*Graph, error) {
	cachePath := p.graphCachePath(home)
	if graph, err := loadGraphCache(cachePath); err == nil {
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
	return graph, nil
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

func (g *Graph) GenerateLoop(req planner.CandidateRequest) (planner.CandidateRoute, error) {
	if len(g.Nodes) == 0 {
		return planner.CandidateRoute{}, errors.New("local graph is empty")
	}
	start := g.nearestNode(req.Start)
	if start < 0 {
		return planner.CandidateRoute{}, errors.New("no start node found in local graph")
	}

	rng := rand.New(rand.NewSource(req.Seed))
	best := localCandidate{}
	bestScore := math.MaxFloat64
	attempts := 40
	nearestCache := make(map[projectionKey]int)
	search := g.newSearchWorkspace()
	for i := 0; i < attempts; i++ {
		candidate, err := g.loopCandidate(start, req.TargetDistanceM, req.MinPavedPercent, pavedOnly(req), rng, nearestCache, search)
		if err != nil {
			continue
		}
		score := localScore(candidate, req.TargetDistanceM, req.MinPavedPercent)
		if score < bestScore {
			best = candidate
			bestScore = score
		}
		if candidate.DistanceM >= req.TargetDistanceM && candidate.DistanceM <= req.TargetDistanceM+500 && math.Abs(candidate.PavedPercent-req.MinPavedPercent) <= 5 {
			break
		}
	}
	if len(best.Path) == 0 {
		return planner.CandidateRoute{}, errors.New("could not find a local OSM loop")
	}

	paved := round(best.PavedPercent, 1)
	unpaved := round(best.UnpavedPercent, 1)
	unknown := round(best.UnknownPercent, 1)
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
		PavedPercent:   &paved,
		UnpavedPercent: &unpaved,
		UnknownPercent: &unknown,
		Provider:       "local-osm",
	}, nil
}

type localCandidate struct {
	Path           []int
	DistanceM      float64
	PavedPercent   float64
	UnpavedPercent float64
	UnknownPercent float64
}

func pavedOnly(req planner.CandidateRequest) bool {
	return req.PreferPaved || req.MinPavedPercent > 0
}

func (g *Graph) loopCandidate(start int, targetM float64, minPavedPercent float64, pavedOnly bool, rng *rand.Rand, nearestCache map[projectionKey]int, search *searchWorkspace) (localCandidate, error) {
	radiusM := math.Max(600, targetM/(2*math.Pi))
	baseBearing := rng.Float64() * 2 * math.Pi
	waypoints := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		bearing := baseBearing + float64(i)*2*math.Pi/3
		distance := radiusM * (0.75 + rng.Float64()*0.75)
		waypoints = append(waypoints, g.nodeNearProjection(start, bearing, distance, nearestCache))
	}
	points := append([]int{start}, waypoints...)
	points = append(points, start)

	fullPath := []int{}
	var edges []GraphEdge
	for i := 1; i < len(points); i++ {
		path, pathEdges, err := g.shortestPath(points[i-1], points[i], minPavedPercent, pavedOnly, search)
		if err != nil {
			return localCandidate{}, err
		}
		if i > 1 && len(path) > 0 {
			path = path[1:]
		}
		fullPath = append(fullPath, path...)
		edges = append(edges, pathEdges...)
	}
	if len(fullPath) < 2 {
		return localCandidate{}, errors.New("route path is too short")
	}

	total, paved, unpaved, unknown := edgeStats(edges)
	if total == 0 {
		return localCandidate{}, errors.New("route has zero distance")
	}
	if hasRepeatedEdges(fullPath) {
		return localCandidate{}, errors.New("route repeats a road segment")
	}
	return localCandidate{
		Path:           fullPath,
		DistanceM:      total,
		PavedPercent:   paved / total * 100,
		UnpavedPercent: unpaved / total * 100,
		UnknownPercent: unknown / total * 100,
	}, nil
}

func hasRepeatedEdges(path []int) bool {
	seen := make(map[edgeKey]struct{})
	for i := 1; i < len(path); i++ {
		key := newEdgeKey(path[i-1], path[i])
		if _, ok := seen[key]; ok {
			return true
		}
		seen[key] = struct{}{}
	}
	return false
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

type projectionKey struct {
	BearingBucket  int
	DistanceBucket int
}

func (g *Graph) nodeNearProjection(start int, bearing float64, distanceM float64, cache map[projectionKey]int) int {
	key := projectionKey{
		BearingBucket:  int(math.Round(bearing * 180 / math.Pi / 5)),
		DistanceBucket: int(math.Round(distanceM / 100)),
	}
	if idx, ok := cache[key]; ok {
		return idx
	}
	startNode := g.Nodes[start]
	targetLat, targetLon := project(startNode.Lat, startNode.Lon, bearing, distanceM)
	idx := g.nearestNode(planner.Coordinate{Lat: targetLat, Lon: targetLon})
	cache[key] = idx
	return idx
}

func (g *Graph) shortestPath(start int, goal int, minPavedPercent float64, pavedOnly bool, search *searchWorkspace) ([]int, []GraphEdge, error) {
	if start == goal {
		return []int{start}, nil, nil
	}
	search.Reset()
	search.Dist[start] = 0
	search.Prev[start] = -1
	search.Touched = append(search.Touched, start)
	pq := priorityQueue{{Node: start, Priority: heuristicM(g, start, goal)}}
	heap.Init(&pq)

	for pq.Len() > 0 {
		item := heap.Pop(&pq).(queueItem)
		if item.Node == goal {
			break
		}
		for _, edge := range g.Edges[item.Node] {
			if pavedOnly && edge.Surface != SurfacePaved {
				continue
			}
			cost := edge.Distance * surfaceWeight(edge.Surface, minPavedPercent)
			next := search.Dist[item.Node] + cost
			if next < search.Dist[edge.To] {
				if search.Dist[edge.To] == math.MaxFloat64 {
					search.Touched = append(search.Touched, edge.To)
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

type searchWorkspace struct {
	Dist     []float64
	Prev     []int
	PrevEdge []GraphEdge
	Touched  []int
}

func (g *Graph) newSearchWorkspace() *searchWorkspace {
	search := &searchWorkspace{
		Dist:     make([]float64, len(g.Nodes)),
		Prev:     make([]int, len(g.Nodes)),
		PrevEdge: make([]GraphEdge, len(g.Nodes)),
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
	}
	s.Touched = s.Touched[:0]
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

func localScore(candidate localCandidate, targetM float64, minPavedPercent float64) float64 {
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
	return shortPenalty + extraPenalty + pavedPenalty
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
