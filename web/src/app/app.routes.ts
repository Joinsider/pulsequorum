// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Routes } from '@angular/router';
import { provideTranslocoScope } from '@jsverse/transloco';
import { adminGuard, authGuard, guestGuard } from './core/auth/auth.guards';

/**
 * Every feature is lazy-loaded and owns its translation scope
 * (`public/i18n/<scope>/<lang>.json`).
 */
export const routes: Routes = [
  {
    path: 'login',
    canActivate: [guestGuard],
    providers: [provideTranslocoScope('auth')],
    loadComponent: () => import('./features/auth/login-page').then((m) => m.LoginPage),
  },
  {
    path: 'setup',
    canActivate: [guestGuard],
    providers: [provideTranslocoScope('auth')],
    loadComponent: () => import('./features/auth/setup-page').then((m) => m.SetupPage),
  },
  {
    path: 'status',
    loadComponent: () => import('./features/status/status-page').then((m) => m.StatusPage),
  },
  {
    path: '',
    canActivate: [authGuard],
    loadComponent: () => import('./layout/shell').then((m) => m.Shell),
    children: [
      {
        path: '',
        pathMatch: 'full',
        providers: [provideTranslocoScope('dashboard')],
        loadChildren: () => import('./features/dashboard/dashboard.routes').then((m) => m.routes),
      },
      {
        path: 'monitors/new',
        providers: [provideTranslocoScope('monitorEditor')],
        loadChildren: () =>
          import('./features/monitor-editor/monitor-editor.routes').then((m) => m.createRoutes),
      },
      {
        path: 'monitors/:id/edit',
        providers: [provideTranslocoScope('monitorEditor')],
        loadChildren: () =>
          import('./features/monitor-editor/monitor-editor.routes').then((m) => m.editRoutes),
      },
      {
        path: 'monitors/:id',
        providers: [provideTranslocoScope('monitorDetail')],
        loadChildren: () =>
          import('./features/monitor-detail/monitor-detail.routes').then((m) => m.routes),
      },
      {
        path: 'incidents',
        providers: [provideTranslocoScope('incidents')],
        loadChildren: () => import('./features/incidents/incidents.routes').then((m) => m.routes),
      },
      {
        path: 'channels',
        providers: [provideTranslocoScope('channels')],
        loadChildren: () => import('./features/channels/channels.routes').then((m) => m.routes),
      },
      {
        path: 'maintenance',
        providers: [provideTranslocoScope('maintenance')],
        loadChildren: () =>
          import('./features/maintenance/maintenance.routes').then((m) => m.routes),
      },
      {
        path: 'account',
        providers: [provideTranslocoScope('account')],
        loadChildren: () => import('./features/account/account.routes').then((m) => m.routes),
      },
      {
        path: 'users',
        canActivate: [adminGuard],
        providers: [provideTranslocoScope('users')],
        loadChildren: () => import('./features/users/users.routes').then((m) => m.routes),
      },
    ],
  },
  { path: '**', redirectTo: '' },
];
