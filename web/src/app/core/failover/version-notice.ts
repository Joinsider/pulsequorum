// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { Injectable, InjectionToken, effect, inject } from '@angular/core';
import { MatSnackBar } from '@angular/material/snack-bar';
import { TranslocoService } from '@jsverse/transloco';
import { take } from 'rxjs';
import { NodeSelector } from './node-selector.service';
import { SPA_VERSION, versionMismatch } from './version-check';

/** Session storage key: the notice is shown at most once per browser tab session. */
export const VERSION_NOTICE_KEY = 'pulsequorum.versionNoticeShown';

/** Version of this frontend; tests replace it through this token. */
export const APP_VERSION = new InjectionToken<string>('APP_VERSION', {
  providedIn: 'root',
  factory: () => SPA_VERSION,
});

/**
 * Tells the user once per session when the connected node runs a different
 * major or minor version than this frontend (ADR 0036). The notice stays until
 * dismissed.
 */
@Injectable({ providedIn: 'root' })
export class VersionNotice {
  private readonly selector = inject(NodeSelector);
  private readonly snackBar = inject(MatSnackBar);
  private readonly transloco = inject(TranslocoService);
  private readonly appVersion = inject(APP_VERSION);
  private shown = false;

  constructor() {
    effect(() => {
      const node = this.selector.nodeVersion();
      if (!this.shown && versionMismatch(this.appVersion, node) && !wasShown()) {
        this.shown = true;
        rememberShown();
        this.show(node);
      }
    });
  }

  private show(node: string): void {
    this.transloco
      .selectTranslate('failover.versionMismatch', { spa: this.appVersion, node })
      .pipe(take(1))
      .subscribe((message) => {
        this.snackBar.open(message, this.transloco.translate('failover.dismiss'), {
          duration: 0,
        });
      });
  }
}

/** Session storage can be blocked; then the notice may show again on the next change. */
function wasShown(): boolean {
  try {
    return sessionStorage.getItem(VERSION_NOTICE_KEY) === '1';
  } catch {
    return false;
  }
}

function rememberShown(): void {
  try {
    sessionStorage.setItem(VERSION_NOTICE_KEY, '1');
  } catch {
    // Storage unavailable: the in-memory flag still limits the notice to one per instance.
  }
}
