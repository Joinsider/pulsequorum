// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { TranslocoDirective } from '@jsverse/transloco';
import { durationParts } from './incident.logic';

/** Renders a duration localised, e.g. "2 h 5 min". */
@Component({
  selector: 'app-incident-duration',
  imports: [TranslocoDirective],
  template: `
    <ng-container *transloco="let t">
      @for (part of parts(); track part.unit; let first = $first) {
        @if (!first) {
          <span> </span>
        }
        {{ t('incidents.units.' + part.unit, { value: part.value }) }}
      }
    </ng-container>
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class IncidentDuration {
  readonly ms = input.required<number>();

  protected readonly parts = computed(() => durationParts(this.ms()));
}
