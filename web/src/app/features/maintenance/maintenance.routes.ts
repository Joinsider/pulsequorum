// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { inject } from '@angular/core';
import { CanActivateFn, Router, Routes } from '@angular/router';
import { AuthService } from '../../core/auth/auth.service';
import { MaintenanceEditor } from './maintenance-editor';
import { MaintenanceList } from './maintenance-list';

/** Only roles that may change windows reach the editor (convenience; the server enforces it). */
const editGuard: CanActivateFn = async () => {
  const auth = inject(AuthService);
  await auth.restore();
  return auth.canEdit() ? true : inject(Router).createUrlTree(['/maintenance']);
};

export const routes: Routes = [
  { path: '', component: MaintenanceList },
  { path: 'new', canActivate: [editGuard], component: MaintenanceEditor },
  { path: ':maintenanceId', canActivate: [editGuard], component: MaintenanceEditor },
];
