// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { Observable, of, throwError } from 'rxjs';
import { beforeEach, describe, expect, it } from 'vitest';
import { NotificationsService } from '../../api/gen/endpoints/notifications/notifications.service';
import { NotificationChannel } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { ChannelsPage } from './channels-page';

const CHANNEL: NotificationChannel = {
  id: '6f1c7a2e-0000-4000-8000-000000000001',
  name: 'Ops Telegram',
  kind: 'shoutrrr',
  maskedUrl: 'telegram://***@telegram?chats=@ops',
  events: ['down', 'up'],
  isDefault: true,
  state: 'failing',
  lastError: 'HTTP 500',
  createdAt: '2026-10-01T10:00:00Z',
};

const WEBHOOK: NotificationChannel = {
  id: '6f1c7a2e-0000-4000-8000-000000000002',
  name: 'Hook',
  kind: 'webhook',
  webhook: {
    url: 'https://hooks.example.org/in',
    headers: { Authorization: '***' },
  },
  events: ['down'],
  isDefault: false,
  state: 'ok',
  createdAt: '2026-10-01T10:00:00Z',
};

interface Setup {
  list: Observable<NotificationChannel[]>;
  canAdmin?: boolean;
}

function setup({ list, canAdmin = true }: Setup): ComponentFixture<ChannelsPage> {
  TestBed.configureTestingModule({
    imports: [
      ChannelsPage,
      TranslocoTestingModule.forRoot({
        langs: { en: {} },
        translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
      }),
    ],
    providers: [
      provideRouter([]),
      {
        provide: NotificationsService,
        useValue: {
          listNotificationChannels: () => list,
          testNotificationChannel: () => of({ delivered: true, durationMs: 42 }),
          deleteNotificationChannel: () => of(undefined),
        },
      },
      { provide: AuthService, useValue: { canAdmin: signal(canAdmin).asReadonly() } },
    ],
  });
  const fixture = TestBed.createComponent(ChannelsPage);
  fixture.detectChanges();
  return fixture;
}

function text(fixture: ComponentFixture<ChannelsPage>): string {
  return (fixture.nativeElement as HTMLElement).textContent ?? '';
}

describe('ChannelsPage', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('lists channels with masked targets and never shows header values', () => {
    const fixture = setup({ list: of([CHANNEL, WEBHOOK]) });
    const content = text(fixture);

    expect(content).toContain('Ops Telegram');
    expect(content).toContain('telegram://***@telegram?chats=@ops');
    expect(content).toContain('https://hooks.example.org/in');
    expect(content).toContain('HTTP 500');
    expect(content).not.toContain('Authorization');
  });

  it('shows the empty state when there are no channels', () => {
    const fixture = setup({ list: of([]) });
    expect(text(fixture)).toContain('channels.empty');
  });

  it('shows an error with a retry button when loading fails', () => {
    const fixture = setup({ list: throwError(() => new HttpErrorResponse({ status: 500 })) });
    const element = fixture.nativeElement as HTMLElement;

    expect(element.querySelector('[role="alert"]')).not.toBeNull();
    expect(element.textContent).toContain('common.retry');
  });

  it('hides the admin actions for non-admin users', () => {
    const fixture = setup({ list: of([CHANNEL]), canAdmin: false });
    const content = text(fixture);

    expect(content).not.toContain('channels.test');
    expect(content).not.toContain('common.delete');
    expect(content).not.toContain('channels.new');
  });

  it('shows admin actions for admins', () => {
    const fixture = setup({ list: of([CHANNEL]) });
    expect(text(fixture)).toContain('channels.test');
  });
});
