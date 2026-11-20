// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { REDIRECT_STATE_PREFIX, SplitSecurityStorage } from './split-security-storage';

const ACCESS = 'eyJhbGciOiJSUzI1NiJ9.access.sig';
const REFRESH = 'refresh-token-secret';
const ID = 'eyJhbGciOiJSUzI1NiJ9.id.sig';

/** Every value the browser storages hold, as one string. */
function persisted(): string {
  const out: string[] = [];
  for (const s of [localStorage, sessionStorage]) {
    for (let i = 0; i < s.length; i++) {
      const k = s.key(i)!;
      out.push(k, s.getItem(k) ?? '');
    }
  }
  return out.join('\n');
}

describe('SplitSecurityStorage', () => {
  let storage: SplitSecurityStorage;

  beforeEach(() => {
    localStorage.clear();
    sessionStorage.clear();
    storage = new SplitSecurityStorage();
  });

  afterEach(() => {
    localStorage.clear();
    sessionStorage.clear();
  });

  it('never persists tokens or user data (NFA-SEC-04)', () => {
    storage.write(
      'cfg',
      JSON.stringify({
        authnResult: { access_token: ACCESS, refresh_token: REFRESH, id_token: ID },
        authzData: ACCESS,
        userData: { sub: 'u1', email: 'a@example.test' },
        reusable_refresh_token: REFRESH,
        access_token_expires_at: 123,
        codeVerifier: 'verifier',
      }),
    );

    const stored = persisted();
    for (const secret of [ACCESS, REFRESH, ID, 'a@example.test']) {
      expect(stored).not.toContain(secret);
    }
    expect(JSON.parse(storage.read('cfg')!)).toMatchObject({
      authzData: ACCESS,
      authnResult: { refresh_token: REFRESH },
      codeVerifier: 'verifier',
    });
  });

  it('keeps the redirect state in sessionStorage across a reload', () => {
    storage.write(
      'cfg',
      JSON.stringify({
        authStateControl: 'state-1',
        authNonce: 'nonce-1',
        codeVerifier: 'verifier-1',
        storageCodeFlowInProgress: true,
        authWellKnownEndPoints: { tokenEndpoint: 'https://id.example.test/token' },
      }),
    );

    const afterReload = new SplitSecurityStorage();
    expect(JSON.parse(afterReload.read('cfg')!)).toEqual({
      authStateControl: 'state-1',
      authNonce: 'nonce-1',
      codeVerifier: 'verifier-1',
      storageCodeFlowInProgress: true,
    });
  });

  it('drops redirect properties the library reset or removed', () => {
    storage.write('cfg', JSON.stringify({ authStateControl: 'state-1', codeVerifier: 'v' }));
    storage.write(
      'cfg',
      JSON.stringify({ authStateControl: '', authNonce: null, codeVerifier: 'v' }),
    );
    expect(JSON.parse(sessionStorage.getItem(REDIRECT_STATE_PREFIX + 'cfg')!)).toEqual({
      codeVerifier: 'v',
    });

    storage.write('cfg', JSON.stringify({ storageCodeFlowInProgress: false }));
    expect(sessionStorage.getItem(REDIRECT_STATE_PREFIX + 'cfg')).toBeNull();
  });

  it('clears the redirect state after the callback', () => {
    storage.write('cfg', JSON.stringify({ codeVerifier: 'v', authzData: ACCESS }));
    storage.clearRedirectState('cfg');
    expect(sessionStorage.getItem(REDIRECT_STATE_PREFIX + 'cfg')).toBeNull();
    expect(JSON.parse(storage.read('cfg')!)).toEqual({ authzData: ACCESS });
  });

  it('remove and clear touch only their own entries', () => {
    sessionStorage.setItem('other', 'x');
    storage.write('a', JSON.stringify({ codeVerifier: 'va' }));
    storage.write('b', JSON.stringify({ codeVerifier: 'vb' }));

    storage.remove('a');
    expect(storage.read('a')).toBeNull();
    expect(storage.read('b')).not.toBeNull();

    storage.clear();
    expect(storage.read('b')).toBeNull();
    expect(sessionStorage.getItem('other')).toBe('x');
  });

  it('returns null for unknown keys and survives malformed values', () => {
    expect(storage.read('missing')).toBeNull();
    storage.write('cfg', 'not json');
    expect(storage.read('cfg')).toBe('{}');
  });
});
