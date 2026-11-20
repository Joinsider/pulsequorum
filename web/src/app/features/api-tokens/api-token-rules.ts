// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ApiToken } from '../../api/gen/model';
import { problemOf } from '../../shared/problem';

/** Translation key for a failed list, revoke or create call on the token page. */
export function apiTokenErrorKey(err: unknown): string {
  switch (problemOf(err)?.status) {
    case 403:
      return 'apiTokens.error.forbidden';
    case 404:
      return 'apiTokens.error.notFound';
    case 409:
      return 'apiTokens.error.conflict';
    case 503:
      return 'apiTokens.error.unavailable';
    default:
      return 'apiTokens.error.generic';
  }
}

/** Active tokens first, then expired ones; each group newest first by createdAt. Does not mutate the input. */
export function sortTokens(tokens: readonly ApiToken[]): ApiToken[] {
  return [...tokens].sort((a, b) => {
    if (a.expired !== b.expired) {
      return a.expired ? 1 : -1;
    }
    return Date.parse(b.createdAt) - Date.parse(a.createdAt);
  });
}
