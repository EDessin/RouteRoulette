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
  seed?: number;
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
  provider: string;
  warnings?: string[];
}

@Injectable({ providedIn: 'root' })
export class RouteApiService {
  constructor(private readonly http: HttpClient) {}

  generateRoute(request: GenerateRouteRequest): Observable<RouteResponse> {
    return this.http.post<RouteResponse>('/api/routes/generate', request);
  }
}
