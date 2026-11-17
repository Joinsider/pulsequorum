// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { NotificationEvent } from '../../api/gen/model';

/** One header row of the webhook editor, including what the server returned for it. */
export interface HeaderRow {
  readonly key: string;
  readonly value: string;
  /** Key as returned by the server; empty for rows the user added. */
  readonly serverKey: string;
  /** Masked value as returned by the server; empty for rows the user added. */
  readonly serverValue: string;
}

export type HeadersBuild =
  | { readonly ok: true; readonly headers: Record<string, string> | undefined }
  | {
      readonly ok: false;
      readonly error: 'emptyKey' | 'emptyValue' | 'duplicateKey';
      readonly index: number;
    };

/**
 * Turns the header rows into the `headers` map of a WebhookConfig.
 *
 * A row whose key and value still equal the server's masked value is sent back
 * unchanged, which the server reads as "keep the stored secret". Values the user
 * typed are sent as they are; the server never returns a secret, so nothing is
 * ever unmasked on the client. Rows without a key are ignored when they are
 * empty; a key without a value is an error.
 */
export function buildHeaders(rows: readonly HeaderRow[]): HeadersBuild {
  const headers: Record<string, string> = {};
  const seen = new Set<string>();
  for (const [index, row] of rows.entries()) {
    const key = row.key.trim();
    const value = row.value.trim();
    if (key === '') {
      if (value !== '') {
        return { ok: false, error: 'emptyKey', index };
      }
      continue;
    }
    const folded = key.toLowerCase();
    if (seen.has(folded)) {
      return { ok: false, error: 'duplicateKey', index };
    }
    seen.add(folded);
    const kept = row.serverKey === key && row.serverValue !== '' && value === row.serverValue;
    if (kept) {
      headers[key] = row.serverValue;
      continue;
    }
    // The masked value must not be sent under another key, and an empty value is no value.
    if (value === '' || (row.serverValue !== '' && value === row.serverValue)) {
      return { ok: false, error: 'emptyValue', index };
    }
    headers[key] = value;
  }
  return { ok: true, headers: Object.keys(headers).length > 0 ? headers : undefined };
}

/**
 * Events a tenant channel can subscribe to. `system` is reserved for the system
 * channels (ADR 0025), so it has no checkbox.
 */
export type TenantEvent = Exclude<NotificationEvent, 'system'>;

/** Event checkboxes by name, in the order the API lists them. */
export const EVENT_ORDER: readonly TenantEvent[] = [
  NotificationEvent.down,
  NotificationEvent.up,
  NotificationEvent.degraded,
  NotificationEvent.partial,
  NotificationEvent.flapping,
  NotificationEvent.channel_failed,
];

/** Selected events in API order. */
export function selectedEvents(flags: Readonly<Record<TenantEvent, boolean>>): TenantEvent[] {
  return EVENT_ORDER.filter((event) => flags[event]);
}

/** Checkbox state for an existing channel's events. */
export function eventFlags(events: readonly NotificationEvent[]): Record<TenantEvent, boolean> {
  return {
    down: events.includes(NotificationEvent.down),
    up: events.includes(NotificationEvent.up),
    degraded: events.includes(NotificationEvent.degraded),
    partial: events.includes(NotificationEvent.partial),
    flapping: events.includes(NotificationEvent.flapping),
    channel_failed: events.includes(NotificationEvent.channel_failed),
  };
}
