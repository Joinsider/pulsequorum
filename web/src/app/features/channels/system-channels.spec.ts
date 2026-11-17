// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { of } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { NotificationsService } from '../../api/gen/endpoints/notifications/notifications.service';
import { SystemAdminService } from '../../api/gen/endpoints/system-admin/system-admin.service';
import { NotificationChannel, NotificationChannelInput } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { ChannelApi, channelListPath } from './channel-api';
import { ChannelEditorPage } from './channel-editor-page';
import { ChannelsPage } from './channels-page';

const SYSTEM: NotificationChannel = {
  id: '6f1c7a2e-0000-4000-8000-000000000011',
  name: 'Ops mail',
  kind: 'shoutrrr',
  maskedUrl: 'gotify://***@gotify.local',
  events: ['system'],
  isDefault: false,
  state: 'ok',
  createdAt: '2026-10-01T10:00:00Z',
};

type Calls = Record<string, ReturnType<typeof vi.fn>>;

/** Mocks of both generated clients; each call answers with the system channel. */
function mocks(): { tenant: Calls; system: Calls } {
  return {
    tenant: {
      listNotificationChannels: vi.fn(() => of([])),
      getNotificationChannel: vi.fn(() => of(SYSTEM)),
      createNotificationChannel: vi.fn(() => of(SYSTEM)),
      updateNotificationChannel: vi.fn(() => of(SYSTEM)),
      deleteNotificationChannel: vi.fn(() => of(undefined)),
      testNotificationChannel: vi.fn(() => of({ delivered: true })),
    },
    system: {
      listSystemChannels: vi.fn(() => of([SYSTEM])),
      getSystemChannel: vi.fn(() => of(SYSTEM)),
      createSystemChannel: vi.fn(() => of(SYSTEM)),
      updateSystemChannel: vi.fn(() => of(SYSTEM)),
      deleteSystemChannel: vi.fn(() => of(undefined)),
      testSystemChannel: vi.fn(() => of({ delivered: true })),
    },
  };
}

function configure(m: { tenant: Calls; system: Calls }): void {
  TestBed.configureTestingModule({
    imports: [
      ChannelsPage,
      ChannelEditorPage,
      TranslocoTestingModule.forRoot({
        langs: { en: {} },
        translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
      }),
    ],
    providers: [
      provideRouter([{ path: '**', children: [] }]),
      { provide: NotificationsService, useValue: m.tenant },
      { provide: SystemAdminService, useValue: m.system },
      { provide: AuthService, useValue: { canAdmin: signal(true).asReadonly() } },
    ],
  });
}

function lastInput(mock: ReturnType<typeof vi.fn>): NotificationChannelInput {
  return mock.mock.calls.at(-1)?.at(-1) as NotificationChannelInput;
}

describe('ChannelApi and channel scopes', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('routes each scope to its own generated client', () => {
    const m = mocks();
    configure(m);
    const api = TestBed.inject(ChannelApi);

    api.list('system').subscribe();
    expect(m.system['listSystemChannels']).toHaveBeenCalledOnce();
    expect(m.tenant['listNotificationChannels']).not.toHaveBeenCalled();

    api.list('tenant').subscribe();
    expect(m.tenant['listNotificationChannels']).toHaveBeenCalledOnce();
    expect(m.system['listSystemChannels']).toHaveBeenCalledOnce();
  });

  it('points the system scope at the admin list', () => {
    expect(channelListPath('system')).toBe('/admin/system-channels');
    expect(channelListPath('tenant')).toBe('/channels');
  });
});

describe('ChannelsPage with the system scope', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  function create(scope: 'tenant' | 'system'): {
    fixture: ComponentFixture<ChannelsPage>;
    m: { tenant: Calls; system: Calls };
  } {
    const m = mocks();
    configure(m);
    const fixture = TestBed.createComponent(ChannelsPage);
    fixture.componentRef.setInput('scope', scope);
    fixture.detectChanges();
    return { fixture, m };
  }

  it('lists the system channels and links to the admin pages', () => {
    const { fixture, m } = create('system');

    expect(m.system['listSystemChannels']).toHaveBeenCalledOnce();
    expect(m.tenant['listNotificationChannels']).not.toHaveBeenCalled();
    const hrefs = [...(fixture.nativeElement as HTMLElement).querySelectorAll('a')].map((a) =>
      a.getAttribute('href'),
    );
    expect(hrefs).toContain('/admin/system-channels/new');
    expect(hrefs).toContain(`/admin/system-channels/${SYSTEM.id}`);
  });

  it('tests and deletes through the system client', () => {
    const { fixture, m } = create('system');
    const page = fixture.componentInstance;
    page['test'](SYSTEM);

    expect(m.system['testSystemChannel']).toHaveBeenCalledWith(SYSTEM.id);
    expect(m.tenant['testNotificationChannel']).not.toHaveBeenCalled();
  });

  it('keeps the tenant scope on the tenant client by default', () => {
    const m = mocks();
    configure(m);
    const fixture = TestBed.createComponent(ChannelsPage);
    fixture.detectChanges();

    expect(m.tenant['listNotificationChannels']).toHaveBeenCalledOnce();
    expect(m.system['listSystemChannels']).not.toHaveBeenCalled();
  });
});

describe('ChannelEditorPage with the system scope', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  function create(channelId?: string): {
    fixture: ComponentFixture<ChannelEditorPage>;
    m: { tenant: Calls; system: Calls };
  } {
    const m = mocks();
    configure(m);
    const fixture = TestBed.createComponent(ChannelEditorPage);
    fixture.componentRef.setInput('scope', 'system');
    if (channelId) {
      fixture.componentRef.setInput('channelId', channelId);
    }
    fixture.detectChanges();
    return { fixture, m };
  }

  it('loads an existing channel through the system client', () => {
    const { m } = create(SYSTEM.id);

    expect(m.system['getSystemChannel']).toHaveBeenCalledWith(SYSTEM.id);
    expect(m.tenant['getNotificationChannel']).not.toHaveBeenCalled();
  });

  it('creates a system channel with the system event only and no default flag', () => {
    const { fixture, m } = create();
    const page = fixture.componentInstance;
    page['form'].controls.name.setValue('Mail');
    page['shoutrrrChoice'].setValue('gotify');
    page['fieldControl']('host').setValue('gotify.local');
    page['fieldControl']('token').setValue('a/b');
    page['submit']();

    expect(m.tenant['createNotificationChannel']).not.toHaveBeenCalled();
    const input = lastInput(m.system['createSystemChannel']);
    expect(input).toMatchObject({
      name: 'Mail',
      kind: 'shoutrrr',
      url: 'gotify://gotify.local/a%2Fb',
      events: ['system'],
    });
    expect(input.isDefault).toBeUndefined();
  });

  it('updates a system channel through the system client and keeps the stored url', () => {
    const { fixture, m } = create(SYSTEM.id);
    fixture.componentInstance['submit']();

    expect(m.tenant['updateNotificationChannel']).not.toHaveBeenCalled();
    expect(m.system['updateSystemChannel']).toHaveBeenCalledOnce();
    const [id, input] = m.system['updateSystemChannel'].mock.calls[0] as [
      string,
      NotificationChannelInput,
    ];
    expect(id).toBe(SYSTEM.id);
    expect(input.url).toBeUndefined();
    expect(input.events).toEqual(['system']);
  });
});
