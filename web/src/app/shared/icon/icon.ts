// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { ICONS, IconName } from './icons';

/**
 * Inline SVG icon from the bundled Material Symbols paths. The glyph takes the
 * current text colour. It is decorative: pair it with a visible label or an
 * aria-label on the enclosing control.
 *
 * @example <app-icon name="dashboard" size="1.25rem" />
 */
@Component({
  selector: 'app-icon',
  template: `<svg viewBox="0 -960 960 960" focusable="false" aria-hidden="true">
    <path [attr.d]="path()" />
  </svg>`,
  styles: `
    :host {
      display: inline-flex;
      flex: none;
      width: var(--icon-size);
      height: var(--icon-size);
      line-height: 0;
      vertical-align: middle;
    }
    svg {
      display: block;
      width: 100%;
      height: 100%;
      fill: currentColor;
    }
  `,
  host: {
    'aria-hidden': 'true',
    '[style.--icon-size]': 'size()',
  },
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class Icon {
  /** Name of an entry in ICONS, e.g. `monitorHeart` for the Material icon monitor_heart. */
  readonly name = input.required<IconName>();
  /** Any CSS length. */
  readonly size = input<string>('1.5em');

  protected readonly path = computed(() => ICONS[this.name()]);
}
