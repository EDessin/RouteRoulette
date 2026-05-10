import { HttpClient } from '@angular/common/http';
import { Injectable } from '@angular/core';
import { Observable } from 'rxjs';

export interface Coordinate {
  lat: number;
  lon: number;
}

export interface GenerateRouteRequest {
  home: Coordinate;
  targetDistanceKm: number;
  maxStartDistanceKm: number;
  estimatedPaceMinPerKm?: number;
  preferPaved: boolean;
  minPavedPercent: number;
  surfacePolicy?: 'strict' | 'assume_paved';
  preferUnrunRoads: boolean;
  seed?: number;
}

export interface HistoryStatus {
  connected: boolean;
  syncedActivities: number;
  lastSyncAt?: string;
  newestActivityStartDate?: string;
}

export interface HistorySyncResult {
  fetchedActivities: number;
  skippedActivities: number;
  syncedActivities: number;
  lastSyncAt?: string;
}

export interface GeocodeRequest {
  text: string;
}

export interface GeocodeResponse {
  label: string;
  home: Coordinate;
}

export interface GeoJsonLineString {
  type: 'LineString';
  coordinates: number[][];
}

export interface RouteResponse {
  routeId: string;
  start: Coordinate;
  distanceKm: number;
  durationMinutes: number;
  geometry: GeoJsonLineString;
  pavedPercent?: number;
  unpavedPercent?: number;
  unknownSurfacePercent?: number;
  unrunPercent?: number;
  previouslyRunPercent?: number;
  provider: string;
  warnings?: string[];
}

@Injectable({ providedIn: 'root' })
export class RouteApiService {
  constructor(private readonly http: HttpClient) {}

  geocodeAddress(request: GeocodeRequest): Observable<GeocodeResponse> {
    return this.http.post<GeocodeResponse>('/api/geocode', request);
  }

  generateRoute(request: GenerateRouteRequest): Observable<RouteResponse> {
    return this.http.post<RouteResponse>('/api/routes/generate', request);
  }

  getHistoryStatus(): Observable<HistoryStatus> {
    return this.http.get<HistoryStatus>('/api/history/status');
  }

  syncStravaHistory(): Observable<HistorySyncResult> {
    return this.http.post<HistorySyncResult>('/api/strava/sync', {});
  }

  clearHistory(): Observable<{ status: string }> {
    return this.http.delete<{ status: string }>('/api/history');
  }
}
