// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { LogLevel, OpenIdConfiguration } from 'angular-auth-oidc-client';
import { AuthConfig } from '../config/app-config';

/** Config id of the library; also the key under which the library asks our storage. */
export const OIDC_CONFIG_ID = 'pulsequorum';

/** In-app route that receives the identity provider redirect (redirect_uri and silent login). */
export const CALLBACK_PATH = '/auth/callback';

const DEFAULT_SCOPES = ['openid', 'profile', 'offline_access'];

/**
 * Maps the backend's `auth` section of `/config.json` to the library configuration
 * (ADR 0027, SPA). In local mode the OIDC flow never starts, so the result is a
 * placeholder that only satisfies the loader.
 */
export function toOpenIdConfiguration(auth: AuthConfig, origin: string): OpenIdConfiguration {
  if (auth.mode !== 'oidc') {
    return { configId: OIDC_CONFIG_ID, authority: '', clientId: '' };
  }
  return {
    configId: OIDC_CONFIG_ID,
    authority: auth.issuer ?? '',
    clientId: auth.clientId ?? '',
    redirectUrl: `${origin}${CALLBACK_PATH}`,
    postLogoutRedirectUri: `${origin}/login`,
    scope: (auth.scopes?.length ? auth.scopes : DEFAULT_SCOPES).join(' '),
    responseType: 'code',
    // Refresh tokens instead of an iframe: silent renew works without third-party cookies.
    useRefreshToken: true,
    silentRenew: true,
    renewTimeBeforeTokenExpiresInSeconds: 60,
    startCheckSession: false,
    ignoreNonceAfterRefresh: true,
    triggerAuthorizationResultEvent: true,
    // Load the profile from userinfo: Zitadel and others put no name into the
    // ID token. userData stays in memory (SplitSecurityStorage).
    autoUserInfo: true,
    // Empty: the library's own auth interceptor is not used (its prefix match is unsafe).
    secureRoutes: [],
    historyCleanupOff: false,
    logLevel: LogLevel.Warn,
  };
}
