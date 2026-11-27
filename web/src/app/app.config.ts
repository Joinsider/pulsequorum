// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ApplicationConfig,
  inject,
  isDevMode,
  provideAppInitializer,
  provideBrowserGlobalErrorListeners,
} from '@angular/core';
import { provideHttpClient, withFetch, withInterceptors } from '@angular/common/http';
import { provideRouter, withComponentInputBinding } from '@angular/router';
import { provideTransloco } from '@jsverse/transloco';
import {
  AbstractSecurityStorage,
  StsConfigHttpLoader,
  StsConfigLoader,
  provideAuth,
} from 'angular-auth-oidc-client';
import { from, map, shareReplay } from 'rxjs';
import { MAT_FORM_FIELD_DEFAULT_OPTIONS } from '@angular/material/form-field';
import { routes } from './app.routes';
import { apiBaseUrlInterceptor } from './core/api/api-base-url.interceptor';
import { NodeSelector } from './core/failover/node-selector.service';
import { failoverInterceptor } from './core/failover/failover.interceptor';
import { authInterceptor } from './core/auth/auth.interceptor';
import { SplitSecurityStorage } from './core/auth/split-security-storage';
import { toOpenIdConfiguration } from './core/auth/oidc-config';
import { ConfigService } from './core/config/config.service';
import { HttpTranslocoLoader } from './core/i18n/transloco-loader';
import { LANGUAGES, initialLanguage } from './core/i18n/language';

export const appConfig: ApplicationConfig = {
  providers: [
    provideBrowserGlobalErrorListeners(),
    // Outline fields everywhere, and dynamic subscripts so hints never overlap the next row.
    {
      provide: MAT_FORM_FIELD_DEFAULT_OPTIONS,
      useValue: { appearance: 'outline', subscriptSizing: 'dynamic' },
    },
    // Order matters: the auth interceptor only trusts relative /api/ URLs, before the
    // base-URL interceptor turns them into absolute ones (ADR 0036). The failover
    // interceptor comes last: it sees the absolute node URL and retries with a relative one.
    provideHttpClient(
      withFetch(),
      withInterceptors([authInterceptor, apiBaseUrlInterceptor, failoverInterceptor]),
    ),
    provideAppInitializer(async () => {
      const config = inject(ConfigService);
      const selector = inject(NodeSelector);
      await config.load();
      await selector.select();
    }),
    // The OIDC configuration derives from /config.json, which ConfigService loads at startup.
    provideAuth({
      loader: {
        provide: StsConfigLoader,
        useFactory: (config: ConfigService) =>
          new StsConfigHttpLoader(
            from(config.whenLoaded()).pipe(
              map((app) => toOpenIdConfiguration(app.auth, location.origin)),
              shareReplay(1),
            ),
          ),
        deps: [ConfigService],
      },
    }),
    // Must come after provideAuth: tokens stay in memory, only the redirect state uses sessionStorage (NFA-SEC-04).
    { provide: AbstractSecurityStorage, useClass: SplitSecurityStorage },
    provideRouter(routes, withComponentInputBinding()),
    provideTransloco({
      config: {
        availableLangs: [...LANGUAGES],
        defaultLang: initialLanguage(),
        fallbackLang: 'en',
        missingHandler: { useFallbackTranslation: true },
        reRenderOnLangChange: true,
        prodMode: !isDevMode(),
      },
      loader: HttpTranslocoLoader,
    }),
  ],
};
