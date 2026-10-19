// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpInterceptorFn } from '@angular/common/http';
import { inject } from '@angular/core';
import { ConfigService } from '../config/config.service';

/**
 * Prefixes relative API calls (`/api/...`, as produced by the generated client)
 * with the node URL from the runtime configuration. Same-origin when no node is
 * configured.
 */
export const apiBaseUrlInterceptor: HttpInterceptorFn = (req, next) => {
  if (!req.url.startsWith('/api/')) {
    return next(req);
  }
  const base = inject(ConfigService).apiBaseUrl();
  return next(base ? req.clone({ url: base + req.url }) : req);
};
