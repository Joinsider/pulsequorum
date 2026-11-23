// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

/**
 * Builds Shoutrrr URLs (ADR 0014, FA-UI-07) from the guided forms. Pure functions
 * without Angular dependencies, so the encoding rules are covered by unit tests.
 *
 * Every user-supplied component is percent-encoded with `encodeURIComponent`,
 * so characters such as `@ : / ? #` in passwords or tokens cannot change the
 * meaning of the URL. Host names are not encoded but validated against a strict
 * character set instead.
 *
 * Formats follow the Shoutrrr service documentation (nicholas-fedor/shoutrrr).
 * Slack, Teams and the email query parameter names were taken from the same
 * documentation; see the report for the points that could not be verified
 * against a running server.
 */

export const SHOUTRRR_SERVICES = [
  'email',
  'telegram',
  'discord',
  'slack',
  'ntfy',
  'gotify',
  'teams',
  'matrix',
  'pushover',
] as const;

export type ShoutrrrService = (typeof SHOUTRRR_SERVICES)[number];

/** Picker value: a guided service or the free-text expert mode. */
export type ShoutrrrChoice = ShoutrrrService | 'expert';

export type FieldKind = 'text' | 'secret' | 'select';

export interface ServiceField {
  /** Form control name; shared names (e.g. `host`) keep their value when switching service. */
  readonly name: string;
  readonly kind: FieldKind;
  readonly required: boolean;
  /** Allowed values for `select` fields; the first one is the default. */
  readonly options?: readonly string[];
}

export const SERVICE_FIELDS: Readonly<Record<ShoutrrrService, readonly ServiceField[]>> = {
  email: [
    { name: 'host', kind: 'text', required: true },
    { name: 'port', kind: 'text', required: false },
    { name: 'username', kind: 'text', required: false },
    { name: 'password', kind: 'secret', required: false },
    { name: 'from', kind: 'text', required: true },
    { name: 'to', kind: 'text', required: true },
  ],
  telegram: [
    { name: 'token', kind: 'secret', required: true },
    { name: 'chats', kind: 'text', required: true },
  ],
  discord: [
    { name: 'token', kind: 'secret', required: true },
    { name: 'id', kind: 'text', required: true },
  ],
  slack: [
    { name: 'mode', kind: 'select', required: true, options: ['hook', 'bot'] },
    { name: 'token', kind: 'secret', required: true },
    { name: 'channel', kind: 'text', required: true },
    { name: 'botname', kind: 'text', required: false },
  ],
  ntfy: [
    { name: 'host', kind: 'text', required: false },
    { name: 'username', kind: 'text', required: false },
    { name: 'password', kind: 'secret', required: false },
    { name: 'topic', kind: 'text', required: true },
  ],
  gotify: [
    { name: 'host', kind: 'text', required: true },
    { name: 'token', kind: 'secret', required: true },
  ],
  teams: [{ name: 'webhookUrl', kind: 'secret', required: true }],
  matrix: [
    { name: 'host', kind: 'text', required: true },
    { name: 'port', kind: 'text', required: false },
    { name: 'username', kind: 'text', required: true },
    { name: 'password', kind: 'secret', required: true },
    { name: 'rooms', kind: 'text', required: false },
  ],
  pushover: [
    { name: 'apiToken', kind: 'secret', required: true },
    { name: 'userKey', kind: 'secret', required: true },
    { name: 'devices', kind: 'text', required: false },
  ],
};

/** Every form control the guided forms use, each once. */
export const ALL_SERVICE_FIELD_NAMES: readonly string[] = [
  ...new Set(Object.values(SERVICE_FIELDS).flatMap((fields) => fields.map((f) => f.name))),
];

export type ShoutrrrBuild =
  | { readonly ok: true; readonly url: string }
  | { readonly ok: false; readonly error: 'missing' | 'invalid'; readonly field: string };

/** Values of the guided form by control name. Secrets are used as typed (not trimmed). */
export type ServiceValues = Readonly<Record<string, string | null | undefined>>;

const HOST_PATTERN = /^[A-Za-z0-9.\-[\]:]+$/;
const PORT_PATTERN = /^\d{1,5}$/;
const HTTPS_PATTERN = /^https:\/\/\S+$/i;
const EXPERT_PATTERN = /^[a-z][a-z0-9+.-]*:\/\/\S+$/i;

/** Percent-encodes one URL component. */
export function encodeComponent(value: string): string {
  return encodeURIComponent(value);
}

/** Splits a comma-separated list, trims items and drops empty ones. */
export function splitList(value: string): string[] {
  return value
    .split(',')
    .map((item) => item.trim())
    .filter((item) => item !== '');
}

function raw(values: ServiceValues, name: string): string {
  return values[name] ?? '';
}

function text(values: ServiceValues, name: string): string {
  return raw(values, name).trim();
}

function fail(field: string): ShoutrrrBuild {
  return { ok: false, error: 'invalid', field };
}

function ok(url: string): ShoutrrrBuild {
  return { ok: true, url };
}

/** Host name, IPv4, IPv6 in brackets, with an optional port. Never encoded. */
function validHost(host: string): boolean {
  return HOST_PATTERN.test(host);
}

function portPart(port: string): string {
  return port === '' ? '' : `:${port}`;
}

