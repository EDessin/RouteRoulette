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
  segments?: RouteSegment[];
  pavedPercent?: number;
  unpavedPercent?: number;
  unknownSurfacePercent?: number;
  unrunPercent?: number;
  previouslyRunPercent?: number;
  avoidedRoadDistanceM?: number;
  provider: string;
  warnings?: string[];
}

export interface RouteSegment {
  fromIndex: number;
  toIndex: number;
  osmWayId?: number;
  name?: string;
  distanceM: number;
}

export interface AvoidedRoad {
  id: string;
  osmWayId: number;
  name?: string;
  reason: AvoidanceReason;
  coordinate: Coordinate;
  createdAt: string;
}

export type AvoidanceReason = 'busy_road' | 'no_lights' | 'not_accessible' | 'other';

export interface AddAvoidedRoadRequest {
  osmWayId: number;
  name?: string;
  reason: AvoidanceReason;
  coordinate: Coordinate;
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

  getAvoidedRoads(): Observable<AvoidedRoad[]> {
    return this.http.get<AvoidedRoad[]>('/api/avoidance');
  }

  addAvoidedRoad(request: AddAvoidedRoadRequest): Observable<AvoidedRoad> {
    return this.http.post<AvoidedRoad>('/api/avoidance', request);
  }

  deleteAvoidedRoad(id: string): Observable<{ status: string }> {
    return this.http.delete<{ status: string }>(`/api/avoidance/${encodeURIComponent(id)}`);
  }
}
