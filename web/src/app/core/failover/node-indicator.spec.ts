// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { NodeSelector } from './node-selector.service';
import { NodeIndicator, SWITCH_INDICATOR_MS } from './node-indicator';

const A = 'https://a.example';
const B = 'https://b.example';

describe('NodeIndicator', () => {
  let nodes: ReturnType<typeof signal<string[]>>;
  let current: ReturnType<typeof signal<string>>;
  let switchedAt: ReturnType<typeof signal<number | null>>;
  let fixture: ComponentFixture<NodeIndicator>;

  beforeEach(() => {
    vi.useFakeTimers();
    nodes = signal<string[]>([A, B]);
    current = signal(A);
    switchedAt = signal<number | null>(null);
    TestBed.configureTestingModule({
      imports: [
        NodeIndicator,
        TranslocoTestingModule.forRoot({
          langs: {
            en: {
              failover: {
                connectedTo: 'Connected to {{ host }}',
                switchedTo: 'Switched to {{ host }} after a node failure',
              },
            },
          },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        {
          provide: NodeSelector,
          useValue: {
            nodes: nodes.asReadonly(),
            current: current.asReadonly(),
            switchedAt: switchedAt.asReadonly(),
          },
        },
      ],
    });
    fixture = TestBed.createComponent(NodeIndicator);
    fixture.detectChanges();
  });

  afterEach(() => {
    TestBed.resetTestingModule();
    vi.useRealTimers();
  });

  function status(): HTMLElement | null {
    return fixture.nativeElement.querySelector('[role="status"]');
  }

  function refresh(): void {
    fixture.detectChanges();
  }

  it('is hidden with a single node', () => {
    nodes.set([A]);
    refresh();

    expect(status()).toBeNull();
  });

  it('shows the connected host with more than one node', () => {
    refresh();

    expect(status()).not.toBeNull();
    expect(status()!.getAttribute('aria-live')).toBe('polite');
    expect(status()!.getAttribute('aria-label')).toBe('Connected to a.example');
    expect(status()!.querySelector('.visually-hidden')).toBeNull();
  });

  it('shows the switched state and text for 60 s after a switch', () => {
    refresh();

    current.set(B);
    switchedAt.set(Date.now());
    refresh();

    expect(status()!.classList).toContain('node-indicator--switched');
    expect(status()!.getAttribute('aria-label')).toBe('Switched to b.example after a node failure');
    expect(status()!.querySelector('.visually-hidden')!.textContent).toContain('b.example');

    vi.advanceTimersByTime(SWITCH_INDICATOR_MS - 1);
    refresh();
    expect(status()!.classList).toContain('node-indicator--switched');

    vi.advanceTimersByTime(1);
    refresh();
    expect(status()!.classList).not.toContain('node-indicator--switched');
    expect(status()!.getAttribute('aria-label')).toBe('Connected to b.example');
  });
});
