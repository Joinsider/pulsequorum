import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { ConfigService } from '../config/config.service';
import { Health } from './health';

/** Reads the health endpoint of the backend node. */
@Injectable({ providedIn: 'root' })
export class HealthService {
  private readonly http = inject(HttpClient);
  private readonly config = inject(ConfigService);

  get(): Observable<Health> {
    return this.http.get<Health>(`${this.config.apiBaseUrl()}/api/v1/health`);
  }
}
