// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting } from '@angular/common/http/testing';
import { WritableSignal, signal } from '@angular/core';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { Subject, of } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { Monitor, MonitorStatus, StreamEvent } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { DashboardPage } from './dashboard-page';

function monitor(id: string, name: string, status: MonitorStatus): Monitor {
  return {
    id,
    name,
    status,
    type: 'http',
    config: { url: `https://${id}.example` },
    intervalSeconds: 60,
    statusSince: '2026-10-01T00:00:00Z',
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    uptime24h: null,
    lastLatencyMs: null,
  };
}

describe('DashboardPage', () => {
  let fixture: ComponentFixture<DashboardPage>;
  let events: Subject<StreamEvent>;
  let canEdit: WritableSignal<boolean>;
  let listMonitors: ReturnType<typeof vi.fn>;
  let pauseMonitor: ReturnType<typeof vi.fn>;
  let bulkMonitors: ReturnType<typeof vi.fn>;
  let dialogOpen: ReturnType<typeof vi.fn>;
  let snackOpen: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    events = new Subject<StreamEvent>();
    canEdit = signal(false);
    listMonitors = vi.fn();
    pauseMonitor = vi.fn();
    bulkMonitors = vi.fn(() => of({ results: [] }));
    // Confirms by default; a test can make the dialog resolve with false.
    dialogOpen = vi.fn(() => ({ afterClosed: () => of(true) }));
    snackOpen = vi.fn();
    TestBed.configureTestingModule({
      imports: [
        DashboardPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        { provide: AuthService, useValue: { canEdit } },
        {
          provide: LiveStreamService,
          useValue: { resync: signal(0), events: events.asObservable() },
        },
        {
          provide: MonitorsService,
          useValue: { listMonitors, pauseMonitor, bulkMonitors },
        },
        { provide: MatDialog, useValue: { open: dialogOpen } },
        { provide: MatSnackBar, useValue: { open: snackOpen } },
      ],
    });
  });

  function create(): void {
    fixture = TestBed.createComponent(DashboardPage);
    fixture.detectChanges();
  }

  /** Lets the async page load finish, then renders the result. */
  async function settle(): Promise<void> {
    await new Promise((resolve) => setTimeout(resolve));
    fixture.detectChanges();
  }

  function element(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  function text(): string {
    return element().textContent ?? '';
  }

  /** The button whose label is the given translation key (the testing loader echoes keys). */
  function button(key: string): HTMLButtonElement {
    const found = Array.from(element().querySelectorAll('button')).find(
      (b) => b.textContent?.trim() === key,
    );
    if (!found) {
      throw new Error(`no button ${key}`);
    }
    return found;
  }

  function rowCheckboxes(): HTMLInputElement[] {
    return Array.from(element().querySelectorAll<HTMLInputElement>('tbody input[type="checkbox"]'));
  }

  function check(box: HTMLInputElement): void {
    box.click();
    fixture.detectChanges();
  }

  it('follows the cursor until the last page and renders sorted rows', async () => {
    listMonitors
      .mockReturnValueOnce(
        of({ items: [monitor('a', 'Zeta', MonitorStatus.UP)], nextCursor: 'c2' }),
      )
      .mockReturnValueOnce(
        of({ items: [monitor('b', 'Alpha', MonitorStatus.DOWN)], nextCursor: null }),
      );
    create();
    await settle();

    expect(listMonitors).toHaveBeenNthCalledWith(2, expect.objectContaining({ cursor: 'c2' }));
    const names = Array.from(element().querySelectorAll('tbody .name')).map((el) =>
      el.textContent?.trim(),
    );
    expect(names).toEqual(['Alpha', 'Zeta']);
  });

  it('applies a state event to the rendered status', async () => {
    listMonitors.mockReturnValue(of({ items: [monitor('a', 'Zeta', MonitorStatus.UP)] }));
    create();
    await settle();

    events.next({
      type: 'state',
      ts: '2026-10-21T11:00:00Z',
      monitorId: 'a',
      status: MonitorStatus.DOWN,
    });
    fixture.detectChanges();

    expect(text()).toContain('status.DOWN');
  });

  it('hides edit actions without edit rights', async () => {
    listMonitors.mockReturnValue(of({ items: [monitor('a', 'Zeta', MonitorStatus.UP)] }));
    create();
    await settle();

    expect(element().querySelector('td.actions')).toBeNull();
    expect(element().querySelector('a[href="/monitors/new"]')).toBeNull();
  });

  it('shows no selection UI to viewers', async () => {
    listMonitors.mockReturnValue(
      of({
        items: [monitor('a', 'Alpha', MonitorStatus.UP), monitor('b', 'Beta', MonitorStatus.UP)],
      }),
    );
    create();
    await settle();

    expect(element().querySelector('input[type="checkbox"]')).toBeNull();
    expect(element().querySelector('[role="toolbar"]')).toBeNull();
  });

  describe('as an editor', () => {
    beforeEach(() => {
      canEdit.set(true);
      listMonitors.mockReturnValue(
        of({
          // Both UP, so the rows are ordered by name: row 0 is 'a', row 1 is 'b'.
          items: [monitor('a', 'Alpha', MonitorStatus.UP), monitor('b', 'Beta', MonitorStatus.UP)],
        }),
      );
    });

    it('labels each row checkbox with the monitor name and offers select all', async () => {
      create();
      await settle();

      expect(rowCheckboxes()).toHaveLength(2);
      const labels = Array.from(element().querySelectorAll('tbody input[type="checkbox"]')).map(
        (el) => el.getAttribute('aria-label'),
      );
      expect(labels.every((label) => label === 'dashboard.bulk.selectMonitor')).toBe(true);
      expect(text()).toContain('dashboard.bulk.selectAll');
      // no toolbar before anything is selected
      expect(element().querySelector('[role="toolbar"]')).toBeNull();
    });

    it('selects all visible monitors and shows the count and toolbar', async () => {
      create();
      await settle();

      check(element().querySelector<HTMLInputElement>('.bulk__all input')!);

      expect(rowCheckboxes().every((box) => box.checked)).toBe(true);
      expect(text()).toContain('dashboard.bulk.selectedCount');
      expect(element().querySelector('[role="toolbar"]')).not.toBeNull();
    });

    it('pauses the selected IDs in one bulk call with an idempotency key', async () => {
      create();
      await settle();

      check(rowCheckboxes()[0]);
      button('dashboard.bulk.pause').click();
      await settle();

      expect(bulkMonitors).toHaveBeenCalledWith(
        { action: 'pause', ids: ['a'] },
        { headers: { 'Idempotency-Key': expect.stringMatching(/^[0-9a-f-]{36}$/) } },
      );
      expect(snackOpen).toHaveBeenCalled();
    });

    it('asks before deleting and sends nothing when the user declines', async () => {
      dialogOpen.mockReturnValueOnce({ afterClosed: () => of(false) });
      create();
      await settle();

      check(rowCheckboxes()[0]);
      check(rowCheckboxes()[1]);
      button('dashboard.bulk.delete').click();
      await settle();

      expect(dialogOpen).toHaveBeenCalledWith(
        expect.anything(),
        expect.objectContaining({ data: { count: 2 } }),
      );
      expect(bulkMonitors).not.toHaveBeenCalled();
    });

    it('deletes after confirmation', async () => {
      create();
      await settle();

      check(rowCheckboxes()[1]);
      button('dashboard.bulk.delete').click();
      await settle();

      expect(bulkMonitors).toHaveBeenCalledWith(
        { action: 'delete', ids: ['b'] },
        expect.anything(),
      );
    });

    it('rejects invalid tags without calling the API', async () => {
      create();
      await settle();

      check(rowCheckboxes()[0]);
      const input = element().querySelector<HTMLInputElement>('.bulk__tags')!;
      input.value = ' , ';
      input.dispatchEvent(new Event('input'));
      button('dashboard.bulk.addTags').click();
      fixture.detectChanges();

      expect(bulkMonitors).not.toHaveBeenCalled();
      expect(text()).toContain('dashboard.bulk.tags.empty');
    });

    it('adds trimmed, de-duplicated tags', async () => {
      create();
      await settle();

      check(rowCheckboxes()[0]);
      const input = element().querySelector<HTMLInputElement>('.bulk__tags')!;
      input.value = ' prod, eu-west, prod';
      input.dispatchEvent(new Event('input'));
      button('dashboard.bulk.addTags').click();
      await settle();

      expect(bulkMonitors).toHaveBeenCalledWith(
        { action: 'addTags', ids: ['a'], tags: ['prod', 'eu-west'] },
        expect.anything(),
      );
    });

    it('disables the actions and shows a hint above 500 selected monitors', async () => {
      const many = Array.from({ length: 501 }, (_, i) =>
        monitor(`m${i}`, `Monitor ${i}`, MonitorStatus.UP),
      );
      listMonitors.mockReturnValue(of({ items: many }));
      create();
      await settle();

      check(element().querySelector<HTMLInputElement>('.bulk__all input')!);

      expect(button('dashboard.bulk.pause').disabled).toBe(true);
      expect(button('dashboard.bulk.delete').disabled).toBe(true);
      expect(text()).toContain('dashboard.bulk.tooMany');
    });
  });
});
