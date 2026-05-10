package history

import (
	"context"
	"time"

	"github.com/EDessin/RouteRoulette/backend/internal/planner"
	"github.com/EDessin/RouteRoulette/backend/internal/strava"
)

type StravaAPI interface {
	ListActivities(ctx context.Context, page int, perPage int) ([]strava.Activity, error)
	ActivityLatLngStream(ctx context.Context, id int64) ([]planner.Coordinate, error)
}

type SyncResult struct {
	FetchedActivities int    `json:"fetchedActivities"`
	SkippedActivities int    `json:"skippedActivities"`
	SyncedActivities  int    `json:"syncedActivities"`
	LastSyncAt        string `json:"lastSyncAt,omitempty"`
}

func SyncStrava(ctx context.Context, api StravaAPI, store *Store) (SyncResult, error) {
	const perPage = 200

	status, err := store.Status(false)
	if err != nil {
		return SyncResult{}, err
	}
	newestSynced := status.NewestActivityStartDate
	result := SyncResult{}

	for page := 1; ; page++ {
		activities, err := api.ListActivities(ctx, page, perPage)
		if err != nil {
			return SyncResult{}, err
		}
		if len(activities) == 0 {
			break
		}

		pageOnlySyncedOlder := true
		for _, activity := range activities {
			if !strava.IsRunActivity(activity) {
				continue
			}

			alreadySynced, err := store.HasActivity(activity.ID)
			if err != nil {
				return SyncResult{}, err
			}
			if alreadySynced {
				result.SkippedActivities++
				continue
			}

			if newestSynced == "" || activity.StartDate > newestSynced {
				pageOnlySyncedOlder = false
			} else {
				pageOnlySyncedOlder = false
			}

			coords, err := api.ActivityLatLngStream(ctx, activity.ID)
			if err != nil {
				return SyncResult{}, err
			}
			result.FetchedActivities++
			if len(coords) < 2 {
				continue
			}
			if err := store.SaveActivity(Activity{
				ID:          activity.ID,
				Name:        activity.Name,
				StartDate:   activity.StartDate,
				SportType:   activity.SportType,
				DistanceM:   activity.DistanceM,
				Coordinates: coords,
			}); err != nil {
				return SyncResult{}, err
			}
			result.SyncedActivities++
		}

		if pageOnlySyncedOlder && newestSynced != "" {
			break
		}
		if len(activities) < perPage {
			break
		}
	}

	if err := store.MarkSyncComplete(); err != nil {
		return SyncResult{}, err
	}
	result.LastSyncAt = time.Now().UTC().Format(time.RFC3339)
	return result, nil
}
