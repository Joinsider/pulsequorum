// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { Routes } from '@angular/router';

/** Tenant audit log (`/audit`, FA-DAT-01). The platform view lives under `/admin/audit`. */
export const routes: Routes = [
  {
    path: '',
    loadComponent: () => import('./audit-page').then((m) => m.AuditPage),
    data: { scope: 'tenant' },
  },
];
