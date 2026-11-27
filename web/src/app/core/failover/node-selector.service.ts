// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { DestroyRef, Injectable, InjectionToken, inject, signal } from '@angular/core';
import { ConfigService } from '../config/config.service';
import { normalizeOrigin, normalizeOrigins, trustedNodes } from './node-origins';

/** The subset of `fetch` the selector uses; tests replace it through {@link FETCH}. */
export type FetchFn = (input: string, init?: RequestInit) => Promise<Response>;

/** Fetch implementation for health checks and discovery. */
export const FETCH = new InjectionToken<FetchFn>('FETCH', {
  providedIn: 'root',
  factory: () => globalThis.fetch.bind(globalThis),
});

/** Monotonic clock in milliseconds, used for latencies and block-list times. */
export const NODE_CLOCK = new InjectionToken<() => number>('NODE_CLOCK', {
  providedIn: 'root',
  factory: () => () => performance.now(),
});

/** Random numbers in [0, 1) for the block-list jitter. */
export const NODE_RANDOM = new InjectionToken<() => number>('NODE_RANDOM', {
  providedIn: 'root',
  factory: () => () => Math.random(),
});

export const HEALTH_PATH = '/api/v1/health';
export const DISCOVERY_PATH = '/api/v1/cluster/nodes';
export const HEALTH_TIMEOUT_MS = 2_000;
export const HEALTH_RECHECK_MS = 60_000;
export const DISCOVERY_INTERVAL_MS = 5 * 60_000;
export const BLOCK_BASE_MS = 30_000;
export const BLOCK_MAX_MS = 5 * 60_000;
/** Jitter of the block duration, +/- this fraction (ADR 0036, question 2). */
const BLOCK_JITTER = 0.2;
/** A healthy current node keeps its place while it is within this margin of the fastest. */
const STICKY_TOLERANCE = 0.2;

interface Probe {
  origin: string;
  latency: number;
}

/**
 * Chooses the voter node the SPA talks to (ADR 0036, question 2, option 2B):
 * a parallel health check at start, the fastest healthy node, sticky until it
 * fails. Failed nodes are blocked with exponential backoff. The node list can
 * only be narrowed by discovery, never widened (`trustedNodes`).
 *
 * With no configured nodes the SPA uses the page's own origin: no checks, no
 * discovery, no failover.
 */
@Injectable({ providedIn: 'root' })
export class NodeSelector {
  private readonly config = inject(ConfigService);
  private readonly fetchFn = inject(FETCH);
  private readonly clock = inject(NODE_CLOCK);
  private readonly random = inject(NODE_RANDOM);

  /** Node list from the last successful discovery; `null` means the configured list. */
  private discovered: string[] | null = null;
  private inFlight: Promise<void> | null = null;
  private lastCheck = Number.NEGATIVE_INFINITY;
  private timersStarted = false;
  private recheckTimer: ReturnType<typeof setInterval> | undefined;
  private discoveryTimer: ReturnType<typeof setInterval> | undefined;
  private readonly failures = new Map<string, number>();
  private readonly blockedUntil = new Map<string, number>();

  private readonly currentSignal = signal('');
  private readonly usableSignal = signal<string[]>([]);
  private readonly switchedAtSignal = signal<number | null>(null);

  /** Selected node origin, or `''` for same-origin mode. */
  readonly current = this.currentSignal.asReadonly();
  /** Nodes the SPA may use: the configured list, narrowed by discovery. */
  readonly nodes = this.usableSignal.asReadonly();
  /** Time (ms since epoch) of the last automatic switch, for a UI indicator. */
  readonly switchedAt = this.switchedAtSignal.asReadonly();

  constructor() {
    inject(DestroyRef).onDestroy(() => this.stopTimers());
  }

  /**
   * Checks all usable nodes, selects one and loads the node list. Called once
   * after the configuration loaded. Never rejects.
   */
  select(): Promise<void> {
    return this.run(() => this.selectNode());
  }

  /** Reports a failed request to `origin`; returns the origin to use from now on. */
  reportFailure(origin: string): string {
    const node = normalizeOrigin(origin);
    if (node === null || !this.usable().includes(node)) {
      return this.current();
    }
    const now = this.clock();
    const count = (this.failures.get(node) ?? 0) + 1;
    this.failures.set(node, count);
    this.blockedUntil.set(node, now + this.blockDuration(count));

    if (this.current() === node) {
      const next = this.nextNode(node, now);
      if (next !== null) {
        this.setCurrent(next);
      }
      void this.select();
    }
    return this.current();
  }

  /** Clears the failure count and block of `origin` after a successful request. */
  reportSuccess(origin: string): void {
    const node = normalizeOrigin(origin);
    if (node === null) {
      return;
    }
    this.failures.delete(node);
    this.blockedUntil.delete(node);
  }

  private async selectNode(): Promise<void> {
    if (this.configured().length === 0) {
      this.setCurrent('');
      this.usableSignal.set([]);
      return;
    }
    this.startTimers();
    this.usableSignal.set(this.usable());
    await this.chooseNode();
    await this.discoverAndRepair();
  }

  /** Health check of all usable, non-blocked nodes; keeps or changes the current node. */
  private async chooseNode(): Promise<void> {
    const started = this.clock();
    this.lastCheck = started;
    const candidates = this.usable().filter((origin) => !this.isBlocked(origin, started));
    const probes = await Promise.all(candidates.map((origin) => this.probe(origin)));

    // Re-read the clock and the block list: a failure may have been reported while we waited.
    const now = this.clock();
    const healthy = probes
      .filter((p): p is Probe => p !== null && !this.isBlocked(p.origin, now))
      .sort((a, b) => a.latency - b.latency);
    if (healthy.length === 0) {
      this.setCurrent(this.fallback(now));
      return;
    }

    const fastest = healthy[0];
    const kept = healthy.find((p) => p.origin === this.current());
    if (kept !== undefined && kept.latency <= fastest.latency * (1 + STICKY_TOLERANCE)) {
      return;
    }
    this.setCurrent(fastest.origin);
  }