/** `user[:password]@` for URLs, or the empty string without a user. */
function userInfo(username: string, password: string): string {
  if (username === '') {
    return '';
  }
  const passPart = password === '' ? '' : `:${encodeComponent(password)}`;
  return `${encodeComponent(username)}${passPart}@`;
}

/**
 * Builds a Shoutrrr URL for one guided service. Checks required fields first,
 * then the format of each field. Returns the field that caused the failure.
 */
export function buildShoutrrrUrl(service: ShoutrrrService, values: ServiceValues): ShoutrrrBuild {
  for (const field of SERVICE_FIELDS[service]) {
    const value = field.kind === 'secret' ? raw(values, field.name) : text(values, field.name);
    if (field.required && value.trim() === '') {
      return { ok: false, error: 'missing', field: field.name };
    }
  }
  return BUILDERS[service](values);
}

const BUILDERS: Record<ShoutrrrService, (values: ServiceValues) => ShoutrrrBuild> = {
  /** smtp://user:pass@host:port/?fromaddress=…&toaddresses=a,b */
  email(values) {
    const host = text(values, 'host');
    const port = text(values, 'port');
    if (!validHost(host)) {
      return fail('host');
    }
    if (port !== '' && !PORT_PATTERN.test(port)) {
      return fail('port');
    }
    const from = text(values, 'from');
    const recipients = splitList(text(values, 'to'));
    if (recipients.length === 0) {
      return fail('to');
    }
    const auth = userInfo(text(values, 'username'), raw(values, 'password'));
    const toList = recipients.map(encodeComponent).join(',');
    return ok(
      `smtp://${auth}${host}${portPart(port)}/?fromaddress=${encodeComponent(from)}&toaddresses=${toList}`,
    );
  },

  /** telegram://token@telegram?chats=@channel,chatId */
  telegram(values) {
    const chats = splitList(text(values, 'chats'));
    if (chats.length === 0) {
      return fail('chats');
    }
    const token = raw(values, 'token').trim();
    return ok(
      `telegram://${encodeComponent(token)}@telegram?chats=${chats.map(encodeComponent).join(',')}`,
    );
  },

  /** discord://token@id */
  discord(values) {
    const id = text(values, 'id');
    const token = raw(values, 'token').trim();
    return ok(`discord://${encodeComponent(token)}@${encodeComponent(id)}`);
  },

  /** slack://hook:token@channel or slack://xoxb:token@channel, optional ?botname= */
  slack(values) {
    const channel = text(values, 'channel');
    const token = raw(values, 'token').trim();
    const prefix = text(values, 'mode') === 'bot' ? 'xoxb' : 'hook';
    const botname = text(values, 'botname');
    const query = botname === '' ? '' : `?botname=${encodeComponent(botname)}`;
    return ok(`slack://${prefix}:${encodeComponent(token)}@${encodeComponent(channel)}${query}`);
  },

  /** ntfy://[user[:pass]@]host/topic, host defaults to ntfy.sh */
  ntfy(values) {
    const host = text(values, 'host') || 'ntfy.sh';
    if (!validHost(host)) {
      return fail('host');
    }
    const topic = text(values, 'topic');
    const auth = userInfo(text(values, 'username'), raw(values, 'password'));
    return ok(`ntfy://${auth}${host}/${encodeComponent(topic)}`);
  },

  /** gotify://host/token */
  gotify(values) {
    const host = text(values, 'host');
    if (!validHost(host)) {
      return fail('host');
    }
    const token = raw(values, 'token').trim();
    return ok(`gotify://${host}/${encodeComponent(token)}`);
  },

  /** teams://?host=<encoded Power Automate workflow URL> */
  teams(values) {
    const webhookUrl = raw(values, 'webhookUrl').trim();
    if (!HTTPS_PATTERN.test(webhookUrl)) {
      return fail('webhookUrl');
    }
    return ok(`teams://?host=${encodeComponent(webhookUrl)}`);
  },

  /** matrix://user:password@host:port/?rooms=!id1,#alias2 */
  matrix(values) {
    const host = text(values, 'host');
    const port = text(values, 'port');
    if (!validHost(host)) {
      return fail('host');
    }
    if (port !== '' && !PORT_PATTERN.test(port)) {
      return fail('port');
    }
    const auth = userInfo(text(values, 'username'), raw(values, 'password'));
    const rooms = splitList(text(values, 'rooms'));
    const query = rooms.length === 0 ? '' : `?rooms=${rooms.map(encodeComponent).join(',')}`;
    return ok(`matrix://${auth}${host}${portPart(port)}/${query}`);
  },

  /** pushover://shoutrrr:apiToken@userKey/?devices=a,b */
  pushover(values) {
    const apiToken = raw(values, 'apiToken').trim();
    const userKey = raw(values, 'userKey').trim();
    const devices = splitList(text(values, 'devices'));
    const query = devices.length === 0 ? '' : `?devices=${devices.map(encodeComponent).join(',')}`;
    return ok(
      `pushover://shoutrrr:${encodeComponent(apiToken)}@${encodeComponent(userKey)}/${query}`,
    );
  },
};

/** Expert mode: the user enters the whole URL. Only the scheme shape is checked. */
export function buildExpertUrl(value: string | null | undefined): ShoutrrrBuild {
  const url = (value ?? '').trim();
  if (url === '') {
    return { ok: false, error: 'missing', field: 'url' };
  }
  if (!EXPERT_PATTERN.test(url)) {
    return fail('url');
  }
  return ok(url);
}
