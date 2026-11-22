// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { FormArray } from '@angular/forms';
import { ActivatedRoute, Router, convertToParamMap } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { Observable, of, throwError } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { NotificationsService } from '../../api/gen/endpoints/notifications/notifications.service';
import { AssignableNode, Monitor, MonitorType, NotificationChannel } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { MonitorEditorPage } from './monitor-editor-page';

const HTTP_TYPE: MonitorType = {
  type: 'http',
  name: 'HTTP(S)',
  configSchema: {
    type: 'object',
    additionalProperties: false,
    required: ['url'],
    properties: {
      url: { type: 'string', title: 'URL', format: 'uri' },
      method: { type: 'string', enum: ['GET', 'POST'], default: 'GET' },
    },
  },
};

const TCP_TYPE: MonitorType = {
  type: 'tcp',
  name: 'TCP',
  requiresCapability: 'icmp',
  configSchema: {
    type: 'object',
    required: ['host', 'port'],
    properties: {
      host: { type: 'string' },
      port: { type: 'integer', minimum: 1, maximum: 65535 },
    },
  },
};

/** HTTP type after ADR 0015: header values and the basic-auth password are secrets. */
const HTTP_SECRET_TYPE: MonitorType = {
  type: 'http',
  name: 'HTTP(S)',
  configSchema: {
    type: 'object',
    additionalProperties: false,
    required: ['url'],
    properties: {
      url: { type: 'string', title: 'URL', format: 'uri' },
      method: { type: 'string', enum: ['GET', 'POST'], default: 'GET' },
      headers: {
        type: 'object',
        additionalProperties: { type: 'string', writeOnly: true },
      },
      auth: {
        type: 'object',
        additionalProperties: false,
        required: ['type'],
        properties: {
          type: { type: 'string', enum: ['basic', 'bearer'] },
          username: { type: 'string' },
          password: { type: 'string', writeOnly: true },
        },
      },
    },
  },
};

const CHANNELS: NotificationChannel[] = [
  {
    id: 'c1',
    name: 'Ops',
    kind: 'webhook',
    events: ['down'],
    isDefault: true,
    state: 'ok',
    createdAt: '2026-10-01T10:00:00Z',
  },
  {
    id: 'c2',
    name: 'Mail',
    kind: 'shoutrrr',
    events: ['down', 'up'],
    isDefault: false,
    state: 'ok',
    createdAt: '2026-10-01T10:00:00Z',
  },
];

const MONITOR: Monitor = {
  id: '6f1c7a2e-0000-4000-8000-000000000001',
  name: 'Shop',
  type: 'http',
  config: { url: 'https://shop.example', method: 'POST' },
  intervalSeconds: 60,
  retries: 1,
  recoveryThreshold: 1,
  resendIntervalMinutes: 0,
  notificationChannelIds: ['c2'],
  paused: false,
  status: 'UP',
  statusSince: '2026-10-01T10:00:00Z',
  createdAt: '2026-10-01T10:00:00Z',
  updatedAt: '2026-10-01T10:00:00Z',
};

/** A stored monitor whose secrets come back masked, as the API returns them. */
const SECRET_MONITOR: Monitor = {
  ...MONITOR,
  config: {
    url: 'https://shop.example',
    headers: { 'X-Api-Key': '***' },
    auth: { type: 'basic', username: 'bob', password: '***' },
  },
};

const ASSIGNABLE_NODES: AssignableNode[] = [
  { id: 'v1', name: 'voter-eu', role: 'voter', private: false, healthy: true, capabilities: [] },
  {
    id: 'p-priv',
    name: 'edge-corp',
    role: 'probe',
    private: true,
    healthy: true,
    capabilities: ['http'],
    region: 'dc-1',
  },
];

interface Setup {
  fixture: ComponentFixture<MonitorEditorPage>;
  monitors: {
    listMonitorTypes: ReturnType<typeof vi.fn>;
    listAssignableNodes: ReturnType<typeof vi.fn>;
    getMonitor: ReturnType<typeof vi.fn>;
    createMonitor: ReturnType<typeof vi.fn>;
    updateMonitor: ReturnType<typeof vi.fn>;
  };
  router: { navigate: ReturnType<typeof vi.fn> };
}

