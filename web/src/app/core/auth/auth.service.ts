// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpContext, HttpContextToken } from '@angular/common/http';
import { DestroyRef, Injectable, Injector, computed, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import {
  AbstractSecurityStorage,
  EventTypes,
  OidcSecurityService,
  PublicEventsService,
} from 'angular-auth-oidc-client';
import { Subscription, firstValueFrom } from 'rxjs';
import { AuthService as AuthApi } from '../../api/gen/endpoints/auth/auth.service';
import { Me, Role, TokenResponse } from '../../api/gen/model';
import { ConfigService } from '../config/config.service';
import { CALLBACK_PATH, OIDC_CONFIG_ID } from './oidc-config';
import { SplitSecurityStorage } from './split-security-storage';

/** Marks requests that must not carry the bearer token or trigger a refresh. */
export const SKIP_AUTH = new HttpContextToken<boolean>(() => false);

/** Header the backend requires on cookie-authenticated calls (ADR 0008, CSRF). */
const CSRF_HEADERS = { 'X-Requested-With': 'uptimecluster' };

/** Refresh this long before the access token expires. */
const REFRESH_LEAD_MS = 60_000;
const MIN_REFRESH_DELAY_MS = 5_000;

/** sessionStorage flag (value "1"): this tab has used its one silent login. Holds no secret. */
const SILENT_TRIED_KEY = 'uc.oidc-silent-tried';
/** sessionStorage: the in-app path to show after the identity provider redirect. */
const RETURN_URL_KEY = 'uc.oidc-return';

export type AuthStatus = 'unknown' | 'authenticated' | 'anonymous';

/**
 * Session of the SPA (ADR 0008 local, ADR 0027 oidc, NFA-SEC-04): the access
 * token lives only in memory. Local mode: the refresh token is an HttpOnly
 * cookie the SPA never sees, and a reload restores the session with one
 * refresh call. OIDC mode: the identity provider's library holds the tokens in
 * memory, and refreshes them with refresh tokens.
 */
@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly api = inject(AuthApi);
  private readonly router = inject(Router);
  private readonly configService = inject(ConfigService);
  // Resolved lazily: local mode never touches the OIDC library.
  private readonly injector = inject(Injector);

  private token: string | null = null;
  private expiresAt = 0;
  private refreshTimer: ReturnType<typeof setTimeout> | undefined;
  private refreshing: Promise<boolean> | null = null;
  private restoring: Promise<void> | null = null;
  private eventsSub: Subscription | undefined;

  private readonly statusSignal = signal<AuthStatus>('unknown');
  private readonly meSignal = signal<Me | null>(null);
  /** OIDC mode: the ID token claims, kept in memory only (NFA-SEC-04). */
  private readonly claimsSignal = signal<IdTokenClaims | null>(null);

  readonly status = this.statusSignal.asReadonly();
  readonly me = this.meSignal.asReadonly();
  /** `local` for password accounts (ADR 0008), `oidc` for an external identity provider (ADR 0027). */
  readonly mode = computed(() => this.configService.config().auth.mode);
  readonly authenticated = computed(() => this.statusSignal() === 'authenticated');
  /** Convenience for showing or hiding UI; the server enforces every permission. */
  readonly canEdit = computed(() =>
    this.hasRole('tenant-editor', 'tenant-admin', 'platform-admin'),
  );
  readonly canAdmin = computed(() => this.hasRole('tenant-admin', 'platform-admin'));
  /** Cluster administration (FA-UI-05); the server allows it to platform admins only. */
  readonly canPlatformAdmin = computed(() => this.hasRole('platform-admin'));
  /** Name to show for the signed-in user; see {@link displayNameFor}. */
  readonly displayName = computed(() =>
    displayNameFor(this.mode(), this.claimsSignal(), this.meSignal()),
  );

  constructor() {
    inject(DestroyRef).onDestroy(() => {
      clearTimeout(this.refreshTimer);
      this.eventsSub?.unsubscribe();
    });
  }

  /** The current access token, or null. Only the auth interceptor should need it. */
  accessToken(): string | null {
    return this.token && Date.now() < this.expiresAt ? this.token : null;
  }

  /** Tries once to resume a session (refresh cookie, or the identity provider callback). Safe to call repeatedly. */
  restore(): Promise<void> {
    if (this.statusSignal() !== 'unknown') {
      return Promise.resolve();
    }
    this.restoring ??=
      this.mode() === 'oidc' ? this.restoreOidc() : this.refresh().then(() => undefined);
    return this.restoring;
  }

  /** Local mode: signs in with username and password. OIDC mode: redirects to the identity provider. */
  login(username: string, password: string): Promise<void>;
  login(returnUrl?: string): Promise<void>;
  login(usernameOrReturnUrl?: string, password?: string): Promise<void> {
    if (this.mode() === 'oidc') {
      return this.loginOidc(usernameOrReturnUrl);
    }
    return this.loginLocal(usernameOrReturnUrl ?? '', password ?? '');
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
   * Gets a new access token. Local mode: from the refresh cookie. OIDC mode: via
   * the library's refresh token. Concurrent callers share one request, because
   * the server rotates the cookie and treats reuse of an old one as theft
   * (family revocation).
   */
  refresh(): Promise<boolean> {
    this.refreshing ??= (async () => {
      try {
        return this.mode() === 'oidc' ? await this.refreshOidc() : await this.refreshLocal();
      } finally {
        this.refreshing = null;
      }
    })();
    return this.refreshing;
  }

  async logout(): Promise<void> {
    if (this.mode() === 'oidc') {
      this.clear();
      // No silent login after an explicit logout in this tab.
      writeSession(SILENT_TRIED_KEY, '1');
      // GET redirect to the provider's end_session endpoint; the provider sends the user back to /login.
      this.oidc()
        .logoff(OIDC_CONFIG_ID)
        .subscribe({ complete: () => this.navigateToLogin(), error: () => this.navigateToLogin() });
      return;
    }
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
    if (this.mode() === 'oidc') {
      this.oidc().logoffLocal(OIDC_CONFIG_ID);
      // Show the expired notice instead of silently signing in again.
      writeSession(SILENT_TRIED_KEY, '1');
    }
    this.clear();
    await this.router.navigate(['/login'], {
      queryParams: { returnUrl: safeReturnUrl(returnUrl) === '/' ? null : returnUrl, expired: 1 },
    });
  }

  /** Reloads the profile, e.g. after the user changed it. */
  async reloadMe(): Promise<void> {
    this.meSignal.set(await firstValueFrom(this.api.getMe()));
  }

  /**
   * Returns the in-app path to show after an identity provider redirect, and
   * forgets it. `/` if none was stored.
   */
  takeReturnUrl(): string {
    const value = readSession(RETURN_URL_KEY);
    removeSession(RETURN_URL_KEY);
    return safeReturnUrl(value);
  }

  private async loginLocal(username: string, password: string): Promise<void> {
    const t = await firstValueFrom(this.api.login({ username, password }, this.cookieOptions()));
    await this.start(t);
  }

  private async refreshLocal(): Promise<boolean> {
    try {
      const t = await firstValueFrom(this.api.refresh(this.cookieOptions()));
      await this.start(t);
      return true;
    } catch {
      this.clear();
      return false;
    }
  }

  private async refreshOidc(): Promise<boolean> {
    try {
      const result = await firstValueFrom(
        this.oidc().forceRefreshSession(undefined, OIDC_CONFIG_ID),
      );
      if (!result.isAuthenticated || !this.cacheToken(result.accessToken)) {
        this.clear();
        return false;
      }
      return true;
    } catch {
      this.clear();
      return false;
    }
  }

  private async loginOidc(returnUrl?: string): Promise<void> {
    removeSession(SILENT_TRIED_KEY);
    writeSession(RETURN_URL_KEY, safeReturnUrl(returnUrl));
    this.oidc().authorize(OIDC_CONFIG_ID);
  }

  /**
   * OIDC start-up: finishes the redirect callback, or resumes an existing
   * library session. Never rejects; any failure leaves the session anonymous.
   */
  private async restoreOidc(): Promise<void> {
    let oidc: OidcSecurityService | undefined;
    try {
      this.wireOidcEvents();
      oidc = this.oidc();
      // Must run first: the token endpoint is only known after discovery, and the
      // library's storage keeps it in memory only.
      await firstValueFrom(oidc.preloadAuthWellKnownDocument(OIDC_CONFIG_ID));

      if (new URLSearchParams(location.search).has('error')) {
        // A failed prompt=none login or a refused consent comes back as ?error=...
        // The library reports it only as "no code in url", so it is handled here.
        this.storage().clearRedirectState(OIDC_CONFIG_ID);
        if (location.search) {
          history.replaceState(history.state, '', location.pathname);
        }
        this.statusSignal.set('anonymous');
        return;
      }

      const result = await firstValueFrom(oidc.checkAuth(undefined, OIDC_CONFIG_ID));
      if (result.isAuthenticated && this.cacheToken(result.accessToken)) {
        this.storage().clearRedirectState(OIDC_CONFIG_ID);
        // With autoUserInfo, the library's userData holds the userinfo claims.
        this.claimsSignal.set(isClaims(result.userData) ? result.userData : null);
        this.meSignal.set(await firstValueFrom(this.api.getMe()));
        this.statusSignal.set('authenticated');
        return;
      }

      this.statusSignal.set('anonymous');
      if (location.pathname !== CALLBACK_PATH && readSession(SILENT_TRIED_KEY) !== '1') {
        this.startSilentLogin();
      }
    } catch {
      this.clear();
      oidc?.logoffLocal(OIDC_CONFIG_ID);
    }
  }

  /** One attempt per tab without showing the login page (prompt=none). */
  private startSilentLogin(): void {
    writeSession(SILENT_TRIED_KEY, '1');
    writeSession(RETURN_URL_KEY, safeReturnUrl(location.pathname + location.search));
    this.oidc().authorize(OIDC_CONFIG_ID, { customParams: { prompt: 'none' } });
  }

  /** Keeps the cached token current and ends the session when the provider gives up. */
  private wireOidcEvents(): void {
    if (this.eventsSub) {
      return;
    }
    this.eventsSub = this.injector
      .get(PublicEventsService)
      .registerForEvents()
      .subscribe((event) => {
        switch (event.type) {
          case EventTypes.NewAuthenticationResult:
            void this.cacheLibraryToken();
            break;
          case EventTypes.TokenExpired:
          case EventTypes.SilentRenewFailed:
            if (this.authenticated()) {
              void this.sessionExpired();
            }
            break;
        }
      });
  }

  private async cacheLibraryToken(): Promise<void> {
    const token = await firstValueFrom(this.oidc().getAccessToken(OIDC_CONFIG_ID));
    if (token) {
      this.cacheToken(token);
    }
  }

  /** Stores the access token and its expiry from the JWT `exp` claim. False if there is none. */
  private cacheToken(accessToken: string): boolean {
    const expiresAt = jwtExpiryMs(accessToken);
    if (!expiresAt) {
      return false;
    }
    this.token = accessToken;
    this.expiresAt = expiresAt;
    return true;
  }

  private navigateToLogin(): void {
    void this.router.navigateByUrl('/login');
  }

  private oidc(): OidcSecurityService {
    return this.injector.get(OidcSecurityService);
  }

  /** Fails closed: OIDC mode must run with the split storage that keeps tokens in memory. */
  private storage(): SplitSecurityStorage {
    const storage = this.injector.get(AbstractSecurityStorage);
    if (!(storage instanceof SplitSecurityStorage)) {
      throw new Error('OIDC mode requires SplitSecurityStorage (NFA-SEC-04)');
    }
    return storage;
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
    this.claimsSignal.set(null);
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

/** Claims of the OIDC ID token as the library reports them. */
export type IdTokenClaims = Record<string, unknown>;

/**
 * Name to show for the signed-in user. OIDC mode prefers the identity provider's
 * `name`, then `preferred_username`, then `email`; the server profile is the
 * fallback, so the subject ID (`sub`) is shown only if nothing else exists.
 * Local mode always uses the server profile.
 */
export function displayNameFor(mode: string, claims: IdTokenClaims | null, me: Me | null): string {
  if (mode === 'oidc') {
    for (const key of ['name', 'preferred_username', 'email']) {
      const value = claims?.[key];
      if (typeof value === 'string' && value.trim() !== '') {
        return value.trim();
      }
    }
  }
  return me?.displayName || me?.username || '';
}

function isClaims(value: unknown): value is IdTokenClaims {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Reads `exp` (as epoch milliseconds) from a JWT payload. The signature is not
 * checked here: the server verifies every request. Returns 0 if there is no valid `exp`.
 */
export function jwtExpiryMs(jwt: string): number {
  const payload = base64UrlDecode(jwt.split('.')[1] ?? '');
  if (payload === null) {
    return 0;
  }
  try {
    const claims = JSON.parse(payload) as { exp?: unknown };
    return typeof claims.exp === 'number' && Number.isFinite(claims.exp) ? claims.exp * 1000 : 0;
  } catch {
    return 0;
  }
}

function base64UrlDecode(input: string): string | null {
  if (!/^[A-Za-z0-9_-]*$/.test(input)) {
    return null;
  }
  const padded = input
    .replace(/-/g, '+')
    .replace(/_/g, '/')
    .padEnd(Math.ceil(input.length / 4) * 4, '=');
  try {
    const bytes = Uint8Array.from(atob(padded), (c) => c.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return null;
  }
}

function readSession(key: string): string | null {
  try {
    return sessionStorage.getItem(key);
  } catch {
    // No sessionStorage: nothing was stored there.
    return null;
  }
}

function writeSession(key: string, value: string): void {
  try {
    sessionStorage.setItem(key, value);
  } catch {
    // Without sessionStorage the flag is lost; the worst case is one more silent login.
  }
}

function removeSession(key: string): void {
  try {
    sessionStorage.removeItem(key);
  } catch {
    // Ignore: storage disabled.
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
