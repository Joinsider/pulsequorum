// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfigService } from '../config/config.service';
import {
  BLOCK_BASE_MS,
  DISCOVERY_INTERVAL_MS,
  FETCH,
  HEALTH_RECHECK_MS,
  NODE_CLOCK,
  NODE_RANDOM,
  NodeSelector,
} from './node-selector.service';

const A = 'https://a.example';
const B = 'https://b.example';
const C = 'https://c.example';
const Z = 'https://evil.example';

type Route =
  | { kind: 'json'; status: number; body: unknown; latency?: number }
  | { kind: 'reject'; error: unknown; latency?: number };

interface Harness {
  selector: NodeSelector;
  fetch: ReturnType<typeof vi.fn>;
  routes: Map<string, Route>;
  setConfig(nodes: string[]): void;
  health(origin: string, body: unknown, latency?: number): void;
  healthStatus(origin: string, status: number, latency?: number): void;
  timeout(origin: string): void;
  discovery(origin: string, body: unknown): void;
  /** Advances fake time (which also drives the fake fetch latencies). */
  advance(ms: number): Promise<void>;
  /** Advances fake time until the promise settles. */
  settle(promise: Promise<void>): Promise<void>;
  probed(): string[];
}

function setup(nodes: string[], random = 0.5): Harness {
  const routes = new Map<string, Route>();
  const configNodes = signal(nodes);

  const respond = async (input: string): Promise<Response> => {
    const route: Route = routes.get(input) ?? { kind: 'json', status: 404, body: null };
    await new Promise((resolve) => setTimeout(resolve, route.latency ?? 0));
    if (route.kind === 'reject') {
      throw route.error;
    }
    return new Response(JSON.stringify(route.body), { status: route.status });
  };
  const fetch = vi.fn(respond);

  TestBed.configureTestingModule({
    providers: [
      {
        provide: ConfigService,
        useValue: { config: () => ({ nodes: configNodes(), auth: { mode: 'local' } }) },
      },
      { provide: FETCH, useValue: fetch },
      // Date is faked by vi.useFakeTimers, so latencies and block times follow fake time.
      { provide: NODE_CLOCK, useValue: () => Date.now() },
      { provide: NODE_RANDOM, useValue: () => random },
    ],
  });

  return {
    selector: TestBed.inject(NodeSelector),
    fetch,
    routes,
    setConfig: (list) => configNodes.set(list),
    health: (origin, body, latency = 100) =>
      routes.set(origin + '/api/v1/health', { kind: 'json', status: 200, body, latency }),
    healthStatus: (origin, status, latency = 100) =>
      routes.set(origin + '/api/v1/health', { kind: 'json', status, body: null, latency }),
    timeout: (origin) =>
      routes.set(origin + '/api/v1/health', {
        kind: 'reject',
        error: new DOMException('timed out', 'TimeoutError'),
      }),
    discovery: (origin, body) =>
      routes.set(origin + '/api/v1/cluster/nodes', { kind: 'json', status: 200, body }),
    advance: async (ms) => {
      await vi.advanceTimersByTimeAsync(ms);
    },
    settle: async (promise) => {
      let done = false;
      void promise.then(
        () => (done = true),
        () => (done = true),
      );
      for (let i = 0; i < 2_000 && !done; i++) {
        await vi.advanceTimersByTimeAsync(10);
      }
      await promise;
    },
    probed: () =>
      fetch.mock.calls
        .map((call) => String(call[0]))
        .filter((url) => url.endsWith('/api/v1/health'))
        .map((url) => url.replace('/api/v1/health', '')),
  };
}

function healthy(latency = 100): Route {
  return { kind: 'json', status: 200, body: { status: 'ok', degraded: false }, latency };
}

function setVisibility(state: 'visible' | 'hidden'): void {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state });
}

