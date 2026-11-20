// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { problemOf } from '../../shared/problem';

/**
 * Text for a failed request: the problem detail or title when the server sent
 * one (RFC 9457), otherwise the translated fallback. The server keeps secrets
 * out of problem details, so they are safe to show.
 */
export function failureText(err: unknown, fallback: string): string {
  const problem = problemOf(err);
  return problem?.detail ?? problem?.title ?? fallback;
}
