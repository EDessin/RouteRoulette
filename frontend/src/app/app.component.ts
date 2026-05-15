import { AfterViewInit, Component, ElementRef, OnDestroy, OnInit, ViewChild } from '@angular/core';
import { CommonModule } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { HttpErrorResponse } from '@angular/common/http';
import * as L from 'leaflet';
import { ButtonModule } from 'primeng/button';
import { CheckboxModule } from 'primeng/checkbox';
import { DialogModule } from 'primeng/dialog';
import { InputNumberModule } from 'primeng/inputnumber';
import { InputTextModule } from 'primeng/inputtext';
import { MessageModule } from 'primeng/message';
import { ProgressSpinnerModule } from 'primeng/progressspinner';
import { SelectButtonModule } from 'primeng/selectbutton';
import { SliderModule } from 'primeng/slider';
import { TagModule } from 'primeng/tag';
import {
  AvoidanceReason,
  AvoidedRoad,
  Coordinate,
  HistoryStatus,
  RouteApiService,
  RouteResponse,
  RouteSegment,
} from './route-api.service';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [
    ButtonModule,
    CheckboxModule,
    CommonModule,
    DialogModule,
    FormsModule,
    InputNumberModule,
    InputTextModule,
    MessageModule,
    ProgressSpinnerModule,
    SelectButtonModule,
    SliderModule,
    TagModule,
  ],
  templateUrl: './app.component.html',
  styleUrl: './app.component.css',
})
export class AppComponent implements AfterViewInit, OnDestroy, OnInit {
  @ViewChild('map', { static: true }) private readonly mapElement!: ElementRef<HTMLDivElement>;

  homeAddress = 'Bossepleinstraat 121, 3130 Betekom';
  resolvedHomeLabel = this.homeAddress;
  targetDistanceKm = 8;
  maxStartDistanceKm = 0;
  estimatedPaceMinPerKm = 6;
  preferPaved = true;
  minPavedPercent = 70;
  preferUnrunRoads = true;
  surfacePolicy: 'strict' | 'assume_paved' = 'assume_paved';
  surfacePolicyOptions = [
    { label: 'Assume normal roads paved', value: 'assume_paved' },
    { label: 'Strict OSM tags', value: 'strict' },
  ];

  route?: RouteResponse;
  historyStatus?: HistoryStatus;
  avoidedRoads: AvoidedRoad[] = [];
  errorMessage = '';
  historyMessage = '';
  avoidanceMessage = '';
  isGenerating = false;
  isSyncingHistory = false;
  isSavingAvoidedRoad = false;
  avoidDialogVisible = false;
  selectedSegment?: RouteSegment;
  selectedSegmentCoordinate?: Coordinate;
  selectedAvoidanceReason: AvoidanceReason = 'busy_road';
  avoidanceReasonOptions = [
    { label: 'Busy road', value: 'busy_road' },
    { label: 'No lights', value: 'no_lights' },
    { label: 'Not accessible', value: 'not_accessible' },
    { label: 'Other', value: 'other' },
  ];

  private homeLat = 50.9950381;
  private homeLon = 4.7699273;
  private map?: L.Map;
  private routeRenderer?: L.Renderer;
  private routeLayer?: L.LayerGroup;
  private homeMarker?: L.CircleMarker;
  private startMarker?: L.CircleMarker;

  constructor(private readonly routeApi: RouteApiService) {}

  ngOnInit(): void {
    this.loadHistoryStatus();
    this.loadAvoidedRoads();
  }

