// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { inject, Injectable, Injector } from '@angular/core';
import { Observable } from 'rxjs';
import { NotificationsService } from '../../api/gen/endpoints/notifications/notifications.service';
import { SystemAdminService } from '../../api/gen/endpoints/system-admin/system-admin.service';
import { DeliveryResult, NotificationChannel, NotificationChannelInput } from '../../api/gen/model';

/**
 * Which channels a page works on: the tenant's channels (`tenant`, FA-UI-04) or
 * the platform's system channels (`system`, FA-OBS-01, ADR 0025).
 */
export type ChannelScope = 'tenant' | 'system';

/**
 * The channel operations of both scopes behind one interface, so the list and
 * the editor pages are the same for tenant and system channels.
 *
 * The generated clients are resolved on use, not at construction: a tenant page
 * never needs the system client, and specs that mock only the tenant client keep working.
 */
@Injectable({ providedIn: 'root' })
export class ChannelApi {
  private readonly injector = inject(Injector);

  list(scope: ChannelScope): Observable<NotificationChannel[]> {
    return scope === 'system'
      ? this.injector.get(SystemAdminService).listSystemChannels()
      : this.injector.get(NotificationsService).listNotificationChannels();
  }

  get(scope: ChannelScope, id: string): Observable<NotificationChannel> {
    return scope === 'system'
      ? this.injector.get(SystemAdminService).getSystemChannel(id)
      : this.injector.get(NotificationsService).getNotificationChannel(id);
  }

  create(scope: ChannelScope, input: NotificationChannelInput): Observable<NotificationChannel> {
    return scope === 'system'
      ? this.injector.get(SystemAdminService).createSystemChannel(input)
      : this.injector.get(NotificationsService).createNotificationChannel(input);
  }

  update(
    scope: ChannelScope,
    id: string,
    input: NotificationChannelInput,
  ): Observable<NotificationChannel> {
    return scope === 'system'
      ? this.injector.get(SystemAdminService).updateSystemChannel(id, input)
      : this.injector.get(NotificationsService).updateNotificationChannel(id, input);
  }

  remove(scope: ChannelScope, id: string): Observable<void> {
    return scope === 'system'
      ? this.injector.get(SystemAdminService).deleteSystemChannel(id)
      : this.injector.get(NotificationsService).deleteNotificationChannel(id);
  }

  test(scope: ChannelScope, id: string): Observable<DeliveryResult> {
    return scope === 'system'
      ? this.injector.get(SystemAdminService).testSystemChannel(id)
      : this.injector.get(NotificationsService).testNotificationChannel(id);
  }
}

/** The list page of a scope, which the editor's cancel link and the list's links use. */
export function channelListPath(scope: ChannelScope): string {
  return scope === 'system' ? '/admin/system-channels' : '/channels';
}
