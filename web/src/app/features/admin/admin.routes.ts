// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { Routes } from '@angular/router';
import { provideTranslocoScope } from '@jsverse/transloco';

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
  {
    path: 'tenants',
    loadComponent: () => import('./tenants-page').then((m) => m.TenantsPage),
  },
  {
    path: 'plans',
    loadComponent: () => import('./plans-page').then((m) => m.PlansPage),
  },
  {
    path: 'audit',
    providers: [provideTranslocoScope('audit')],
    loadComponent: () => import('../audit/audit-page').then((m) => m.AuditPage),
    data: { scope: 'platform' },
  },
  {
    path: 'system-channels',
    providers: [provideTranslocoScope('channels')],
    children: [
      {
        path: '',
        loadComponent: () => import('../channels/channels-page').then((m) => m.ChannelsPage),
        data: { scope: 'system' },
      },
      {
        path: 'new',
        loadComponent: () =>
          import('../channels/channel-editor-page').then((m) => m.ChannelEditorPage),
        data: { scope: 'system' },
      },
      {
        path: ':channelId',
        loadComponent: () =>
          import('../channels/channel-editor-page').then((m) => m.ChannelEditorPage),
        data: { scope: 'system' },
      },
    ],
  },
];
