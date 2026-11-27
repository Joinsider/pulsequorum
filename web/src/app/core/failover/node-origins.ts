// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

/**
 * Node origins steer where the SPA sends bearer tokens (ADR 0036). This file is
 * security code: the rule mirrors `cluster.NormalizePublicOrigin` in the
 * backend and must not be loosened without a review.
 */

/** Longest node URL accepted before normalization, in characters. */
export const MAX_NODE_URL_LENGTH = 256;

const LOOPBACK_HOSTS = new Set(['localhost', '127.0.0.1', '[::1]']);

const DNS_NAME = /^(?!-)[a-z0-9-]{1,63}(?<!-)(\.(?!-)[a-z0-9-]{1,63}(?<!-))*$/;

/**
 * Checks a node URL and returns its origin (`scheme://host[:port]`, lower case,
 * default port dropped), or `null` if it is not acceptable:
 *
 * - `https`, or `http` only for `localhost`, `127.0.0.1` and `[::1]`;
 * - no userinfo, path (a single `/` is allowed), query or fragment, not even an
 *   empty `?` or `#`;
 * - printable ASCII without spaces or backslashes, at most
 *   {@link MAX_NODE_URL_LENGTH} characters. The WHATWG parser would otherwise
 *   strip or reinterpret such characters silently.
 */
export function normalizeOrigin(raw: unknown): string | null {
  if (typeof raw !== 'string' || raw.length === 0 || raw.length > MAX_NODE_URL_LENGTH) {
    return null;
  }
  // Printable ASCII only, without space, backslash, query or fragment marks.
  if (!/^[\x21-\x7e]+$/.test(raw) || /[\\?#]/.test(raw)) {
    return null;
  }
  // The scheme must be spelled out with "//": "https:host" is relative-looking
  // and parsed leniently by the WHATWG URL parser.
  if (!/^https?:\/\/[^/]/i.test(raw)) {
    return null;
  }
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return null;
  }
  if (url.username !== '' || url.password !== '' || raw.includes('@')) {
    return null;
  }
  if (url.pathname !== '/' || url.search !== '' || url.hash !== '') {
    return null;
  }
  // "https://host/" and "https://host" are fine; any other path is not, even
  // one the parser normalizes away ("/.", "/%2e").
  const afterScheme = raw.slice(raw.indexOf('//') + 2);
  const slash = afterScheme.indexOf('/');
  if (slash >= 0 && slash !== afterScheme.length - 1) {
    return null;
  }
  // WHATWG accepts port 0; the backend does not.
  if (url.port !== '' && Number(url.port) < 1) {
    return null;
  }
  if (url.protocol === 'http:') {
    if (!LOOPBACK_HOSTS.has(url.hostname)) {
      return null;
    }
  } else if (url.protocol !== 'https:') {
    return null;
  }
  // The parser maps numeric hosts such as 0x7f000001 or 127.1 to dotted IPv4.
  // Accept only hosts the operator wrote in their canonical form, so that the
  // list in config.json, the CSP and the backend registry agree.
  const writtenHost = hostPart(afterScheme.slice(0, slash >= 0 ? slash : undefined));
  if (writtenHost.toLowerCase() !== url.hostname) {
    return null;
  }
  // Same host alphabet as the backend: DNS labels of [a-z0-9-] without a
  // trailing dot, or an IPv6 literal (IPv4 passes as labels of digits).
  if (!url.hostname.startsWith('[') && !DNS_NAME.test(url.hostname)) {
    return null;
  }
  return url.origin;
}

/** Host of `host[:port]` as written, IPv6 literals with brackets. */
function hostPart(hostport: string): string {
  if (hostport.startsWith('[')) {
    const end = hostport.indexOf(']');
    return end < 0 ? hostport : hostport.slice(0, end + 1);
  }
  const colon = hostport.lastIndexOf(':');
  return colon < 0 ? hostport : hostport.slice(0, colon);
}

/**
 * Normalizes a list of node URLs: invalid entries are dropped, duplicates
 * removed, the order is kept.
 */
export function normalizeOrigins(raw: readonly unknown[]): string[] {
  const out: string[] = [];
  for (const entry of raw) {
    const origin = normalizeOrigin(entry);
    if (origin !== null && !out.includes(origin)) {
      out.push(origin);
    }
  }
  return out;
}

/**
 * Returns the discovered node URLs (from `/api/v1/cluster/nodes`) that are also
 * in the operator's configuration, in configuration order (ADR 0036, 1B). The
 * configuration from `config.json` is the only source of trust: a node the
 * backend reports but the configuration does not list is never used.
 *
 * Callers keep the configured list when discovery fails or returns nothing;
 * discovery may only narrow it, never widen it.
 */
export function trustedNodes(
  configured: readonly unknown[],
  discovered: readonly unknown[],
): string[] {
  const allowed = normalizeOrigins(configured);
  const seen = new Set(normalizeOrigins(discovered));
  return allowed.filter((origin) => seen.has(origin));
}