function setup(options: {
  mode: 'create' | 'edit';
  create?: () => Observable<Monitor>;
  update?: () => Observable<Monitor>;
  types?: MonitorType[];
  monitor?: Monitor;
  nodes?: AssignableNode[];
}): Setup {
  const monitors = {
    listMonitorTypes: vi.fn(() => of(options.types ?? [HTTP_TYPE, TCP_TYPE])),
    listAssignableNodes: vi.fn(() => of(options.nodes ?? ASSIGNABLE_NODES)),
    getMonitor: vi.fn(() => of(options.monitor ?? MONITOR)),
    createMonitor: vi.fn(options.create ?? (() => of({ ...MONITOR, id: 'new-id' }))),
    updateMonitor: vi.fn(options.update ?? (() => of(MONITOR))),
  };
  const router = { navigate: vi.fn(() => Promise.resolve(true)) };
  const route = {
    snapshot: {
      data: { mode: options.mode },
      paramMap: convertToParamMap(options.mode === 'edit' ? { id: MONITOR.id } : {}),
      parent: null,
    },
  };

  TestBed.configureTestingModule({
    imports: [
      MonitorEditorPage,
      TranslocoTestingModule.forRoot({
        langs: { en: {} },
        translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
      }),
    ],
    providers: [
      { provide: MonitorsService, useValue: monitors },
      {
        provide: NotificationsService,
        useValue: { listNotificationChannels: () => of(CHANNELS) },
      },
      { provide: AuthService, useValue: { canEdit: signal(true).asReadonly() } },
      { provide: ActivatedRoute, useValue: route },
      { provide: Router, useValue: router },
    ],
  });
  const fixture = TestBed.createComponent(MonitorEditorPage);
  fixture.detectChanges();
  return { fixture, monitors, router };
}

/** The component instance, typed loosely for the protected members under test. */
function page(fixture: ComponentFixture<MonitorEditorPage>): Record<string, any> {
  return fixture.componentInstance as unknown as Record<string, any>;
}

