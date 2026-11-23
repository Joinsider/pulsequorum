// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { afterEach, describe, expect, it, vi } from 'vitest';
import { applyTheme, nextTheme, readStoredTheme, storeTheme } from './theme';

describe('theme', () => {
  afterEach(() => {
    localStorage.clear();
    delete document.documentElement.dataset['theme'];
    vi.restoreAllMocks();
  });

  it('reads stored light/dark theme and defaults to system', () => {
    expect(readStoredTheme()).toBe('system');
    localStorage.setItem('uc.theme', 'light');
    expect(readStoredTheme()).toBe('light');
    localStorage.setItem('uc.theme', 'dark');
    expect(readStoredTheme()).toBe('dark');
    localStorage.setItem('uc.theme', 'unknown');
    expect(readStoredTheme()).toBe('system');
  });

  it('falls back to system when localStorage throws', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(readStoredTheme()).toBe('system');
  });

  it('stores light/dark and removes the key for system', () => {
    storeTheme('dark');
    expect(localStorage.getItem('uc.theme')).toBe('dark');
    storeTheme('system');
    expect(localStorage.getItem('uc.theme')).toBeNull();
  });

  it('ignores storage errors when writing', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(() => storeTheme('light')).not.toThrow();
  });

  it('applies and clears data-theme on the document root', () => {
    applyTheme('dark');
    expect(document.documentElement.dataset['theme']).toBe('dark');
    applyTheme('system');
    expect(document.documentElement.dataset['theme']).toBeUndefined();
  });

  it('cycles system -> light -> dark -> system', () => {
    expect(nextTheme('system')).toBe('light');
    expect(nextTheme('light')).toBe('dark');
    expect(nextTheme('dark')).toBe('system');
  });
});
