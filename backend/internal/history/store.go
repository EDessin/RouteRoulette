package history

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
)

type Store struct {
	dir                   string
	mu                    sync.Mutex
	cachedActivities      []Activity
	cachedActivityCount   int
	cachedNewestStartDate string
}

type Activity struct {
	ID          int64                `json:"id"`
	Name        string               `json:"name,omitempty"`
	StartDate   string               `json:"startDate"`
	SportType   string               `json:"sportType"`
	DistanceM   float64              `json:"distanceM"`
	SyncedAt    string               `json:"syncedAt"`
	Coordinates []planner.Coordinate `json:"coordinates"`
}

type ActivityIndexEntry struct {
	ID        int64   `json:"id"`
	StartDate string  `json:"startDate"`
	DistanceM float64 `json:"distanceM"`
	SportType string  `json:"sportType"`
	SyncedAt  string  `json:"syncedAt"`
}

type Index struct {
	Activities              map[string]ActivityIndexEntry `json:"activities"`
	NewestActivityStartDate string                        `json:"newestActivityStartDate,omitempty"`
	LastSyncAt              string                        `json:"lastSyncAt,omitempty"`
}

type Status struct {
	Connected               bool   `json:"connected"`
	SyncedActivities        int    `json:"syncedActivities"`
	LastSyncAt              string `json:"lastSyncAt,omitempty"`
	NewestActivityStartDate string `json:"newestActivityStartDate,omitempty"`
}

func NewStore(dir string) Store {
	if dir == "" {
		dir = "data/history"
	}
	return Store{dir: dir}
}

func (s *Store) Status(connected bool) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.loadIndexLocked()
	if err != nil {
		return Status{}, err
	}
	return Status{
		Connected:               connected,
		SyncedActivities:        len(index.Activities),
		LastSyncAt:              index.LastSyncAt,
		NewestActivityStartDate: index.NewestActivityStartDate,
	}, nil
}

func (s *Store) HasActivity(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.loadIndexLocked()
	if err != nil {
		return false, err
	}
	_, ok := index.Activities[strconv.FormatInt(id, 10)]
	return ok, nil
}

func (s *Store) SaveActivity(activity Activity) error {
	if len(activity.Coordinates) == 0 {
		return errors.New("history activity has no coordinates")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.loadIndexLocked()
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	activity.SyncedAt = now
	if err := os.MkdirAll(s.activitiesDir(), 0o755); err != nil {
		return err
	}
	if err := writeJSONFile(s.activityPath(activity.ID), activity); err != nil {
		return err
	}
	s.cachedActivities = nil

	key := strconv.FormatInt(activity.ID, 10)
	index.Activities[key] = ActivityIndexEntry{
		ID:        activity.ID,
		StartDate: activity.StartDate,
		DistanceM: activity.DistanceM,
		SportType: activity.SportType,
		SyncedAt:  now,
	}
	index.LastSyncAt = now
	if index.NewestActivityStartDate == "" || activity.StartDate > index.NewestActivityStartDate {
		index.NewestActivityStartDate = activity.StartDate
	}
	return s.saveIndexLocked(index)
}

func (s *Store) MarkSyncComplete() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.loadIndexLocked()
	if err != nil {
		return err
	}
	index.LastSyncAt = time.Now().UTC().Format(time.RFC3339)
	return s.saveIndexLocked(index)
}

func (s *Store) Activities() ([]Activity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.loadIndexLocked()
	if err != nil {
		return nil, err
	}
	if s.cachedActivities != nil && s.cachedActivityCount == len(index.Activities) && s.cachedNewestStartDate == index.NewestActivityStartDate {
		return append([]Activity{}, s.cachedActivities...), nil
	}

	entries, err := os.ReadDir(s.activitiesDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	activities := make([]Activity, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var activity Activity
		if err := readJSONFile(filepath.Join(s.activitiesDir(), entry.Name()), &activity); err != nil {
			return nil, err
		}
		activities = append(activities, activity)
	}
	sort.Slice(activities, func(i, j int) bool {
		return activities[i].StartDate > activities[j].StartDate
	})
	s.cachedActivities = append([]Activity{}, activities...)
	s.cachedActivityCount = len(index.Activities)
	s.cachedNewestStartDate = index.NewestActivityStartDate
	return activities, nil
}

func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.RemoveAll(s.dir); err != nil {
		return err
	}
	s.cachedActivities = nil
	s.cachedActivityCount = 0
	s.cachedNewestStartDate = ""
	return s.saveIndexLocked(newIndex())
}

func (s *Store) loadIndexLocked() (Index, error) {
	var index Index
	if err := readJSONFile(s.indexPath(), &index); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newIndex(), nil
		}
		return Index{}, err
	}
	if index.Activities == nil {
		index.Activities = make(map[string]ActivityIndexEntry)
	}
	return index, nil
}

func (s *Store) saveIndexLocked(index Index) error {
	if index.Activities == nil {
		index.Activities = make(map[string]ActivityIndexEntry)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return writeJSONFile(s.indexPath(), index)
}

func newIndex() Index {
	return Index{Activities: make(map[string]ActivityIndexEntry)}
}

func (s *Store) indexPath() string {
	return filepath.Join(s.dir, "sync-index.json")
}

func (s *Store) activitiesDir() string {
	return filepath.Join(s.dir, "activities")
}

func (s *Store) activityPath(id int64) string {
	return filepath.Join(s.activitiesDir(), strconv.FormatInt(id, 10)+".json")
}

func readJSONFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(file).Decode(target)
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
