// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { of } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { NotificationsService } from '../../api/gen/endpoints/notifications/notifications.service';
import { NotificationChannel, NotificationChannelInput } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { ChannelEditorPage } from './channel-editor-page';

const SHOUTRRR: NotificationChannel = {
  id: '6f1c7a2e-0000-4000-8000-000000000001',
  name: 'Ops Telegram',
  kind: 'shoutrrr',
  maskedUrl: 'telegram://***@telegram?chats=@ops',
  events: ['down', 'up'],
  isDefault: false,
  state: 'ok',
  createdAt: '2026-10-01T10:00:00Z',
};

const WEBHOOK: NotificationChannel = {
  id: '6f1c7a2e-0000-4000-8000-000000000002',
  name: 'Hook',
  kind: 'webhook',
  webhook: {
    url: 'https://hooks.example.org/in',
    method: 'PUT',
    headers: { Authorization: '***' },
  },
  events: ['down'],
  isDefault: true,
  state: 'ok',
  createdAt: '2026-10-01T10:00:00Z',
};

interface Api {
  get?: NotificationChannel;
  create: ReturnType<typeof vi.fn>;
  update: ReturnType<typeof vi.fn>;
}

function setup(channel: NotificationChannel | null): {
  fixture: ComponentFixture<ChannelEditorPage>;
  api: Api;
} {
  const api: Api = {
    get: channel ?? undefined,
    create: vi.fn(() => of({ ...SHOUTRRR })),
    update: vi.fn(() => of({ ...SHOUTRRR })),
  };
  TestBed.configureTestingModule({
    imports: [
      ChannelEditorPage,
      TranslocoTestingModule.forRoot({
        langs: { en: {} },
        translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
      }),
    ],
    providers: [
      provideRouter([{ path: '**', children: [] }]),
      {
        provide: NotificationsService,
        useValue: {
          getNotificationChannel: () => of(channel as NotificationChannel),
          createNotificationChannel: api.create,
          updateNotificationChannel: api.update,
        },
      },
      { provide: AuthService, useValue: { canAdmin: signal(true).asReadonly() } },
    ],
  });
  const fixture = TestBed.createComponent(ChannelEditorPage);
  if (channel) {
    fixture.componentRef.setInput('channelId', channel.id);
  }
  fixture.detectChanges();
  return { fixture, api };
}

/** The component instance behind the fixture, typed for the test helpers. */
function component(fixture: ComponentFixture<ChannelEditorPage>): ChannelEditorPage {
  return fixture.componentInstance;
}

function lastInput(mock: ReturnType<typeof vi.fn>): NotificationChannelInput {
  // create(input) and update(id, input): the payload is always the last argument.
  return mock.mock.calls.at(-1)?.at(-1) as NotificationChannelInput;
}

describe('ChannelEditorPage', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('omits url on update when the stored Shoutrrr destination is kept', () => {
    const { fixture, api } = setup(SHOUTRRR);
    component(fixture)['submit']();

    expect(api.update).toHaveBeenCalledOnce();
    const input = lastInput(api.update);
    expect(input.url).toBeUndefined();
    expect(input.kind).toBe('shoutrrr');
    expect(input.name).toBe('Ops Telegram');
    expect(input.events).toEqual(['down', 'up']);
  });

  it('sends the built url on update only after the user replaces the destination', () => {
    const { fixture, api } = setup(SHOUTRRR);
    const page = component(fixture);
    page['startReplace']();
    page['shoutrrrChoice'].setValue('discord');
    page['fieldControl']('token').setValue('tok/en');
    page['fieldControl']('id').setValue('4242');
    page['submit']();

    expect(lastInput(api.update).url).toBe('discord://tok%2Fen@4242');
  });

  it('requires a destination when creating a Shoutrrr channel', () => {
    const { fixture, api } = setup(null);
    const page = component(fixture);
    page['form'].controls.name.setValue('New');
    page['submit']();

    expect(api.create).not.toHaveBeenCalled();
    expect(page['shoutrrrError']()).not.toBeNull();
  });

  it('creates a Shoutrrr channel with the built url', () => {
    const { fixture, api } = setup(null);
    const page = component(fixture);
    page['form'].controls.name.setValue('Mail');
    page['shoutrrrChoice'].setValue('gotify');
    page['fieldControl']('host').setValue('gotify.local');
    page['fieldControl']('token').setValue('a/b');
    page['submit']();

    expect(lastInput(api.create)).toMatchObject({
      name: 'Mail',
      kind: 'shoutrrr',
      url: 'gotify://gotify.local/a%2Fb',
      events: ['down', 'up'],
    });
  });

  it('keeps the stored webhook headers masked and omits unknown values', () => {
    const { fixture, api } = setup(WEBHOOK);
    component(fixture)['submit']();

    expect(api.update).toHaveBeenCalledOnce();
    const input = lastInput(api.update);
    expect(input.url).toBeUndefined();
    expect(input.webhook).toEqual({
      url: 'https://hooks.example.org/in',
      method: 'PUT',
      headers: { Authorization: '***' },
      bodyTemplate: undefined,
    });
  });

  it('refuses to save without at least one event', () => {
    const { fixture, api } = setup(null);
    const page = component(fixture);
    page['form'].controls.name.setValue('X');
    page['form'].controls.events.setValue({
      down: false,
      up: false,
      degraded: false,
      partial: false,
    });
    page['shoutrrrChoice'].setValue('expert');
    page['expertUrl'].setValue('ntfy://ntfy.sh/t');
    page['submit']();

    expect(api.create).not.toHaveBeenCalled();
    expect(page['formError']()).not.toBeNull();
  });
});
