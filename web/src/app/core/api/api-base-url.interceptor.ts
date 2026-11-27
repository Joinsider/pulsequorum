// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpInterceptorFn } from '@angular/common/http';
import { inject } from '@angular/core';
import { NodeSelector } from '../failover/node-selector.service';

/**
 * Prefixes relative API calls (`/api/...`, as produced by the generated client)
 * with the node the {@link NodeSelector} currently uses (ADR 0036). Same-origin
 * when no node is selected.
 */
export const apiBaseUrlInterceptor: HttpInterceptorFn = (req, next) => {
  if (!req.url.startsWith('/api/')) {
    return next(req);
  }
  const base = inject(NodeSelector).current();
  return next(base ? req.clone({ url: base + req.url }) : req);
};