  /** Healthy means HTTP 200, a JSON object, and neither `degraded` nor `stale` set. */
  private async probe(origin: string): Promise<Probe | null> {
    const started = this.clock();
    try {
      const res = await this.fetchFn(origin + HEALTH_PATH, requestInit());
      if (res.status !== 200) {
        return null;
      }
      const body: unknown = await res.json();
      if (!isRecord(body) || body['degraded'] === true || body['stale'] === true) {
        return null;
      }
      return { origin, latency: this.clock() - started };
    } catch {
      return null;
    }
  }

  /** Loads the node list from the current node and repairs the selection if it dropped out. */
  private async discoverAndRepair(): Promise<void> {
    const origin = this.current();
    if (origin !== '') {
      await this.discover(origin);
    }
    if (!this.usable().includes(this.current())) {
      await this.chooseNode();
    }
  }

  private async discover(origin: string): Promise<void> {
    let urls: string[] = [];
    try {
      const res = await this.fetchFn(origin + DISCOVERY_PATH, requestInit());
      if (res.ok) {
        urls = discoveredUrls(await res.json());
      }
    } catch {
      urls = [];
    }
    // Discovery narrows the configured list; an empty or failed result keeps it.
    const trusted = trustedNodes(this.configured(), urls);
    this.discovered = trusted.length > 0 ? trusted : null;
    this.usableSignal.set(this.usable());
  }

  private startTimers(): void {
    if (this.timersStarted) {
      return;
    }
    this.timersStarted = true;
    this.recheckTimer = setInterval(() => {
      if (isVisible()) {
        void this.run(() => this.chooseNode());
      }
    }, HEALTH_RECHECK_MS);
    this.discoveryTimer = setInterval(() => {
      void this.run(() => this.discoverAndRepair());
    }, DISCOVERY_INTERVAL_MS);
    document.addEventListener('visibilitychange', this.onVisibilityChange);
  }

  private stopTimers(): void {
    clearInterval(this.recheckTimer);
    clearInterval(this.discoveryTimer);
    this.recheckTimer = undefined;
    this.discoveryTimer = undefined;
    if (this.timersStarted) {
      document.removeEventListener('visibilitychange', this.onVisibilityChange);
    }
    this.timersStarted = false;
  }

  /** A tab that becomes visible again re-checks at once if the last check is old. */
  private readonly onVisibilityChange = (): void => {
    if (isVisible() && this.clock() - this.lastCheck >= HEALTH_RECHECK_MS) {
      void this.run(() => this.chooseNode());
    }
  };

  /** Runs one task at a time; concurrent callers share the running one. */
  private run(task: () => Promise<void>): Promise<void> {
    this.inFlight ??= task().finally(() => {
      this.inFlight = null;
    });
    return this.inFlight;
  }

  private configured(): string[] {
    return normalizeOrigins(this.config.config().nodes);
  }

  private usable(): string[] {
    return this.discovered ?? this.configured();
  }

  private isBlocked(origin: string, now: number): boolean {
    return (this.blockedUntil.get(origin) ?? Number.NEGATIVE_INFINITY) > now;
  }

  /** Node to use when no node answered the health check: the first one not blocked. */
  private fallback(now: number): string {
    const usable = this.usable();
    return usable.find((origin) => !this.isBlocked(origin, now)) ?? usable[0] ?? '';
  }

  /** Next node after `exclude` in configuration order, preferring unblocked ones. */
  private nextNode(exclude: string, now: number): string | null {
    const usable = this.usable();
    const start = usable.indexOf(exclude);
    const ordered = [...usable.slice(start + 1), ...usable.slice(0, start)];
    const free = ordered.find((origin) => !this.isBlocked(origin, now));
    if (free !== undefined) {
      return free;
    }
    // All other nodes are blocked: take the one that unblocks first.
    let best: string | null = null;
    for (const origin of ordered) {
      if (best === null || this.blockedUntil.get(origin)! < this.blockedUntil.get(best)!) {
        best = origin;
      }
    }
    return best;
  }

  /** 30 s doubling per consecutive failure, capped at 5 min, with +/- 20 % jitter. */
  private blockDuration(count: number): number {
    const base = Math.min(BLOCK_MAX_MS, BLOCK_BASE_MS * 2 ** (count - 1));
    const jitter = 1 + (this.random() * 2 - 1) * BLOCK_JITTER;
    return Math.min(BLOCK_MAX_MS, Math.round(base * jitter));
  }

  private setCurrent(origin: string): void {
    const previous = this.currentSignal();
    if (previous === origin) {
      return;
    }
    this.currentSignal.set(origin);
    // The first selection is not a switch.
    if (previous !== '') {
      this.switchedAtSignal.set(Date.now());
    }
  }
}

function requestInit(): RequestInit {
  return {
    credentials: 'omit',
    cache: 'no-store',
    signal: AbortSignal.timeout(HEALTH_TIMEOUT_MS),
  };
}

function isVisible(): boolean {
  return document.visibilityState === 'visible';
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** The `url` fields of a `/api/v1/cluster/nodes` response; anything else yields nothing. */
function discoveredUrls(body: unknown): string[] {
  if (!isRecord(body) || !Array.isArray(body['nodes'])) {
    return [];
  }
  const urls: string[] = [];
  for (const node of body['nodes'] as unknown[]) {
    if (isRecord(node) && typeof node['url'] === 'string') {
      urls.push(node['url']);
    }
  }
  return urls;
}
