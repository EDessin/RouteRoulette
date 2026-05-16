package surfacemarks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

const (
	SurfacePaved   = "paved"
	SurfaceUnpaved = "unpaved"
)

type Store struct {
	dir    string
	mu     sync.Mutex
	cached []Road
}

type Road struct {
	ID         string             `json:"id"`
	OSMWayID   int64              `json:"osmWayId"`
	Name       string             `json:"name,omitempty"`
	Surface    string             `json:"surface"`
	Coordinate planner.Coordinate `json:"coordinate"`
	CreatedAt  string             `json:"createdAt"`
	UpdatedAt  string             `json:"updatedAt"`
}

type MarkRoadRequest struct {
	OSMWayID   int64              `json:"osmWayId"`
	Name       string             `json:"name,omitempty"`
	Surface    string             `json:"surface"`
	Coordinate planner.Coordinate `json:"coordinate"`
}

func NewStore(dir string) Store {
	if dir == "" {
		dir = "data/surface-marks"
	}
	return Store{dir: dir}
}

func (s *Store) List() ([]Road, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	roads, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	return append([]Road{}, roads...), nil
}

func (s *Store) Mark(req MarkRoadRequest) (Road, error) {
	if req.OSMWayID == 0 {
		return Road{}, errors.New("osmWayId is required")
	}
	surface := NormalizeSurface(req.Surface)
	if surface == "" {
		return Road{}, errors.New("surface must be paved or unpaved")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	roads, err := s.loadLocked()
	if err != nil {
		return Road{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	id := RoadID(req.OSMWayID)
	road := Road{
		ID:         id,
		OSMWayID:   req.OSMWayID,
		Name:       strings.TrimSpace(req.Name),
		Surface:    surface,
		Coordinate: req.Coordinate,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	replaced := false
	for idx := range roads {
		if roads[idx].ID == id {
			if roads[idx].CreatedAt != "" {
				road.CreatedAt = roads[idx].CreatedAt
			}
			roads[idx] = road
			replaced = true
			break
		}
	}
	if !replaced {
		roads = append(roads, road)
	}
	sortRoads(roads)
	if err := s.saveLocked(roads); err != nil {
		return Road{}, err
	}
	return road, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	roads, err := s.loadLocked()
	if err != nil {
		return err
	}
	filtered := roads[:0]
	for _, road := range roads {
		if road.ID != id {
			filtered = append(filtered, road)
		}
	}
	if len(filtered) == len(roads) {
		return os.ErrNotExist
	}
	return s.saveLocked(filtered)
}

func RoadID(osmWayID int64) string {
	return fmt.Sprintf("way:%d", osmWayID)
}

func ByWayID(roads []Road) map[int64]Road {
	index := make(map[int64]Road, len(roads))
	for _, road := range roads {
		if road.OSMWayID != 0 {
			index[road.OSMWayID] = road
		}
	}
	return index
}

func NormalizeSurface(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case SurfacePaved, SurfaceUnpaved:
		return strings.TrimSpace(strings.ToLower(value))
	default:
		return ""
	}
}

func (s *Store) loadLocked() ([]Road, error) {
	if s.cached != nil {
		return append([]Road{}, s.cached...), nil
	}
	file, err := os.Open(s.path())
	if errors.Is(err, os.ErrNotExist) {
		s.cached = []Road{}
		return []Road{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var roads []Road
	if err := json.NewDecoder(file).Decode(&roads); err != nil {
		return nil, err
	}
	sortRoads(roads)
	s.cached = append([]Road{}, roads...)
	return roads, nil
}

func (s *Store) saveLocked(roads []Road) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmpPath := s.path() + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(roads); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path()); err != nil {
		return err
	}
	s.cached = append([]Road{}, roads...)
	return nil
}

func (s *Store) path() string {
	return filepath.Join(s.dir, "marked_roads.json")
}

func sortRoads(roads []Road) {
	sort.Slice(roads, func(i, j int) bool {
		if roads[i].Name == roads[j].Name {
			return roads[i].ID < roads[j].ID
		}
		return roads[i].Name < roads[j].Name
	})
}
