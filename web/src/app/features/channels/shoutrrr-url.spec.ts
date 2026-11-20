// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { describe, expect, it } from 'vitest';
import {
  ALL_SERVICE_FIELD_NAMES,
  buildExpertUrl,
  buildShoutrrrUrl,
  encodeComponent,
  splitList,
} from './shoutrrr-url';

describe('encodeComponent', () => {
  it.each([
    ['@', '%40'],
    [':', '%3A'],
    ['/', '%2F'],
    ['?', '%3F'],
    ['#', '%23'],
    ['%', '%25'],
    [' ', '%20'],
  ])('encodes %s as %s', (input, expected) => {
    expect(encodeComponent(input)).toBe(expected);
  });
});

describe('splitList', () => {
  it('trims items and drops empty ones', () => {
    expect(splitList(' a@x.org , ,b@x.org,')).toEqual(['a@x.org', 'b@x.org']);
  });
  it('returns an empty list for blank input', () => {
    expect(splitList('  ,  ')).toEqual([]);
  });
});

describe('buildShoutrrrUrl', () => {
  it('builds an SMTP URL with encoded credentials and recipients', () => {
    const result = buildShoutrrrUrl('email', {
      host: 'smtp.example.org',
      port: '587',
      username: 'ops@example.org',
      password: 'p@ss:w/rd?#%',
      from: 'uptime@example.org',
      to: 'a@example.org, b@example.org',
    });
    expect(result).toEqual({
      ok: true,
      url:
        'smtp://ops%40example.org:p%40ss%3Aw%2Frd%3F%23%25@smtp.example.org:587/' +
        '?fromaddress=uptime%40example.org&toaddresses=a%40example.org,b%40example.org',
    });
  });

  it('omits userinfo and port when they are empty', () => {
    const result = buildShoutrrrUrl('email', {
      host: 'relay.local',
      from: 'x@example.org',
      to: 'y@example.org',
    });
    expect(result).toEqual({
      ok: true,
      url: 'smtp://relay.local/?fromaddress=x%40example.org&toaddresses=y%40example.org',
    });
  });

  it('rejects a non-numeric SMTP port', () => {
    expect(buildShoutrrrUrl('email', { host: 'h', port: 'abc', from: 'a', to: 'b' })).toEqual({
      ok: false,
      error: 'invalid',
      field: 'port',
    });
  });

  it('rejects a host with path or userinfo characters', () => {
    expect(buildShoutrrrUrl('email', { host: 'evil.org/x@y', from: 'a', to: 'b' })).toEqual({
      ok: false,
      error: 'invalid',
      field: 'host',
    });
  });

  it('builds a Telegram URL and keeps the @ of channel names encoded', () => {
    expect(buildShoutrrrUrl('telegram', { token: '123456:ABC-def', chats: '@ops, -1001' })).toEqual(
      {
        ok: true,
        url: 'telegram://123456%3AABC-def@telegram?chats=%40ops,-1001',
      },
    );
  });

  it('reports a missing Telegram chat', () => {
    expect(buildShoutrrrUrl('telegram', { token: 't', chats: ' ' })).toEqual({
      ok: false,
      error: 'missing',
      field: 'chats',
    });
  });

  it('builds a Discord URL', () => {
    expect(buildShoutrrrUrl('discord', { token: 'tok/en', id: '4242' })).toEqual({
      ok: true,
      url: 'discord://tok%2Fen@4242',
    });
  });

  it('builds Slack URLs for hook and bot tokens with optional botname', () => {
    expect(
      buildShoutrrrUrl('slack', {
        mode: 'hook',
        token: 'T1-B2-C3',
        channel: 'alerts',
        botname: 'Uptime Bot',
      }),
    ).toEqual({ ok: true, url: 'slack://hook:T1-B2-C3@alerts?botname=Uptime%20Bot' });
    expect(buildShoutrrrUrl('slack', { mode: 'bot', token: 'xoxb-1', channel: '#ops' })).toEqual({
      ok: true,
      url: 'slack://xoxb:xoxb-1@%23ops',
    });
  });

  it('defaults the ntfy host to ntfy.sh and encodes credentials', () => {
    expect(
      buildShoutrrrUrl('ntfy', { host: '', username: 'me', password: 'a:b', topic: 'alerts' }),
    ).toEqual({ ok: true, url: 'ntfy://me:a%3Ab@ntfy.sh/alerts' });
  });

  it('keeps the host of a self-hosted ntfy server with a port', () => {
    expect(buildShoutrrrUrl('ntfy', { host: 'push.local:8080', topic: 't' })).toEqual({
      ok: true,
      url: 'ntfy://push.local:8080/t',
    });
  });

  it('builds a Gotify URL with the token in the path', () => {
    expect(buildShoutrrrUrl('gotify', { host: 'gotify.local', token: 'a/b' })).toEqual({
      ok: true,
      url: 'gotify://gotify.local/a%2Fb',
    });
  });

  it('builds a Teams URL with the encoded workflow URL and rejects http', () => {
    expect(
      buildShoutrrrUrl('teams', { webhookUrl: 'https://flow.example.com/x?sig=a&b=c' }),
    ).toEqual({
      ok: true,
      url: 'teams://?host=https%3A%2F%2Fflow.example.com%2Fx%3Fsig%3Da%26b%3Dc',
    });
    expect(buildShoutrrrUrl('teams', { webhookUrl: 'http://flow.example.com' })).toEqual({
      ok: false,
      error: 'invalid',
      field: 'webhookUrl',
    });
  });

  it('builds a Matrix URL with port and rooms', () => {
    expect(
      buildShoutrrrUrl('matrix', {
        host: 'matrix.org',
        port: '443',
        username: '@bot:matrix.org',
        password: 'p:w@d',
        rooms: '!abc:matrix.org,#ops:matrix.org',
      }),
    ).toEqual({
      ok: true,
      url:
        'matrix://%40bot%3Amatrix.org:p%3Aw%40d@matrix.org:443/' +
        '?rooms=!abc%3Amatrix.org,%23ops%3Amatrix.org',
    });
  });

  it('builds a Pushover URL with the fixed shoutrrr user and devices', () => {
    expect(
      buildShoutrrrUrl('pushover', {
        apiToken: 'a1/b',
        userKey: 'u2?',
        devices: 'phone, tablet',
      }),
    ).toEqual({
      ok: true,
      url: 'pushover://shoutrrr:a1%2Fb@u2%3F/?devices=phone,tablet',
    });
  });

  it('keeps leading and trailing spaces of a secret as typed', () => {
    expect(buildShoutrrrUrl('matrix', { host: 'h', username: 'u', password: ' pw ' })).toEqual({
      ok: true,
      url: 'matrix://u:%20pw%20@h/',
    });
  });

  it('reports the first missing required field', () => {
    expect(buildShoutrrrUrl('pushover', { apiToken: '', userKey: 'x' })).toEqual({
      ok: false,
      error: 'missing',
      field: 'apiToken',
    });
  });

  it('ignores null values from the form', () => {
    expect(buildShoutrrrUrl('gotify', { host: null, token: 't' })).toEqual({
      ok: false,
      error: 'missing',
      field: 'host',
    });
  });

  it('exposes every control name once', () => {
    expect(new Set(ALL_SERVICE_FIELD_NAMES).size).toBe(ALL_SERVICE_FIELD_NAMES.length);
    expect(ALL_SERVICE_FIELD_NAMES).toContain('webhookUrl');
  });
});

describe('buildExpertUrl', () => {
  it('accepts a URL with a scheme and trims it', () => {
    expect(buildExpertUrl('  ntfy://ntfy.sh/topic  ')).toEqual({
      ok: true,
      url: 'ntfy://ntfy.sh/topic',
    });
  });

  it('rejects an empty value', () => {
    expect(buildExpertUrl('   ')).toEqual({ ok: false, error: 'missing', field: 'url' });
  });

  it.each(['ntfy.sh/topic', 'telegram:/token', '://x', 'two words://x'])('rejects %s', (value) => {
    expect(buildExpertUrl(value)).toEqual({ ok: false, error: 'invalid', field: 'url' });
  });
});