describe('MonitorEditorPage', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('creates a monitor with the default type, the channel defaults and an idempotency key', async () => {
    const { fixture, monitors, router } = setup({ mode: 'create' });
    const p = page(fixture);

    expect(p['form'].controls['type'].value).toBe('http');
    expect(p['form'].controls['notificationChannelIds'].value).toEqual(['c1']);

    p['form'].controls['name'].setValue('  Shop  ');
    p['form'].get('config').get('url').setValue('https://shop.example');
    await p['submit']();

    expect(monitors.createMonitor).toHaveBeenCalledOnce();
    const [input, options] = monitors.createMonitor.mock.calls[0] as [
      Record<string, unknown>,
      { headers: Record<string, string> },
    ];
    expect(input['name']).toBe('Shop');
    expect(input['type']).toBe('http');
    expect(input['config']).toEqual({ url: 'https://shop.example', method: 'GET' });
    expect(input['notificationChannelIds']).toEqual(['c1']);
    expect(options.headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/);
    expect(router.navigate).toHaveBeenCalledWith(['/monitors', 'new-id']);
  });

  it('renders the type-specific fields from the schema of the selected type', () => {
    const { fixture } = setup({ mode: 'create' });
    const root = fixture.nativeElement as HTMLElement;

    expect(root.querySelector('input[type="url"]')).not.toBeNull();
    expect(root.querySelector('mat-select[formcontrolname="type"]')).not.toBeNull();

    page(fixture)['selectType']('tcp');
    fixture.detectChanges();
    expect(root.querySelector('input[type="url"]')).toBeNull();
    expect(root.querySelector('input[type="number"][max="65535"]')).not.toBeNull();
  });

  it('reuses the idempotency key for a retry of the same payload and renews it after a change', async () => {
    const failing = throwError(() => new HttpErrorResponse({ status: 0 }));
    const { fixture, monitors } = setup({
      mode: 'create',
      create: () => failing as Observable<Monitor>,
    });
    const p = page(fixture);
    p['form'].controls['name'].setValue('Shop');
    p['form'].get('config').get('url').setValue('https://shop.example');

    await p['submit']();
    await p['submit']();
    const keyOf = (i: number): string =>
      (monitors.createMonitor.mock.calls[i] as [unknown, { headers: Record<string, string> }])[1]
        .headers['Idempotency-Key'];
    expect(keyOf(1)).toBe(keyOf(0));

    p['form'].controls['name'].setValue('Shop 2');
    await p['submit']();
    expect(keyOf(2)).not.toBe(keyOf(0));
  });

  it('maps 422 field errors onto the config controls and lists unknown fields', async () => {
    const rejected = throwError(
      () =>
        new HttpErrorResponse({
          status: 422,
          error: {
            type: 'about:blank',
            title: 'Invalid',
            status: 422,
            errors: [
              { field: '/config/url', message: 'must be https' },
              { field: '/unknown', message: 'no such field' },
            ],
          },
        }),
    );
    const { fixture, router } = setup({
      mode: 'create',
      create: () => rejected as Observable<Monitor>,
    });
    const p = page(fixture);
    p['form'].controls['name'].setValue('Shop');
    p['form'].get('config').get('url').setValue('http://shop.example');

    await p['submit']();

    expect(p['form'].get('config').get('url').errors).toEqual({ server: 'must be https' });
    expect(p['summary']()).toEqual({
      key: 'monitorEditor.errors.validation',
      messages: ['no such field'],
    });
    expect(router.navigate).not.toHaveBeenCalled();
  });

  it('shows the problem detail of a refused create, e.g. a reached plan limit', async () => {
    const refused = throwError(
      () =>
        new HttpErrorResponse({
          status: 403,
          error: {
            type: 'about:blank',
            title: 'Forbidden',
            status: 403,
            detail: "monitor limit reached: plan 'starter' allows 10 monitors",
          },
        }),
    );
    const { fixture, router } = setup({
      mode: 'create',
      create: () => refused as Observable<Monitor>,
    });
    const p = page(fixture);
    p['form'].controls['name'].setValue('Shop');
    p['form'].get('config').get('url').setValue('https://shop.example');

    await p['submit']();

    expect(p['summary']()).toEqual({
      key: 'monitorEditor.errors.forbidden',
      messages: ["monitor limit reached: plan 'starter' allows 10 monitors"],
    });
    expect(router.navigate).not.toHaveBeenCalled();
  });

  it('does not send an invalid form and marks the missing fields', async () => {
    const { fixture, monitors } = setup({ mode: 'create' });
    const p = page(fixture);

    await p['submit']();

    expect(monitors.createMonitor).not.toHaveBeenCalled();
    expect(p['form'].controls['name'].touched).toBe(true);
    expect(p['summary']()?.key).toBe('monitorEditor.errors.invalidForm');
  });

  it('resets the config to the defaults of the newly selected type', () => {
    const { fixture } = setup({ mode: 'create' });
    const p = page(fixture);

    p['selectType']('tcp');

    expect(p['form'].get('config').get('host')).not.toBeNull();
    expect(p['form'].get('config').get('url')).toBeNull();
    expect(p['capability']()).toBe('icmp');
  });

  it('loads the monitor by the parent route id and updates it with the current channel selection', async () => {
    const { fixture, monitors, router } = setup({ mode: 'edit' });
    const p = page(fixture);

    expect(monitors.getMonitor).toHaveBeenCalledWith(MONITOR.id);
    expect(p['form'].controls['name'].value).toBe('Shop');
    expect(p['form'].controls['notificationChannelIds'].value).toEqual(['c2']);
    expect(p['form'].get('config').get('method').value).toBe('POST');

    p['form'].controls['notificationChannelIds'].setValue([]);
    await p['submit']();

    expect(monitors.createMonitor).not.toHaveBeenCalled();
    expect(monitors.updateMonitor).toHaveBeenCalledOnce();
    const [id, input] = monitors.updateMonitor.mock.calls[0] as [string, Record<string, unknown>];
    expect(id).toBe(MONITOR.id);
    expect(input['notificationChannelIds']).toEqual([]);
    expect(router.navigate).toHaveBeenCalledWith(['/monitors', MONITOR.id]);
  });

  it('shows the error state with a retry when the monitor cannot be loaded', () => {
    TestBed.configureTestingModule({
      imports: [
        MonitorEditorPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        {
          provide: MonitorsService,
          useValue: {
            listMonitorTypes: () => throwError(() => new HttpErrorResponse({ status: 500 })),
            listAssignableNodes: () => of([]),
          },
        },
        { provide: NotificationsService, useValue: { listNotificationChannels: () => of([]) } },
        { provide: AuthService, useValue: { canEdit: signal(true).asReadonly() } },
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: { data: { mode: 'create' }, paramMap: convertToParamMap({}), parent: null },
          },
        },
        { provide: Router, useValue: { navigate: vi.fn() } },
      ],
    });
    const fixture = TestBed.createComponent(MonitorEditorPage);
    fixture.detectChanges();

    expect(page(fixture)['loadState']()).toBe('error');
    const text = (fixture.nativeElement as HTMLElement).textContent ?? '';
    expect(text).toContain('monitorEditor.errors.load');
  });

  describe('stored secrets (ADR 0015)', () => {
    /** The config part of the body of the last PUT. */
    function sentConfig(monitors: Setup['monitors']): Record<string, any> {
      const [, input] = monitors.updateMonitor.mock.calls[0] as [
        string,
        { config: Record<string, any> },
      ];
      return input.config;
    }

    /** Clicks the button with the given (translation) text inside a scope. */
    function clickIn(scope: Element, text: string): void {
      const button = Array.from(scope.querySelectorAll('button')).find(
        (b) => b.textContent?.trim() === text,
      );
      if (!button) {
        throw new Error(`no button "${text}"`);
      }
      button.click();
    }

    it('sends the masks back unchanged when the stored secrets are not touched', async () => {
      const { fixture, monitors } = setup({
        mode: 'edit',
        types: [HTTP_SECRET_TYPE, TCP_TYPE],
        monitor: SECRET_MONITOR,
      });

      await page(fixture)['submit']();

      expect(monitors.updateMonitor).toHaveBeenCalledOnce();
      expect(sentConfig(monitors)).toMatchObject({
        headers: { 'X-Api-Key': '***' },
        auth: { type: 'basic', username: 'bob', password: '***' },
      });
    });

    it('shows stored secrets as hidden and sends only the replaced one', async () => {
      const { fixture, monitors } = setup({
        mode: 'edit',
        types: [HTTP_SECRET_TYPE, TCP_TYPE],
        monitor: SECRET_MONITOR,
      });
      const root = fixture.nativeElement as HTMLElement;

      expect(root.textContent).toContain('monitorEditor.secret.stored');
      expect(root.textContent).not.toContain('***');
      expect(root.querySelector('input[value="***"]')).toBeNull();

      const password = root.querySelector('div.secret[aria-label="Password"]');
      expect(password).not.toBeNull();
      clickIn(password as Element, 'monitorEditor.secret.replace');
      fixture.detectChanges();

      const input = root.querySelector('input[type="password"]') as HTMLInputElement;
      expect(input.value).toBe('');
      input.value = 'new-pass';
      input.dispatchEvent(new Event('input'));
      fixture.detectChanges();

      await page(fixture)['submit']();

      expect(sentConfig(monitors)).toMatchObject({
        headers: { 'X-Api-Key': '***' },
        auth: { type: 'basic', username: 'bob', password: 'new-pass' },
      });
    });

    it('keeps the key of a stored header read-only', () => {
      const { fixture } = setup({
        mode: 'edit',
        types: [HTTP_SECRET_TYPE, TCP_TYPE],
        monitor: SECRET_MONITOR,
      });
      const keyInput = (fixture.nativeElement as HTMLElement).querySelector(
        'input[readonly]',
      ) as HTMLInputElement | null;

      expect(keyInput?.value).toBe('X-Api-Key');
    });

    it('drops the stored secrets when the type changes', () => {
      const { fixture } = setup({
        mode: 'edit',
        types: [HTTP_SECRET_TYPE, TCP_TYPE],
        monitor: SECRET_MONITOR,
      });
      const p = page(fixture);

      p['selectType']('tcp');
      p['selectType']('http');
      fixture.detectChanges();

      const config = p['form'].get('config');
      expect(JSON.stringify(config.getRawValue())).not.toContain('***');
      expect((config.get('headers') as FormArray).length).toBe(0);
      expect(config.get('auth')?.disabled).toBe(true);
    });
  });

  describe('assignable nodes picker', () => {
    it('loads assignable nodes and shows internal-targets hint only for private probes', () => {
      const { fixture } = setup({ mode: 'create' });
      const p = page(fixture);

      p['form'].controls['assignmentMode'].setValue('nodes');
      p['form'].controls['assignmentNodes'].setValue(['v1']);
      fixture.detectChanges();
      expect(
        fixture.nativeElement.querySelector('[data-testid="private-probe-ssrf-hint"]'),
      ).toBeNull();

      p['form'].controls['assignmentNodes'].setValue(['p-priv']);
      fixture.detectChanges();
      expect(
        fixture.nativeElement.querySelector('[data-testid="private-probe-ssrf-hint"]'),
      ).not.toBeNull();

      p['form'].controls['assignmentNodes'].setValue(['p-priv', 'v1']);
      fixture.detectChanges();
      expect(
        fixture.nativeElement.querySelector('[data-testid="private-probe-ssrf-hint"]'),
      ).toBeNull();
    });

    it('preserves unknown saved node ids when editing an existing monitor', () => {
      const { fixture } = setup({
        mode: 'edit',
        monitor: { ...MONITOR, assignment: { mode: 'nodes', nodes: ['deleted-node'] } },
      });
      const p = page(fixture);
      expect(p['unknownSelectedNodes']()).toEqual(['deleted-node']);
    });
  });
});
