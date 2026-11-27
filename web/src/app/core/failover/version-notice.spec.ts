// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { MatSnackBar } from '@angular/material/snack-bar';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { NodeSelector } from './node-selector.service';
import { APP_VERSION, VERSION_NOTICE_KEY, VersionNotice } from './version-notice';

describe('VersionNotice', () => {
  let nodeVersion: ReturnType<typeof signal<string>>;
  let open: ReturnType<typeof vi.fn>;

  function create(appVersion: string): VersionNotice {
    TestBed.configureTestingModule({
      imports: [
        TranslocoTestingModule.forRoot({
          langs: {
            en: {
              failover: {
                versionMismatch: 'Frontend {{ spa }}, node {{ node }}',
                dismiss: 'Dismiss',
              },
            },
          },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        { provide: NodeSelector, useValue: { nodeVersion: nodeVersion.asReadonly() } },
        { provide: MatSnackBar, useValue: { open } },
        { provide: APP_VERSION, useValue: appVersion },
      ],
    });
    return TestBed.inject(VersionNotice);
  }

  beforeEach(() => {
    nodeVersion = signal('');
    open = vi.fn();
    sessionStorage.removeItem(VERSION_NOTICE_KEY);
  });

  afterEach(() => {
    TestBed.resetTestingModule();
    sessionStorage.removeItem(VERSION_NOTICE_KEY);
  });

  it('shows a dismissable notice when the node has another minor version', () => {
    create('1.2.0');
    nodeVersion.set('1.3.1');
    TestBed.flushEffects();

    expect(open).toHaveBeenCalledTimes(1);
    expect(open).toHaveBeenCalledWith('Frontend 1.2.0, node 1.3.1', 'Dismiss', { duration: 0 });
  });

  it('stays silent for the same major and minor version', () => {
    create('1.2.0');
    nodeVersion.set('1.2.9');
    TestBed.flushEffects();

    expect(open).not.toHaveBeenCalled();
  });

  it('shows the notice only once per session', () => {
    create('1.2.0');
    nodeVersion.set('2.0.0');
    TestBed.flushEffects();
    nodeVersion.set('3.0.0');
    TestBed.flushEffects();

    expect(open).toHaveBeenCalledTimes(1);
  });

  it('does not show again in a later instance of the same session', () => {
    sessionStorage.setItem(VERSION_NOTICE_KEY, '1');
    create('1.2.0');
    nodeVersion.set('2.0.0');
    TestBed.flushEffects();

    expect(open).not.toHaveBeenCalled();
  });
});