  ngAfterViewInit(): void {
    this.map = L.map(this.mapElement.nativeElement, {
      zoomControl: false,
      scrollWheelZoom: true,
    }).setView([this.homeLat, this.homeLon], 13);
    this.routeRenderer = L.svg({ padding: 1.5 });

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

  exportRouteAsGpx(): void {
    if (!this.route) {
      return;
    }

    const gpx = this.routeToGpx(this.route);
    const blob = new Blob([gpx], { type: 'application/gpx+xml;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = this.gpxFilename(this.route);
    link.click();
    URL.revokeObjectURL(url);
  }

  connectStrava(): void {
    window.location.href = '/api/strava/connect';
  }

  syncHistory(): void {
    this.historyMessage = '';
    this.errorMessage = '';
    this.isSyncingHistory = true;
    this.routeApi.syncStravaHistory().subscribe({
      next: (result) => {
        this.isSyncingHistory = false;
        this.historyMessage = `${result.syncedActivities} new runs synced. ${result.skippedActivities} already up to date.`;
        this.loadHistoryStatus();
      },
      error: (err: HttpErrorResponse) => {
        this.isSyncingHistory = false;
        this.errorMessage = this.errorText(err);
      },
    });
  }

  clearHistory(): void {
    this.historyMessage = '';
    this.errorMessage = '';
    this.routeApi.clearHistory().subscribe({
      next: () => {
        this.historyMessage = 'Run history cleared.';
        this.loadHistoryStatus();
      },
      error: (err: HttpErrorResponse) => {
        this.errorMessage = this.errorText(err);
      },
    });
  }

  saveAvoidedRoad(): void {
    if (!this.selectedSegment?.osmWayId || !this.selectedSegmentCoordinate) {
      this.errorMessage = 'This route segment does not have road metadata and cannot be avoided yet.';
      return;
    }

    this.errorMessage = '';
    this.avoidanceMessage = '';
    this.isSavingAvoidedRoad = true;
    this.routeApi
      .addAvoidedRoad({
        osmWayId: this.selectedSegment.osmWayId,
        name: this.selectedSegment.name,
        reason: this.selectedAvoidanceReason,
        coordinate: this.selectedSegmentCoordinate,
      })
      .subscribe({
        next: () => {
          this.isSavingAvoidedRoad = false;
          this.avoidDialogVisible = false;
          this.avoidanceMessage = 'Road added to your avoid list.';
          this.loadAvoidedRoads();
        },
        error: (err: HttpErrorResponse) => {
          this.isSavingAvoidedRoad = false;
          this.errorMessage = this.errorText(err);
        },
      });
  }

  removeAvoidedRoad(road: AvoidedRoad): void {
    this.errorMessage = '';
    this.avoidanceMessage = '';
    this.routeApi.deleteAvoidedRoad(road.id).subscribe({
      next: () => {
        this.avoidanceMessage = 'Road removed from your avoid list.';
        this.loadAvoidedRoads();
      },
      error: (err: HttpErrorResponse) => {
        this.errorMessage = this.errorText(err);
      },
    });
  }

  generateRoute(): void {
    if (!this.isValidForm()) {
      this.errorMessage = 'Check the home address, route length, start radius, pace, and paved percentage.';
      return;
    }

    this.errorMessage = '';
    this.isGenerating = true;

    this.routeApi
      .geocodeAddress({ text: this.homeAddress.trim() })
      .subscribe({
        next: (location) => {
          this.homeLat = location.home.lat;
          this.homeLon = location.home.lon;
          this.resolvedHomeLabel = location.label;
          this.drawHomeMarker();
          this.generateRouteFromResolvedHome();
        },
        error: (err: HttpErrorResponse) => {
          this.isGenerating = false;
          this.errorMessage = this.errorText(err);
        },
      });
  }

  private isValidForm(): boolean {
    return (
      this.homeAddress.trim().length >= 3 &&
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

  private generateRouteFromResolvedHome(): void {
    this.routeApi
      .generateRoute({
        home: this.homeCoordinate(),
        targetDistanceKm: this.targetDistanceKm,
        maxStartDistanceKm: this.maxStartDistanceKm,
        estimatedPaceMinPerKm: this.estimatedPaceMinPerKm,
        preferPaved: this.preferPaved,
        minPavedPercent: this.minPavedPercent,
        surfacePolicy: this.surfacePolicy,
        preferUnrunRoads: this.preferUnrunRoads,
        seed: Date.now(),
      })
      .subscribe({
        next: (route) => {
          this.route = route;
          this.isGenerating = false;
          this.avoidanceMessage = '';
          this.drawRoute(route);
        },
        error: (err: HttpErrorResponse) => {
          this.isGenerating = false;
          this.errorMessage = this.errorText(err);
        },
      });
  }

  private homeCoordinate(): Coordinate {
    return {
      lat: this.homeLat,
      lon: this.homeLon,
    };
  }

  private loadHistoryStatus(): void {
    this.routeApi.getHistoryStatus().subscribe({
      next: (status) => {
        this.historyStatus = status;
      },
      error: () => {
        this.historyStatus = undefined;
      },
    });
  }

  private loadAvoidedRoads(): void {
    this.routeApi.getAvoidedRoads().subscribe({
      next: (roads) => {
        this.avoidedRoads = roads;
      },
      error: () => {
        this.avoidedRoads = [];
      },
    });
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

    const group = L.layerGroup().addTo(this.map);

    L.polyline(latLngs, {
      color: '#f97316',
      weight: 5,
      opacity: 0.95,
      lineJoin: 'round',
      renderer: this.routeRenderer,
    }).addTo(group);

    for (const segment of route.segments || []) {
      const from = latLngs[segment.fromIndex];
      const to = latLngs[segment.toIndex];
      if (!from || !to || !segment.osmWayId) {
        continue;
      }
      L.polyline([from, to], {
        color: '#0f172a',
        weight: 18,
        opacity: 0.01,
        interactive: true,
        renderer: this.routeRenderer,
      })
        .on('click', (event: L.LeafletMouseEvent) => this.openAvoidDialog(segment, event.latlng))
        .addTo(group);
    }

    this.routeLayer = group;

    this.startMarker = L.circleMarker([route.start.lat, route.start.lon], {
      radius: 7,
      color: '#7c2d12',
      weight: 2,
      fillColor: '#fed7aa',
      fillOpacity: 1,
    }).addTo(this.map);
    this.startMarker.bindTooltip('Start');

    this.drawHomeMarker();
    this.map.fitBounds(L.latLngBounds(latLngs), {
      padding: [32, 32],
      maxZoom: 15,
    });
  }

  private openAvoidDialog(segment: RouteSegment, latLng: L.LatLng): void {
    this.selectedSegment = segment;
    this.selectedSegmentCoordinate = { lat: latLng.lat, lon: latLng.lng };
    this.selectedAvoidanceReason = 'busy_road';
    this.avoidDialogVisible = true;
  }

  private routeToGpx(route: RouteResponse): string {
    const routeName = `RouteRoulette ${route.distanceKm} km`;
    const trackPoints = route.geometry.coordinates
      .map(([lon, lat]) => `      <trkpt lat="${this.gpxCoordinate(lat)}" lon="${this.gpxCoordinate(lon)}"></trkpt>`)
      .join('\n');

    return `<?xml version="1.0" encoding="UTF-8"?>
<gpx version="1.1" creator="RouteRoulette" xmlns="http://www.topografix.com/GPX/1/1">
  <metadata>
    <name>${this.escapeXml(routeName)}</name>
    <desc>${this.escapeXml(`${this.resolvedHomeLabel} - ${route.distanceKm} km`)}</desc>
  </metadata>
  <trk>
    <name>${this.escapeXml(routeName)}</name>
    <trkseg>
${trackPoints}
    </trkseg>
  </trk>
</gpx>
`;
  }

  private gpxFilename(route: RouteResponse): string {
    const distance = String(route.distanceKm).replace('.', 'p');
    return `routeroulette-${distance}km-${route.routeId}.gpx`;
  }

  private gpxCoordinate(value: number): string {
    return value.toFixed(7);
  }

  private escapeXml(value: string): string {
    return value.replace(/[<>&'"]/g, (char) => {
      switch (char) {
        case '<':
          return '&lt;';
        case '>':
          return '&gt;';
        case '&':
          return '&amp;';
        case "'":
          return '&apos;';
        case '"':
          return '&quot;';
        default:
          return char;
      }
    });
  }

  private errorText(err: HttpErrorResponse): string {
    if (typeof err.error?.error === 'string') {
      return err.error.error;
    }
    return 'Route generation failed. Check that the Go API is running and configured.';
  }

  reasonLabel(reason: AvoidanceReason): string {
    switch (reason) {
      case 'busy_road':
        return 'Busy road';
      case 'no_lights':
        return 'No lights';
      case 'not_accessible':
        return 'Not accessible';
      default:
        return 'Other';
    }
  }
}