describe('NodeSelector', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    setVisibility('visible');
  });

  afterEach(() => {
    TestBed.resetTestingModule();
    vi.useRealTimers();
    delete (document as unknown as Record<string, unknown>)['visibilityState'];
  });

  describe('selection', () => {
    it('picks the fastest healthy node', async () => {
      const h = setup([A, B, C]);
      h.routes.set(A + '/api/v1/health', healthy(300));
      h.routes.set(B + '/api/v1/health', healthy(100));
      h.routes.set(C + '/api/v1/health', healthy(200));

      await h.settle(h.selector.select());

      expect(h.selector.current()).toBe(B);
    });

    it('skips degraded, stale, failing and timed out nodes', async () => {
      const h = setup([A, B, C, Z]);
      h.health(A, { status: 'ok', degraded: true });
      h.health(B, { status: 'ok', stale: true });
      h.healthStatus(C, 500);
      h.timeout(Z);
      h.routes.set('https://e.example/api/v1/health', healthy(500));
      h.setConfig([A, B, C, Z, 'https://e.example']);

      await h.settle(h.selector.select());

      expect(h.selector.current()).toBe('https://e.example');
    });

    it('falls back to the first configured node when none is healthy', async () => {
      const h = setup([A, B]);
      h.healthStatus(A, 503);
      h.timeout(B);

      await h.settle(h.selector.select());

      expect(h.selector.current()).toBe(A);
    });

    it('keeps the current node while it is within 20 % of the fastest', async () => {
      const h = setup([A, B]);
      h.routes.set(A + '/api/v1/health', healthy(100));
      h.routes.set(B + '/api/v1/health', healthy(50));
      await h.settle(h.selector.select());
      expect(h.selector.current()).toBe(B);

      // B is slower, but within 20 % of A: stay on B.
      h.routes.set(B + '/api/v1/health', healthy(230));
      h.routes.set(A + '/api/v1/health', healthy(200));
      await h.settle(h.selector.select());
      expect(h.selector.current()).toBe(B);

      // Beyond 20 %: switch to A.
      h.routes.set(B + '/api/v1/health', healthy(300));
      await h.settle(h.selector.select());
      expect(h.selector.current()).toBe(A);
    });

    it('does not count the first selection as a switch', async () => {
      const h = setup([A]);
      h.health(A, { status: 'ok' });

      await h.settle(h.selector.select());

      expect(h.selector.switchedAt()).toBeNull();
    });
  });

  describe('block list', () => {
    it('blocks a failed node for 30 s, then doubles up to 5 min', async () => {
      const h = setup([A, B]);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok' });
      await h.settle(h.selector.select());
      expect(h.selector.current()).toBe(A);

      const expected = [30_000, 60_000, 120_000, 240_000, 300_000, 300_000];
      for (const duration of expected) {
        h.selector.reportFailure(A);
        h.fetch.mockClear();

        await h.advance(duration - 1);
        await h.settle(h.selector.select());
        expect(h.probed()).not.toContain(A);

        await h.advance(1);
        await h.settle(h.selector.select());
        expect(h.probed()).toContain(A);
      }
    });

    it('applies the +20 % jitter to the block duration', async () => {
      // A random value of 1 is the upper end of the jitter range: 30 s * 1.2 = 36 s.
      const h = setup([A, B], 1);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok' });
      await h.settle(h.selector.select());

      h.selector.reportFailure(A);
      h.fetch.mockClear();
      await h.advance(36_000 - 1);
      await h.settle(h.selector.select());
      expect(h.probed()).not.toContain(A);

      await h.advance(1);
      await h.settle(h.selector.select());
      expect(h.probed()).toContain(A);
    });

    it('resets the backoff after a success', async () => {
      const h = setup([A, B]);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok' });
      await h.settle(h.selector.select());

      h.selector.reportFailure(A);
      h.selector.reportFailure(A);
      h.selector.reportSuccess(A);
      h.fetch.mockClear();

      await h.advance(BLOCK_BASE_MS);
      await h.settle(h.selector.select());
      h.selector.reportFailure(A);
      h.fetch.mockClear();

      // Counted from one again: 30 s, not 60 s.
      await h.advance(BLOCK_BASE_MS);
      await h.settle(h.selector.select());
      expect(h.probed()).toContain(A);
    });
  });

  describe('reportFailure', () => {
    it('switches to the next configured node when the current one fails', async () => {
      const h = setup([A, B, C]);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok', stale: true });
      h.health(C, { status: 'ok' });
      await h.settle(h.selector.select());
      expect(h.selector.current()).toBe(A);

      // The next configured node takes over at once, then the background check settles on a healthy one.
      expect(h.selector.reportFailure(A)).toBe(B);
      expect(h.selector.current()).toBe(B);
      await h.settle(h.selector.select());

      expect(h.selector.current()).toBe(C);
      expect(h.selector.switchedAt()).not.toBeNull();
    });

    it('only blocks a failing node that is not current', async () => {
      const h = setup([A, B]);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok' });
      await h.settle(h.selector.select());

      expect(h.selector.reportFailure(B)).toBe(A);
      expect(h.selector.current()).toBe(A);
    });

    it('ignores origins that are not in the node list', async () => {
      const h = setup([A]);
      h.health(A, { status: 'ok' });
      await h.settle(h.selector.select());

      expect(h.selector.reportFailure(Z)).toBe(A);
    });

    it('is a no-op without configured nodes', () => {
      const h = setup([]);

      expect(h.selector.reportFailure(A)).toBe('');
      expect(h.fetch).not.toHaveBeenCalled();
    });
  });

  describe('discovery', () => {
    it('narrows the node list and never adds nodes', async () => {
      const h = setup([A, B]);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok' });
      h.discovery(A, {
        nodes: [
          { id: '1', url: A },
          { id: '2', url: Z },
        ],
      });

      await h.settle(h.selector.select());

      expect(h.selector.nodes()).toEqual([A]);
    });

    it('keeps the configured list when discovery returns nothing usable', async () => {
      const h = setup([A, B]);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok' });
      h.discovery(A, { nodes: [{ id: '1', url: Z }] });

      await h.settle(h.selector.select());

      expect(h.selector.nodes()).toEqual([A, B]);
    });

    it('keeps the configured list when discovery fails', async () => {
      const h = setup([A, B]);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok' });
      h.routes.set(A + '/api/v1/cluster/nodes', {
        kind: 'reject',
        error: new TypeError('network'),
      });

      await h.settle(h.selector.select());

      expect(h.selector.nodes()).toEqual([A, B]);
    });

    it('moves away from a node that discovery removed', async () => {
      const h = setup([A, B]);
      h.routes.set(A + '/api/v1/health', healthy(10));
      h.routes.set(B + '/api/v1/health', healthy(50));
      h.discovery(A, { nodes: [{ id: '2', url: B }] });

      await h.settle(h.selector.select());

      expect(h.selector.nodes()).toEqual([B]);
      expect(h.selector.current()).toBe(B);
    });

    it('repeats discovery every 5 min', async () => {
      const h = setup([A, B]);
      h.health(A, { status: 'ok' });
      h.health(B, { status: 'ok' });
      await h.settle(h.selector.select());
      h.discovery(A, { nodes: [{ id: '1', url: A }] });
      h.fetch.mockClear();

      await h.advance(DISCOVERY_INTERVAL_MS);

      expect(h.fetch).toHaveBeenCalledWith(A + '/api/v1/cluster/nodes', expect.anything());
      expect(h.selector.nodes()).toEqual([A]);
    });
  });

  describe('background re-check', () => {
    it('checks every 60 s while the tab is visible', async () => {
      const h = setup([A]);
      h.health(A, { status: 'ok' });
      await h.settle(h.selector.select());
      h.fetch.mockClear();

      await h.advance(HEALTH_RECHECK_MS);

      expect(h.probed()).toEqual([A]);
    });

    it('does not check while the tab is hidden', async () => {
      const h = setup([A]);
      h.health(A, { status: 'ok' });
      await h.settle(h.selector.select());
      h.fetch.mockClear();
      setVisibility('hidden');

      await h.advance(HEALTH_RECHECK_MS * 3);

      expect(h.probed()).toEqual([]);
    });
  });

  describe('same-origin mode', () => {
    it('makes no requests and selects no node without configured nodes', async () => {
      const h = setup([]);

      await h.settle(h.selector.select());
      await h.advance(DISCOVERY_INTERVAL_MS * 2);

      expect(h.selector.current()).toBe('');
      expect(h.selector.nodes()).toEqual([]);
      expect(h.fetch).not.toHaveBeenCalled();
    });
  });
});
