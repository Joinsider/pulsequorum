// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Routes } from '@angular/router';

/** Platform admin area (/admin), guarded by platformAdminGuard in app.routes.ts. */
export const routes: Routes = [
  {
    path: '',
    pathMatch: 'full',
    redirectTo: 'cluster',
  },
  {
    path: 'cluster',
    loadComponent: () => import('./cluster-page').then((m) => m.ClusterPage),
  },
];
