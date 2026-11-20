// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { inject, Injectable } from '@angular/core';
import { Observable } from 'rxjs';
import { SystemAdminService } from '../../api/gen/endpoints/system-admin/system-admin.service';
import { SystemAlert } from '../../api/gen/model';

/** Reads the system alerts of the platform (FA-OBS-01, ADR 0025). Platform admins only. */
@Injectable({ providedIn: 'root' })
export class SystemAlertsService {
  private readonly api = inject(SystemAdminService);

  /** The alerts that are active now, in the order the server lists them (kind, subject). */
  active(): Observable<SystemAlert[]> {
    return this.api.listSystemAlerts({ active: true });
  }
}
