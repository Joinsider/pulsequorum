// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { describe, expect, it } from 'vitest';
import { normalizeOrigin, normalizeOrigins, trustedNodes } from './node-origins';

describe('normalizeOrigin', () => {
  const accepted: [string, string][] = [
    ['https://n1.example.com', 'https://n1.example.com'],
    ['https://n1.example.com/', 'https://n1.example.com'],
    ['HTTPS://N1.Example.COM', 'https://n1.example.com'],
    ['https://n1.example.com:443', 'https://n1.example.com'],
    ['https://n1.example.com:8443', 'https://n1.example.com:8443'],
    ['https://192.0.2.10', 'https://192.0.2.10'],
    ['https://[2001:db8::1]:8443', 'https://[2001:db8::1]:8443'],
    ['http://localhost:8080', 'http://localhost:8080'],
    ['http://127.0.0.1:8080', 'http://127.0.0.1:8080'],
    ['http://[::1]:8080', 'http://[::1]:8080'],
    ['http://localhost:80', 'http://localhost'],
    ['https://xn--bcher-kva.example', 'https://xn--bcher-kva.example'],
  ];
  it.each(accepted)('accepts %s as %s', (raw, want) => {
    expect(normalizeOrigin(raw)).toBe(want);
  });

  const rejected: unknown[] = [
    // Scheme and loopback rule.
    'http://n1.example.com',
    'http://127.0.0.2',
    'http://localhost.example.com',
    'ftp://n1.example.com',
    'wss://n1.example.com',
    'javascript:alert(1)',
    '//n1.example.com',
    'n1.example.com',
    'https:n1.example.com',
    'https:/n1.example.com',
    // Path, query, fragment, userinfo.
    'https://n1.example.com/app',
    'https://n1.example.com//',
    'https://n1.example.com/.',
    'https://n1.example.com/%2e',
    'https://n1.example.com?',
    'https://n1.example.com?a=1',
    'https://n1.example.com#',
    'https://n1.example.com#x',
    'https://user@n1.example.com',
    'https://user:pw@n1.example.com',
    'https://n1.example.com\\@evil.example',
    'https://evil.example\\n1.example.com',
    // Host.
    'https://',
    'https://:8443',
    'https://n1_example.com',
    'https://-n1.example.com',
    'https://n1.example.com.',
    'https://bücher.example',
    'https://0x7f000001',
    'https://127.1',
    'https://[fe80::1%25eth0]',
    'https://[2001:db8:0::1]',
    // Port.
    'https://n1.example.com:0',
    'https://n1.example.com:65536',
    // Characters, length and type.
    '',
    ' https://n1.example.com',
    'https://n1.example.com ',
    'https://n1.example.com\r\nX-Evil: 1',
    'https://n1.exa\tmple.com',
    'https://' + 'a.'.repeat(130) + 'com',
    undefined,
    null,
    42,
    ['https://n1.example.com'],
  ];
  it.each(rejected)('rejects %j', (raw) => {
    expect(normalizeOrigin(raw)).toBeNull();
  });
});

describe('normalizeOrigins', () => {
  it('drops invalid entries and duplicates and keeps the order', () => {
    expect(
      normalizeOrigins([
        'https://n2.example.com',
        'http://evil.example',
        'https://N1.example.com/',
        'https://n2.example.com:443',
        'https://n1.example.com',
      ]),
    ).toEqual(['https://n2.example.com', 'https://n1.example.com']);
  });
});

describe('trustedNodes', () => {
  const configured = ['https://n1.example.com', 'https://n2.example.com', 'https://n3.example.com'];

  it('keeps only discovered nodes that are configured, in configured order', () => {
    expect(
      trustedNodes(configured, [
        'https://n3.example.com',
        'https://evil.example',
        'https://n1.example.com/',
      ]),
    ).toEqual(['https://n1.example.com', 'https://n3.example.com']);
  });

  it('never adds a node that only discovery reports', () => {
    expect(trustedNodes(['https://n1.example.com'], ['https://n4.example.com'])).toEqual([]);
  });

  it('ignores invalid configured entries', () => {
    expect(
      trustedNodes(
        ['http://n1.example.com', 'https://n2.example.com'],
        ['http://n1.example.com', 'https://n2.example.com'],
      ),
    ).toEqual(['https://n2.example.com']);
  });
});
