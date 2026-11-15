// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpContext, HttpContextToken } from '@angular/common/http';
import { DestroyRef, Injectable, computed, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { firstValueFrom } from 'rxjs';
import { AuthService as AuthApi } from '../../api/gen/endpoints/auth/auth.service';
import { Me, Role, TokenResponse } from '../../api/gen/model';

/** Marks requests that must not carry the bearer token or trigger a refresh. */
export const SKIP_AUTH = new HttpContextToken<boolean>(() => false);

/** Header the backend requires on cookie-authenticated calls (ADR 0008, CSRF). */
const CSRF_HEADERS = { 'X-Requested-With': 'uptimecluster' };

/** Refresh this long before the access token expires. */
const REFRESH_LEAD_MS = 60_000;
const MIN_REFRESH_DELAY_MS = 5_000;

export type AuthStatus = 'unknown' | 'authenticated' | 'anonymous';

/**
 * Local session (ADR 0008, NFA-SEC-04): the access token lives only in memory;
 * the refresh token is an HttpOnly cookie the SPA never sees. A page reload
 * restores the session with one refresh call.
 */
@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly api = inject(AuthApi);
  private readonly router = inject(Router);

  private token: string | null = null;
  private expiresAt = 0;
  private refreshTimer: ReturnType<typeof setTimeout> | undefined;
  private refreshing: Promise<boolean> | null = null;
  private restoring: Promise<void> | null = null;

  private readonly statusSignal = signal<AuthStatus>('unknown');
  private readonly meSignal = signal<Me | null>(null);

  readonly status = this.statusSignal.asReadonly();
  readonly me = this.meSignal.asReadonly();
  readonly authenticated = computed(() => this.statusSignal() === 'authenticated');
  /** Convenience for showing or hiding UI; the server enforces every permission. */
  readonly canEdit = computed(() =>
    this.hasRole('tenant-editor', 'tenant-admin', 'platform-admin'),
  );
  readonly canAdmin = computed(() => this.hasRole('tenant-admin', 'platform-admin'));
  /** Cluster administration (FA-UI-05); the server allows it to platform admins only. */
  readonly canPlatformAdmin = computed(() => this.hasRole('platform-admin'));

  constructor() {
    inject(DestroyRef).onDestroy(() => clearTimeout(this.refreshTimer));
  }

  /** The current access token, or null. Only the auth interceptor should need it. */
  accessToken(): string | null {
    return this.token && Date.now() < this.expiresAt ? this.token : null;
  }

  /** Tries once to resume a session from the refresh cookie. Safe to call repeatedly. */
  restore(): Promise<void> {
    if (this.statusSignal() !== 'unknown') {
      return Promise.resolve();
    }
    this.restoring ??= this.refresh().then(() => undefined);
    return this.restoring;
  }

  async login(username: string, password: string): Promise<void> {
    const t = await firstValueFrom(this.api.login({ username, password }, this.cookieOptions()));
    await this.start(t);
  }

  async setup(
    setupToken: string,
    username: string,
    password: string,
    displayName?: string,
  ): Promise<void> {
    const t = await firstValueFrom(
      this.api.setup(
        { setupToken, username, password, displayName: displayName || undefined },
        this.cookieOptions(),
      ),
    );
    await this.start(t);
  }

  /**
   * Gets a new access token from the refresh cookie. Concurrent callers share
   * one request, because the server rotates the cookie and treats reuse of an
   * old one as theft (family revocation).
   */
  refresh(): Promise<boolean> {
    this.refreshing ??= (async () => {
      try {
        const t = await firstValueFrom(this.api.refresh(this.cookieOptions()));
        await this.start(t);
        return true;
      } catch {
        this.clear();
        return false;
      } finally {
        this.refreshing = null;
      }
    })();
    return this.refreshing;
  }

  async logout(): Promise<void> {
    try {
      await firstValueFrom(this.api.logout(this.cookieOptions()));
    } catch {
      // The local session ends regardless; the cookie expires on its own.
    }
    this.clear();
    await this.router.navigateByUrl('/login');
  }

  /** Ends the session after the server rejected it and sends the user to the login. */
  async sessionExpired(): Promise<void> {
    const returnUrl = this.router.url;
    this.clear();
    await this.router.navigate(['/login'], {
      queryParams: { returnUrl: safeReturnUrl(returnUrl) === '/' ? null : returnUrl, expired: 1 },
    });
  }

  /** Reloads the profile, e.g. after the user changed it. */
  async reloadMe(): Promise<void> {
    this.meSignal.set(await firstValueFrom(this.api.getMe()));
  }

  private async start(t: TokenResponse): Promise<void> {
    this.token = t.accessToken;
    this.expiresAt = Date.parse(t.expiresAt);
    this.schedule();
    if (!this.meSignal()) {
      // getMe goes through the interceptor, which now has the token.
      this.meSignal.set(await firstValueFrom(this.api.getMe()));
    }
    this.statusSignal.set('authenticated');
  }

  private schedule(): void {
    clearTimeout(this.refreshTimer);
    const delay = Math.max(this.expiresAt - Date.now() - REFRESH_LEAD_MS, MIN_REFRESH_DELAY_MS);
    this.refreshTimer = setTimeout(() => {
      void this.refresh().then((ok) => {
        if (!ok) {
          void this.sessionExpired();
        }
      });
    }, delay);
  }

  private clear(): void {
    clearTimeout(this.refreshTimer);
    this.token = null;
    this.expiresAt = 0;
    this.meSignal.set(null);
    this.statusSignal.set('anonymous');
  }

  private hasRole(...roles: Role[]): boolean {
    const me = this.meSignal();
    return !!me && me.roles.some((r) => roles.includes(r));
  }

  /** Cookie calls: credentials for an external frontend on another origin (PF-34). */
  private cookieOptions() {
    return {
      headers: CSRF_HEADERS,
      withCredentials: true,
      context: new HttpContext().set(SKIP_AUTH, true),
    };
  }
}

/**
 * Accepts only in-app absolute paths as redirect targets after login, so a
 * crafted link cannot send the user to another site (open redirect).
 */
export function safeReturnUrl(url: unknown): string {
  if (
    typeof url !== 'string' ||
    !url.startsWith('/') ||
    url.startsWith('//') ||
    url.includes('\\')
  ) {
    return '/';
  }
  if (url.startsWith('/login') || url.startsWith('/setup')) {
    return '/';
  }
  return url;
}
