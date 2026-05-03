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
}
