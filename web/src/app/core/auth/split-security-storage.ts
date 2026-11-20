// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { Injectable } from '@angular/core';
import { AbstractSecurityStorage } from 'angular-auth-oidc-client';

/**
 * Properties of the library's per-config blob that must survive the top-level
 * redirect to the identity provider. Everything else, above all the tokens
 * (authnResult, authzData, userData, reusable_refresh_token), stays in memory
 * (NFA-SEC-04, ADR 0027).
 */
export const REDIRECT_STATE_KEYS: ReadonlySet<string> = new Set([
  'authStateControl',
  'authNonce',
  'codeVerifier',
  'storageCodeFlowInProgress',
  'storageCustomParamsAuthRequest',
  'redirect',
]);

/** Prefix of the sessionStorage entries this storage writes. */
export const REDIRECT_STATE_PREFIX = 'uc.oidc-redirect:';

type Blob = Record<string, unknown>;

function parse(value: string | null | undefined): Blob {
  if (!value) {
    return {};
  }
  try {
    const v: unknown = JSON.parse(value);
    return v && typeof v === 'object' && !Array.isArray(v) ? (v as Blob) : {};
  } catch {
    return {};
  }
}

/** A redirect property counts only while it holds a value the library set. */
function present(v: unknown): boolean {
  return v !== null && v !== undefined && v !== '' && v !== false;
}

/**
 * Storage for angular-auth-oidc-client that keeps tokens in memory only and
 * puts only the short-lived redirect state into sessionStorage. The library
 * always writes the whole blob of a config (read-modify-write), so splitting
 * by property and merging on read is lossless.
 */
@Injectable()
export class SplitSecurityStorage extends AbstractSecurityStorage {
  private readonly memory = new Map<string, string>();

  read(key: string): string | null {
    const mem = this.memory.get(key);
    const session = this.sessionGet(key);
    if (mem === undefined && session === null) {
      return null;
    }
    return JSON.stringify({ ...parse(mem), ...parse(session) });
  }

  write(key: string, value: string): void {
    const memPart: Blob = {};
    const redirectPart: Blob = {};
    for (const [prop, v] of Object.entries(parse(value))) {
      if (!REDIRECT_STATE_KEYS.has(prop)) {
        memPart[prop] = v;
      } else if (present(v)) {
        redirectPart[prop] = v;
      }
    }
    this.memory.set(key, JSON.stringify(memPart));
    if (Object.keys(redirectPart).length > 0) {
      this.sessionSet(key, JSON.stringify(redirectPart));
    } else {
      this.clearRedirectState(key);
    }
  }

  remove(key: string): void {
    this.memory.delete(key);
    this.clearRedirectState(key);
  }

  clear(): void {
    this.memory.clear();
    try {
      for (let i = sessionStorage.length - 1; i >= 0; i--) {
        const k = sessionStorage.key(i);
        if (k?.startsWith(REDIRECT_STATE_PREFIX)) {
          sessionStorage.removeItem(k);
        }
      }
    } catch {
      // No sessionStorage: nothing was stored there.
    }
  }

  /**
   * Removes the redirect state of a config. The library leaves codeVerifier
   * and the custom parameters behind after a successful callback, so the
   * auth service calls this once the callback is processed.
   */
  clearRedirectState(key: string): void {
    try {
      sessionStorage.removeItem(REDIRECT_STATE_PREFIX + key);
    } catch {
      // Ignore: storage disabled.
    }
  }

  private sessionGet(key: string): string | null {
    try {
      return sessionStorage.getItem(REDIRECT_STATE_PREFIX + key);
    } catch {
      return null;
    }
  }

  private sessionSet(key: string, value: string): void {
    try {
      sessionStorage.setItem(REDIRECT_STATE_PREFIX + key, value);
    } catch {
      // Without sessionStorage the redirect cannot complete; tokens are unaffected.
    }
  }
}
