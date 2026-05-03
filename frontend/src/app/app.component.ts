import { AfterViewInit, Component, ElementRef, OnDestroy, ViewChild } from '@angular/core';
import { CommonModule } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { HttpErrorResponse } from '@angular/common/http';
import * as L from 'leaflet';
import { ButtonModule } from 'primeng/button';
import { CheckboxModule } from 'primeng/checkbox';
import { InputNumberModule } from 'primeng/inputnumber';
import { MessageModule } from 'primeng/message';
import { ProgressSpinnerModule } from 'primeng/progressspinner';
import { SliderModule } from 'primeng/slider';
import { TagModule } from 'primeng/tag';
import { Coordinate, RouteApiService, RouteResponse } from './route-api.service';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [
    ButtonModule,
    CheckboxModule,
    CommonModule,
    FormsModule,
    InputNumberModule,
    MessageModule,
    ProgressSpinnerModule,
    SliderModule,
    TagModule,
  ],
  templateUrl: './app.component.html',
  styleUrl: './app.component.css',
})
export class AppComponent implements AfterViewInit, OnDestroy {
  @ViewChild('map', { static: true }) private readonly mapElement!: ElementRef<HTMLDivElement>;

  homeLat = 50.9950381;
  homeLon = 4.7699273;
  targetDistanceKm = 8;
  maxStartDistanceKm = 2;
  estimatedPaceMinPerKm = 6;
  preferPaved = true;
  minPavedPercent = 70;

  route?: RouteResponse;
  errorMessage = '';
  isGenerating = false;

  private map?: L.Map;
  private routeLayer?: L.Polyline;
  private homeMarker?: L.CircleMarker;
  private startMarker?: L.CircleMarker;

  constructor(private readonly routeApi: RouteApiService) {}

  ngAfterViewInit(): void {
    this.map = L.map(this.mapElement.nativeElement, {
      zoomControl: false,
      scrollWheelZoom: true,
    }).setView([this.homeLat, this.homeLon], 13);

    L.control.zoom({ position: 'bottomright' }).addTo(this.map);
    L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
      maxZoom: 19,
      attribution: '&copy; OpenStreetMap contributors',
    }).addTo(this.map);

    this.drawHomeMarker();
  }

  ngOnDestroy(): void {
    this.map?.remove();
  }

  generateRoute(): void {
    if (!this.isValidForm()) {
      this.errorMessage = 'Check the route length, start radius, pace, paved percentage, and home coordinates.';
      return;
    }

    this.errorMessage = '';
    this.isGenerating = true;

    this.routeApi
      .generateRoute({
        home: this.homeCoordinate(),
        targetDistanceKm: this.targetDistanceKm,
        maxStartDistanceKm: this.maxStartDistanceKm,
        estimatedPaceMinPerKm: this.estimatedPaceMinPerKm,
        preferPaved: this.preferPaved,
        minPavedPercent: this.minPavedPercent,
        seed: Date.now(),
      })
      .subscribe({
        next: (route) => {
          this.route = route;
          this.isGenerating = false;
          this.drawRoute(route);
        },
        error: (err: HttpErrorResponse) => {
          this.isGenerating = false;
          this.errorMessage = this.errorText(err);
        },
      });
  }

  useCurrentMapCenter(): void {
    const center = this.map?.getCenter();
    if (!center) {
      return;
    }

    this.homeLat = Number(center.lat.toFixed(6));
    this.homeLon = Number(center.lng.toFixed(6));
    this.drawHomeMarker();
  }

  private isValidForm(): boolean {
    return (
      Number.isFinite(this.homeLat) &&
      Number.isFinite(this.homeLon) &&
      this.homeLat >= -90 &&
      this.homeLat <= 90 &&
      this.homeLon >= -180 &&
      this.homeLon <= 180 &&
      this.targetDistanceKm >= 1 &&
      this.targetDistanceKm <= 100 &&
      this.maxStartDistanceKm >= 0 &&
      this.maxStartDistanceKm <= 25 &&
      this.estimatedPaceMinPerKm >= 2 &&
      this.estimatedPaceMinPerKm <= 20 &&
      this.minPavedPercent >= 0 &&
      this.minPavedPercent <= 100
    );
  }

  private homeCoordinate(): Coordinate {
    return {
      lat: this.homeLat,
      lon: this.homeLon,
    };
  }

  private drawHomeMarker(): void {
    if (!this.map) {
      return;
    }

    const home: L.LatLngTuple = [this.homeLat, this.homeLon];
    this.homeMarker?.remove();
    this.homeMarker = L.circleMarker(home, {
      radius: 8,
      color: '#114f3b',
      weight: 2,
      fillColor: '#16a34a',
      fillOpacity: 0.95,
    }).addTo(this.map);
    this.homeMarker.bindTooltip('Home');
  }

  private drawRoute(route: RouteResponse): void {
    if (!this.map) {
      return;
    }

    const latLngs = route.geometry.coordinates.map(([lon, lat]) => [lat, lon] as L.LatLngTuple);

    this.routeLayer?.remove();
    this.startMarker?.remove();

    this.routeLayer = L.polyline(latLngs, {
      color: '#f97316',
      weight: 5,
      opacity: 0.95,
      lineJoin: 'round',
    }).addTo(this.map);

    this.startMarker = L.circleMarker([route.start.lat, route.start.lon], {
      radius: 7,
      color: '#7c2d12',
      weight: 2,
      fillColor: '#fed7aa',
      fillOpacity: 1,
    }).addTo(this.map);
    this.startMarker.bindTooltip('Start');

    this.drawHomeMarker();
    this.map.fitBounds(this.routeLayer.getBounds(), {
      padding: [32, 32],
      maxZoom: 15,
    });
  }

  private errorText(err: HttpErrorResponse): string {
    if (typeof err.error?.error === 'string') {
      return err.error.error;
    }
    return 'Route generation failed. Check that the Go API is running and configured.';
  }
}
