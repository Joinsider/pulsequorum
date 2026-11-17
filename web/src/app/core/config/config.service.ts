// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpClient } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { AppConfig } from './app-config';

const CONFIG_URL = '/config.json';

const FALLBACK_CONFIG: AppConfig = {
  nodes: [],
  auth: { mode: 'local' },
};

/** Loads the runtime configuration once at application start. */
@Injectable({ providedIn: 'root' })
export class ConfigService {
  private readonly http = inject(HttpClient);
  private readonly configSignal = signal<AppConfig>(FALLBACK_CONFIG);
  private resolveLoaded!: (config: AppConfig) => void;
  private readonly loaded = new Promise<AppConfig>((resolve) => {
    this.resolveLoaded = resolve;
  });

  /** The loaded configuration (or the fallback if loading failed). */
  readonly config = this.configSignal.asReadonly();

  /**
   * Resolves with the configuration once `load()` has settled, for code that
   * must not run before the runtime configuration is known (e.g. the OIDC loader).
   */
  whenLoaded(): Promise<AppConfig> {
    return this.loaded;
  }

  /** Fetches `/config.json`. Never rejects; falls back to a local-only configuration on error. */
  async load(): Promise<void> {
    try {
      const config = await firstValueFrom(this.http.get<AppConfig>(CONFIG_URL));
      this.configSignal.set(config);
    } catch (error) {
      console.warn(`Could not load ${CONFIG_URL}, using defaults.`, error);
      this.configSignal.set(FALLBACK_CONFIG);
    }
    this.resolveLoaded(this.configSignal());
  }

  /** Base URL for API calls without trailing slash, or `''` for same-origin requests. */
  apiBaseUrl(): string {
    const first = this.configSignal().nodes[0];
    return first ? first.replace(/\/+$/, '') : '';
  }
}
