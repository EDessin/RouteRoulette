# RouteRoulette

RouteRoulette is an app for creating running routes of a chosen distance while prioritizing roads you have not run before.

The goal is simple: pick a target length, start from where you are, and let the app suggest a route that helps you explore more of your local map instead of repeating the same roads.

Over time, RouteRoulette can use your running history to make each new route feel fresh while still matching the distance you want to run.

## MVP

This repository now contains an MVP with:

- A Go REST API in `backend/`
- An Angular + PrimeNG frontend in `frontend/`
- A route generation form for home address, target distance, max start radius, and paved-only routing
- Estimated run duration based on your pace in minutes per kilometer
- Minimum paved percentage targeting for route candidate scoring
- A surface-data mode for strict OSM paved tags or assuming normal roads are paved unless tagged otherwise
- Strava history sync for preferring roads you have not run before
- A map view that displays the generated circular route
- Local OpenStreetMap route generation from a cached Belgium extract
- OpenRouteService geocoding for address lookup
- A mock route fallback for local UI development when no OpenRouteService API key is configured

## What It Does

- Builds routes around a target distance
- Favors unrun roads when planning
- Helps runners discover new streets, paths, and neighborhoods
- Keeps route planning focused on exploration instead of routine

## Run The Backend

Install Go, then configure the API:

```bash
cd backend
cp .env.example .env
```

Set `ORS_API_KEY` in `backend/.env` if you want real routes from OpenRouteService.

To use Strava history, create an app at `https://www.strava.com/settings/api`, set the callback domain to `localhost`, and add these values to `backend/.env`:

```text
STRAVA_CLIENT_ID=your-client-id
STRAVA_CLIENT_SECRET=your-client-secret
STRAVA_REDIRECT_URL=http://localhost:8080/api/strava/callback
```

By default, route generation uses local OpenStreetMap data:

```text
ROUTING_PROVIDER=local_osm
OSM_EXTRACT_PATH=data/osm/belgium-latest.osm.pbf
ALLOW_OSM_DOWNLOAD=true
```

On first use, the backend downloads and stores the Belgium extract at `backend/data/osm/belgium-latest.osm.pbf`. It then builds a 20 km road graph around the home location and stores it under `backend/data/osm/road-cache/`. Later route requests for the same home location reuse that stored road graph instead of re-reading the full extract.

The stored OSM data is intentionally ignored by Git because it is large and machine-local.
Synced Strava history is stored locally under `backend/data/history/` and is also ignored by Git.

Run:

```bash
cd backend
export $(grep -v '^#' .env | xargs)
CGO_ENABLED=0 go run ./cmd/api
```

The API listens on `http://localhost:8080`.

## Run The Frontend

Install Node.js and npm, then:

```bash
cd frontend
npm install
npm start
```

The Angular app runs on `http://localhost:4200` and proxies `/api` requests to the Go backend.

## Local OSM Routing

The local OSM route engine:

- imports runnable/walkable roads within 20 km of the home location
- classifies road surface from OSM tags such as `surface=asphalt`, `surface=gravel`, and `tracktype=grade1`
- generates circular route candidates locally
- can use only explicitly paved roads, or can treat unknown normal roads as paved unless OSM tags mark them unpaved
- uses A* path search with reusable search buffers for candidate generation
- prioritizes paved percentage over exact distance
- scores route candidates against synced Strava history to prefer unrun roads
- avoids routes shorter than requested when possible
- rejects local route candidates that reuse the same road segment
- returns paved, unpaved, unknown-surface, unrun, and previously-run percentages

If local OSM routing cannot build or use a graph, the backend returns that error directly. OpenRouteService is still used for address geocoding, but no longer as a route-generation fallback.

## API

Geocode a home address:

```http
POST /api/geocode
```

Example body:

```json
{
  "text": "Bossepleinstraat 121, 3130 Betekom"
}
```

Generate a route:

```http
POST /api/routes/generate
```

Example body:

```json
{
  "home": {
    "lat": 50.9950381,
    "lon": 4.7699273
  },
  "targetDistanceKm": 8,
  "maxStartDistanceKm": 0,
  "estimatedPaceMinPerKm": 6,
  "preferPaved": true,
  "minPavedPercent": 70,
  "surfacePolicy": "assume_paved",
  "preferUnrunRoads": true
}
```

Strava history endpoints:

```http
GET /api/strava/connect
GET /api/strava/callback
POST /api/strava/sync
GET /api/history/status
DELETE /api/history
```

## Notes

OpenRouteService is used for address geocoding only. Route generation is local OSM only.

Paved-route preference depends on available OpenStreetMap surface data. Use `surfacePolicy: "strict"` to require explicit paved OSM tags, or `surfacePolicy: "assume_paved"` to treat unknown-surface normal roads as paved unless OSM tags mark them unpaved.

Minimum paved percentage is the main route scoring target. The backend retries many candidates at the requested distance and slightly longer distances, rejects shorter routes when possible, caps planned route enlargement at 0.5 km, and aims to find a paved-road result within 5 percentage points of the requested value. Provider and map data limitations mean this is not always a hard guarantee.

Unrun-road preference depends on synced Strava history. Syncing is incremental: RouteRoulette remembers Strava activity IDs that were already stored locally and fetches GPS streams only for new runs.

Address search uses OpenRouteService geocoding and requires `ORS_API_KEY`. Coordinate text such as `50.9950381, 4.7699273` works without a key for local mock-route testing.

## Project Status

RouteRoulette is in early development.

## Vision

Running apps are great at tracking where you have been. RouteRoulette is about helping decide where to go next.
