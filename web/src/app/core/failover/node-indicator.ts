// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  computed,
  effect,
  inject,
  signal,
} from '@angular/core';
import { MatTooltipModule } from '@angular/material/tooltip';
import { TranslocoPipe } from '@jsverse/transloco';
import { Icon } from '../../shared/icon/icon';
import { NodeSelector } from './node-selector.service';

/** How long the "switched" state stays visible after an automatic node switch (ADR 0036). */
export const SWITCH_INDICATOR_MS = 60_000;

/**
 * Small status symbol in the header: which node the SPA talks to, and for one
 * minute after a failover a different icon. Shown only with more than one node.
 * The icon changes as well as the colour, so the state does not depend on colour.
 */
@Component({
  selector: 'app-node-indicator',
  imports: [Icon, MatTooltipModule, TranslocoPipe],
  template: `
    @if (visible()) {
      <span
        class="node-indicator"
        role="status"
        aria-live="polite"
        [class.node-indicator--switched]="switched()"
        [matTooltip]="
          (switched() ? 'failover.switchedTo' : 'failover.connectedTo')
            | transloco: { host: host() }
        "
        [attr.aria-label]="
          (switched() ? 'failover.switchedTo' : 'failover.connectedTo')
            | transloco: { host: host() }
        "
      >
        <app-icon [name]="switched() ? 'swapHoriz' : 'dns'" />
        @if (switched()) {
          <span class="visually-hidden">{{
            'failover.switchedTo' | transloco: { host: host() }
          }}</span>
        }
      </span>
    }
  `,
  styles: `
    :host {
      display: inline-flex;
    }
    .node-indicator {
      display: inline-flex;
      align-items: center;
      padding: 0.25rem;
      border-radius: 999px;
    }
    .node-indicator--switched {
      background: color-mix(in srgb, currentColor 22%, transparent);
    }
    .visually-hidden {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
      white-space: nowrap;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class NodeIndicator {
  private readonly selector = inject(NodeSelector);

  /** Only meaningful with a choice of nodes. */
  protected readonly visible = computed(() => this.selector.nodes().length > 1);

  /** Host of the current node, e.g. `mon1.example.com:8443`. */
  protected readonly host = computed(() => hostOf(this.selector.current()));

  /** True for {@link SWITCH_INDICATOR_MS} after a switch. */
  protected readonly switched = signal(false);

  constructor() {
    let timer: ReturnType<typeof setTimeout> | undefined;
    effect(() => {
      const switchedAt = this.selector.switchedAt();
      clearTimeout(timer);
      const remaining = switchedAt === null ? 0 : switchedAt + SWITCH_INDICATOR_MS - Date.now();
      this.switched.set(remaining > 0);
      if (remaining > 0) {
        timer = setTimeout(() => this.switched.set(false), remaining);
      }
    });
    inject(DestroyRef).onDestroy(() => clearTimeout(timer));
  }
}

function hostOf(origin: string): string {
  if (origin === '') {
    return '';
  }
  try {
    return new URL(origin).host;
  } catch {
    return origin;
  }
}
