// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

export const LANGUAGES = ['en', 'de'] as const;
export type Language = (typeof LANGUAGES)[number];

const STORAGE_KEY = 'uc.lang';

/** The stored choice, else the browser language, else English (FA-UI-09). */
export function initialLanguage(): Language {
  let stored: string | null = null;
  try {
    stored = localStorage.getItem(STORAGE_KEY);
  } catch {
    // storage may be blocked
  }
  const candidates = [stored, ...(navigator.languages ?? [navigator.language])];
  for (const c of candidates) {
    const lang = c?.slice(0, 2).toLowerCase();
    if ((LANGUAGES as readonly string[]).includes(lang ?? '')) {
      return lang as Language;
    }
  }
  return 'en';
}

export function storeLanguage(lang: Language): void {
  try {
    localStorage.setItem(STORAGE_KEY, lang);
  } catch {
    // storage may be blocked
  }
  document.documentElement.lang = lang;
}
