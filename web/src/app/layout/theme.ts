// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

/** `system` follows the OS setting; `light` and `dark` force a theme (FA-UI-08). */
export type ThemeChoice = 'system' | 'light' | 'dark';

const STORAGE_KEY = 'uc.theme';

/** The stored choice, or `system` when none is stored or storage is blocked. */
export function readStoredTheme(): ThemeChoice {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored === 'light' || stored === 'dark') {
      return stored;
    }
  } catch {
    // storage may be blocked; fall back to the system setting
  }
  return 'system';
}

export function storeTheme(choice: ThemeChoice): void {
  try {
    if (choice === 'system') {
      localStorage.removeItem(STORAGE_KEY);
    } else {
      localStorage.setItem(STORAGE_KEY, choice);
    }
  } catch {
    // storage may be blocked; the choice lasts for this page view only
  }
}

/** Sets or removes data-theme on <html>; styles.scss maps it to color-scheme. */
export function applyTheme(choice: ThemeChoice): void {
  const root = document.documentElement;
  if (choice === 'system') {
    root.removeAttribute('data-theme');
  } else {
    root.setAttribute('data-theme', choice);
  }
}

/** Cycles system -> light -> dark -> system. */
export function nextTheme(choice: ThemeChoice): ThemeChoice {
  switch (choice) {
    case 'system':
      return 'light';
    case 'light':
      return 'dark';
    case 'dark':
      return 'system';
  }
}
