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
import {
  AvoidanceReason,
  AvoidedRoad,
  Coordinate,
  HistoryStatus,
  MarkedRoad,
  MarkedRoadSurface,
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
  ],
  templateUrl: './app.component.html',
  styleUrl: './app.component.css',
})
export class AppComponent implements AfterViewInit, OnDestroy, OnInit {
  @ViewChild('map', { static: true }) private readonly mapElement!: ElementRef<HTMLDivElement>;
  @ViewChild('gpxInput') private readonly gpxInput?: ElementRef<HTMLInputElement>;

  homeAddress = 'Bossepleinstraat 121, 3130 Betekom';
  resolvedHomeLabel = this.homeAddress;
  targetDistanceKm = 8;
  estimatedPaceMinPerKm = 6;
  preferPaved = true;
  preferUnpaved = false;
  preferUnrunRoads = true;

  route?: RouteResponse;
  historyStatus?: HistoryStatus;
  avoidedRoads: AvoidedRoad[] = [];
  markedRoads: MarkedRoad[] = [];
  errorMessage = '';
  historyMessage = '';
  avoidanceMessage = '';
  surfaceMessage = '';
  isGenerating = false;
  isImportingGpx = false;
  isSyncingHistory = false;
  isSavingAvoidedRoad = false;
  isSavingMarkedRoad = false;
  roadDialogVisible = false;
  avoidedRoadsCollapsed = true;
  markedRoadsCollapsed = true;
  selectedSegment?: RouteSegment;
  selectedSegmentCoordinate?: Coordinate;
  selectedAvoidanceReason: AvoidanceReason = 'busy_road';
  selectedMarkedSurface: MarkedRoadSurface = 'paved';
  avoidanceReasonOptions = [
    { label: 'Busy road', value: 'busy_road' },
    { label: 'No lights', value: 'no_lights' },
    { label: 'Not accessible', value: 'not_accessible' },
    { label: 'Other', value: 'other' },
  ];
  surfaceMarkOptions = [
    { label: 'Paved', value: 'paved' },
    { label: 'Unpaved', value: 'unpaved' },
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
    this.loadMarkedRoads();
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

  setPreferPaved(value: boolean): void {
    this.preferPaved = value;
    if (value) {
      this.preferUnpaved = false;
    }
  }

  setPreferUnpaved(value: boolean): void {
    this.preferUnpaved = value;
    if (value) {
      this.preferPaved = false;
    }
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
        next: (road) => {
          this.isSavingAvoidedRoad = false;
          this.roadDialogVisible = false;
          this.avoidanceMessage = 'Road added to your avoid list.';
          this.upsertAvoidedRoad(road);
          this.redrawCurrentRoute();
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
        this.avoidedRoads = this.avoidedRoads.filter((savedRoad) => savedRoad.id !== road.id);
        this.redrawCurrentRoute();
        this.loadAvoidedRoads();
      },
      error: (err: HttpErrorResponse) => {
        this.errorMessage = this.errorText(err);
      },
    });
  }

  saveMarkedRoad(): void {
    if (!this.selectedSegment?.osmWayId || !this.selectedSegmentCoordinate) {
      this.errorMessage = 'This route segment does not have road metadata and cannot be marked yet.';
      return;
    }

    this.errorMessage = '';
    this.surfaceMessage = '';
    this.isSavingMarkedRoad = true;
    this.routeApi
      .markRoadSurface({
        osmWayId: this.selectedSegment.osmWayId,
        name: this.selectedSegment.name,
        surface: this.selectedMarkedSurface,
        coordinate: this.selectedSegmentCoordinate,
      })
      .subscribe({
        next: () => {
          this.isSavingMarkedRoad = false;
          this.roadDialogVisible = false;
          this.surfaceMessage = 'Road surface saved.';
          this.updateCurrentRouteSurface(this.selectedSegment!.osmWayId!, this.selectedMarkedSurface);
          this.loadMarkedRoads();
        },
        error: (err: HttpErrorResponse) => {
          this.isSavingMarkedRoad = false;
          this.errorMessage = this.errorText(err);
        },
      });
  }

  removeMarkedRoad(road: MarkedRoad): void {
    this.errorMessage = '';
    this.surfaceMessage = '';
    this.routeApi.deleteMarkedRoad(road.id).subscribe({
      next: () => {
        this.surfaceMessage = 'Road surface mark removed.';
        this.loadMarkedRoads();
      },
      error: (err: HttpErrorResponse) => {
        this.errorMessage = this.errorText(err);
      },
    });
  }

  generateRoute(): void {
    if (!this.isValidForm()) {
      this.errorMessage = 'Check the home address, route length, and pace.';
      return;
    }

    this.errorMessage = '';
    this.avoidanceMessage = '';
    this.surfaceMessage = '';
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

  triggerGpxImport(): void {
    this.gpxInput?.nativeElement.click();
  }

  importGpxFile(event: Event): void {
    const input = event.target as HTMLInputElement;
    const file = input.files?.[0];
    input.value = '';
    if (!file) {
      return;
    }

    this.errorMessage = '';
    this.avoidanceMessage = '';
    this.surfaceMessage = '';
    this.isImportingGpx = true;

    file
      .text()
      .then((content) => {
        const coordinates = this.parseGpxCoordinates(content);
        this.routeApi
          .importRoute({
            coordinates,
            estimatedPaceMinPerKm: this.estimatedPaceMinPerKm,
          })
          .subscribe({
            next: (route) => {
              this.route = route;
              this.isImportingGpx = false;
              this.resolvedHomeLabel = file.name;
              this.homeLat = route.start.lat;
              this.homeLon = route.start.lon;
              this.drawHomeMarker();
              this.drawRoute(route);
            },
            error: (err: HttpErrorResponse) => {
              this.isImportingGpx = false;
              this.errorMessage = this.errorText(err);
            },
          });
      })
      .catch(() => {
        this.isImportingGpx = false;
        this.errorMessage = 'Could not read the selected GPX file.';
      });
  }

  private isValidForm(): boolean {
    return (
      this.homeAddress.trim().length >= 3 &&
      this.targetDistanceKm >= 1 &&
      this.targetDistanceKm <= 100 &&
      this.estimatedPaceMinPerKm >= 2 &&
      this.estimatedPaceMinPerKm <= 20
    );
  }

  private generateRouteFromResolvedHome(): void {
    const minPavedPercent = this.preferPaved ? 95 : 0;

    this.routeApi
      .generateRoute({
        home: this.homeCoordinate(),
        targetDistanceKm: this.targetDistanceKm,
        maxStartDistanceKm: 0,
        estimatedPaceMinPerKm: this.estimatedPaceMinPerKm,
        preferPaved: this.preferPaved,
        preferUnpaved: this.preferUnpaved,
        minPavedPercent,
        surfacePolicy: 'assume_paved',
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

  private parseGpxCoordinates(content: string): Coordinate[] {
    const document = new DOMParser().parseFromString(content, 'application/xml');
    if (document.querySelector('parsererror')) {
      throw new Error('invalid gpx');
    }
    const points = Array.from(document.querySelectorAll('trkpt, rtept'));
    const coordinates = points
      .map((point) => ({
        lat: Number(point.getAttribute('lat')),
        lon: Number(point.getAttribute('lon')),
      }))
      .filter((coordinate) => Number.isFinite(coordinate.lat) && Number.isFinite(coordinate.lon));
    if (coordinates.length < 2) {
      throw new Error('missing gpx points');
    }
    return coordinates;
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
        this.redrawCurrentRoute();
      },
      error: () => {
        this.avoidedRoads = [];
      },
    });
  }

  private loadMarkedRoads(): void {
    this.routeApi.getMarkedRoads().subscribe({
      next: (roads) => {
        this.markedRoads = roads;
      },
      error: () => {
        this.markedRoads = [];
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

  private drawRoute(route: RouteResponse, fitRoute = true): void {
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
      const overlayStyle = this.segmentOverlayStyle(segment);
      if (!overlayStyle) {
        continue;
      }
      const from = latLngs[segment.fromIndex];
      const to = latLngs[segment.toIndex];
      if (!from || !to) {
        continue;
      }
      L.polyline([from, to], {
        ...overlayStyle,
        opacity: 0.95,
        lineCap: 'round',
        lineJoin: 'round',
        renderer: this.routeRenderer,
      }).addTo(group);
    }

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
        .on('click', (event: L.LeafletMouseEvent) => this.openRoadDialog(segment, event.latlng))
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
    if (fitRoute) {
      this.fitRouteAroundHome(latLngs);
    }
  }

  private updateCurrentRouteSurface(osmWayId: number, surface: 'paved' | 'unpaved'): void {
    if (!this.route?.segments) {
      return;
    }

    const updatedRoute: RouteResponse = {
      ...this.route,
      segments: this.route.segments.map((segment) =>
        segment.osmWayId === osmWayId
          ? {
              ...segment,
              surface,
            }
          : segment,
      ),
    };
    this.updateRouteSurfacePercentages(updatedRoute);
    this.route = updatedRoute;
    this.drawRoute(updatedRoute, false);
  }

  private upsertAvoidedRoad(road: AvoidedRoad): void {
    this.avoidedRoads = [road, ...this.avoidedRoads.filter((savedRoad) => savedRoad.id !== road.id)];
  }

  private redrawCurrentRoute(): void {
    if (this.route) {
      this.drawRoute(this.route, false);
    }
  }

  private segmentOverlayStyle(segment: RouteSegment): L.PolylineOptions | undefined {
    if (this.isAvoidedRoadSegment(segment)) {
      return { color: '#dc2626', weight: 8 };
    }
    switch (segment.surface) {
      case 'unknown':
        return { color: '#2563eb', weight: 7 };
      case 'unpaved':
        return { color: '#16a34a', weight: 7, dashArray: '8 7' };
      default:
        return undefined;
    }
  }

  private isAvoidedRoadSegment(segment: RouteSegment): boolean {
    return !!segment.osmWayId && this.avoidedRoads.some((road) => road.osmWayId === segment.osmWayId);
  }

  private updateRouteSurfacePercentages(route: RouteResponse): void {
    const segments = route.segments || [];
    const total = segments.reduce((sum, segment) => sum + segment.distanceM, 0);
    if (total <= 0) {
      return;
    }

    const paved = segments
      .filter((segment) => segment.surface === 'paved')
      .reduce((sum, segment) => sum + segment.distanceM, 0);
    const unpaved = segments
      .filter((segment) => segment.surface === 'unpaved')
      .reduce((sum, segment) => sum + segment.distanceM, 0);
    const unknown = Math.max(0, total - paved - unpaved);

    route.pavedPercent = this.roundPercent((paved / total) * 100);
    route.unpavedPercent = this.roundPercent((unpaved / total) * 100);
    route.unknownSurfacePercent = this.roundPercent((unknown / total) * 100);
  }

  private roundPercent(value: number): number {
    return Math.round(value * 10) / 10;
  }

  private fitRouteAroundHome(latLngs: L.LatLngTuple[]): void {
    if (!this.map || latLngs.length === 0) {
      return;
    }

    const home = L.latLng(this.homeLat, this.homeLon);
    const padding = 56;
    const mapMaxZoom = this.map.getMaxZoom();
    const minZoom = Math.max(0, this.map.getMinZoom() ?? 0);
    const maxZoom = Math.max(minZoom, Number.isFinite(mapMaxZoom) ? Math.min(15, mapMaxZoom) : 15);
    const size = this.map.invalidateSize(false).getSize();
    const availableX = Math.max(1, size.x / 2 - padding);
    const availableY = Math.max(1, size.y / 2 - padding);

    let selectedZoom = minZoom;
    for (let zoom = maxZoom; zoom >= minZoom; zoom--) {
      const homePoint = this.map.project(home, zoom);
      const routeFits = latLngs.every(([lat, lon]) => {
        const point = this.map!.project([lat, lon], zoom);
        return Math.abs(point.x - homePoint.x) <= availableX && Math.abs(point.y - homePoint.y) <= availableY;
      });
      if (routeFits) {
        selectedZoom = zoom;
        break;
      }
    }

    this.map.setView(home, selectedZoom, { animate: false });
  }

  private openRoadDialog(segment: RouteSegment, latLng: L.LatLng): void {
    this.selectedSegment = segment;
    this.selectedSegmentCoordinate = { lat: latLng.lat, lon: latLng.lng };
    this.selectedAvoidanceReason = 'busy_road';
    this.selectedMarkedSurface = 'paved';
    this.roadDialogVisible = true;
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

  surfaceLabel(surface: MarkedRoadSurface): string {
    return surface === 'paved' ? 'Paved' : 'Unpaved';
  }
}
