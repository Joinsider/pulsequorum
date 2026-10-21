// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Routes } from '@angular/router';

/** Create mode: mounted at `/monitors/new`. */
export const createRoutes: Routes = [
  {
    path: '',
    data: { mode: 'create' },
    loadComponent: () => import('./monitor-editor-page').then((m) => m.MonitorEditorPage),
  },
];

/** Edit mode: mounted at `/monitors/:id/edit`; the id is read from the parent route. */
export const editRoutes: Routes = [
  {
    path: '',
    data: { mode: 'edit' },
    loadComponent: () => import('./monitor-editor-page').then((m) => m.MonitorEditorPage),
  },
];
