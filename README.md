# RouteRoulette

RouteRoulette is an app for creating running routes of a chosen distance while prioritizing roads you have not run before.

The goal is simple: pick a target length, start from where you are, and let the app suggest a route that helps you explore more of your local map instead of repeating the same roads.

Over time, RouteRoulette can use your running history to make each new route feel fresh while still matching the distance you want to run.

## MVP

This repository now contains an MVP with:

- A Go REST API in `backend/`
- An Angular + PrimeNG frontend in `frontend/`
- A route generation form for target distance, home coordinates, max start radius, and paved preference
- Estimated run duration based on your pace in minutes per kilometer
- Minimum paved percentage targeting for route candidate scoring
- A map view that displays the generated circular route
- OpenRouteService integration for real round-trip route generation
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

Run:

```bash
cd backend
export $(grep -v '^#' .env | xargs)
go run ./cmd/api
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

## API

Generate a route:

```http
POST /api/routes/generate
```

Example body:

```json
{
  "home": {
    "lat": 50.8503,
    "lon": 4.3517
  },
  "targetDistanceKm": 8,
  "maxStartDistanceKm": 2,
  "estimatedPaceMinPerKm": 6,
  "preferPaved": true,
  "minPavedPercent": 70
}
```

## Notes

OpenRouteService round-trip routing treats the requested length as a target, not a guarantee. The backend retries with different seeds and returns the best route it finds.

Paved-route preference depends on available OpenStreetMap surface data. When the provider does not return enough surface detail, the API returns a warning instead of pretending to know.

Minimum paved percentage is used as a route scoring target. The backend retries candidates and returns the best match it can find; provider and map data limitations mean this is not always a hard guarantee.

## Project Status

RouteRoulette is in early development.

## Vision

Running apps are great at tracking where you have been. RouteRoulette is about helping decide where to go next.
