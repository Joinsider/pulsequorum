// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Routes } from '@angular/router';

export const routes: Routes = [
  {
    path: '',
    loadComponent: () => import('./incidents-list-page').then((m) => m.IncidentsListPage),
  },
  {
    path: ':incidentId',
    loadComponent: () => import('./incident-detail-page').then((m) => m.IncidentDetailPage),
  },
];
