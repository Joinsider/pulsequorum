// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { LogLevel } from 'angular-auth-oidc-client';
import { describe, expect, it } from 'vitest';
import { CALLBACK_PATH, OIDC_CONFIG_ID, toOpenIdConfiguration } from './oidc-config';

describe('toOpenIdConfiguration', () => {
  const ORIGIN = 'https://uptime.example.test';

  it('maps the OIDC settings of /config.json (ADR 0027, SPA)', () => {
    const cfg = toOpenIdConfiguration(
      {
        mode: 'oidc',
        issuer: 'https://id.example.test/oauth/v2',
        clientId: 'uptime-spa',
        scopes: ['openid', 'profile', 'offline_access'],
      },
      ORIGIN,
    );

    expect(cfg).toMatchObject({
      configId: OIDC_CONFIG_ID,
      authority: 'https://id.example.test/oauth/v2',
      clientId: 'uptime-spa',
      redirectUrl: `${ORIGIN}/auth/callback`,
      postLogoutRedirectUri: `${ORIGIN}/login`,
      scope: 'openid profile offline_access',
      responseType: 'code',
      useRefreshToken: true,
      silentRenew: true,
      renewTimeBeforeTokenExpiresInSeconds: 60,
      startCheckSession: false,
      ignoreNonceAfterRefresh: true,
      triggerAuthorizationResultEvent: true,
      autoUserInfo: true,
      secureRoutes: [],
      historyCleanupOff: false,
      logLevel: LogLevel.Warn,
    });
    expect(cfg.redirectUrl).toBe(`${ORIGIN}${CALLBACK_PATH}`);
  });

  it('keeps the issuer exactly as given', () => {
    const issuer = 'https://id.example.test/';
    expect(toOpenIdConfiguration({ mode: 'oidc', issuer, clientId: 'c' }, ORIGIN).authority).toBe(
      issuer,
    );
  });

  it('uses the default scopes when none are configured', () => {
    expect(
      toOpenIdConfiguration(
        { mode: 'oidc', issuer: 'https://id.example.test', clientId: 'c' },
        ORIGIN,
      ).scope,
    ).toBe('openid profile offline_access');
  });

  it('returns an inert placeholder in local mode', () => {
    expect(toOpenIdConfiguration({ mode: 'local' }, ORIGIN)).toEqual({
      configId: OIDC_CONFIG_ID,
      authority: '',
      clientId: '',
    });
  });
});
