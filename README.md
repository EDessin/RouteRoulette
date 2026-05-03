# RouteRoulette

RouteRoulette is an app for creating running routes of a chosen distance while prioritizing roads you have not run before.

The goal is simple: pick a target length, start from where you are, and let the app suggest a route that helps you explore more of your local map instead of repeating the same roads.

Over time, RouteRoulette can use your running history to make each new route feel fresh while still matching the distance you want to run.

## MVP

This repository now contains an MVP with:

- A Go REST API in `backend/`
- An Angular + PrimeNG frontend in `frontend/`
- A route generation form for home address, target distance, max start radius, and paved preference
- Estimated run duration based on your pace in minutes per kilometer
- Minimum paved percentage targeting for route candidate scoring
- A map view that displays the generated circular route
- Local OpenStreetMap route generation from a cached Belgium extract
- OpenRouteService fallback for geocoding and backup route generation
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

By default, route generation uses local OpenStreetMap data:

```text
ROUTING_PROVIDER=local_osm
OSM_EXTRACT_PATH=data/osm/belgium-latest.osm.pbf
ALLOW_OSM_DOWNLOAD=true
```

On first use, the backend downloads and stores the Belgium extract at `backend/data/osm/belgium-latest.osm.pbf`. It then builds a 50 km road graph around the home location and stores it under `backend/data/osm/road-cache/`. Later route requests for the same home location reuse that stored road graph instead of re-reading the full extract.

The stored OSM data is intentionally ignored by Git because it is large and machine-local.

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

- imports runnable/walkable roads within 50 km of the home location
- classifies road surface from OSM tags such as `surface=asphalt`, `surface=gravel`, and `tracktype=grade1`
- generates circular route candidates locally
- prioritizes paved percentage over exact distance
- avoids routes shorter than requested when possible
- returns paved, unpaved, and unknown-surface percentages

If local OSM routing cannot build or use a graph, the backend falls back to OpenRouteService if `ORS_API_KEY` is configured, and then to mock routes when `ALLOW_MOCK_ROUTES=true`.

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
  "minPavedPercent": 70
}
```

## Notes

OpenRouteService round-trip routing treats the requested length as a target, not a guarantee. When the OpenRouteService fallback is used, the backend retries with different seeds and returns the best route it finds.

Paved-route preference depends on available OpenStreetMap surface data. When the provider does not return enough surface detail, the API returns a warning instead of pretending to know.

Minimum paved percentage is the main route scoring target. The backend retries many candidates at the requested distance and slightly longer distances, rejects shorter routes when possible, caps planned route enlargement at 0.5 km, and aims to find a paved-road result within 5 percentage points of the requested value. Provider and map data limitations mean this is not always a hard guarantee.

Address search uses OpenRouteService geocoding and requires `ORS_API_KEY`. Coordinate text such as `50.9950381, 4.7699273` works without a key for local mock-route testing.

## Project Status

RouteRoulette is in early development.

## Vision

Running apps are great at tracking where you have been. RouteRoulette is about helping decide where to go next.
