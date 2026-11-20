// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, DestroyRef, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { SystemService } from '../../api/gen/endpoints/system/system.service';
import { Health } from '../../api/gen/model';

type LoadState = 'loading' | 'ok' | 'error';

@Component({
  selector: 'app-status-page',
  templateUrl: './status-page.html',
  styleUrl: './status-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class StatusPage {
  private readonly system = inject(SystemService);
  private readonly destroyRef = inject(DestroyRef);

  protected readonly state = signal<LoadState>('loading');
  protected readonly health = signal<Health | null>(null);

  constructor() {
    this.system
      .getHealth()
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (health) => {
          this.health.set(health);
          this.state.set('ok');
        },
        error: () => {
          this.health.set(null);
          this.state.set('error');
        },
      });
  }
}
