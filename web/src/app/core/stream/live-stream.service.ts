// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { Injectable, effect, inject, signal, untracked } from '@angular/core';
import { Observable, Subject, firstValueFrom } from 'rxjs';
import { StreamService as StreamApi } from '../../api/gen/endpoints/stream/stream.service';
import { StreamEvent, StreamEventType } from '../../api/gen/model';
import { AuthService } from '../auth/auth.service';
import { NodeSelector } from '../failover/node-selector.service';

/** Application close codes of /api/v1/stream (internal/api/stream.go). */
const CLOSE_SESSION_ENDED = 4001;
const CLOSE_FORBIDDEN = 4003;

const BACKOFF_BASE_MS = 1_000;
const BACKOFF_MAX_MS = 30_000;
const EVENT_TYPES: readonly string[] = [
  'heartbeat',
  'state',
  'incident',
  'node',
] satisfies StreamEventType[];

export type StreamState = 'idle' | 'connecting' | 'open' | 'reconnecting';

/**
 * Live updates over WebSocket (FA-API-04, FA-UI-06). The socket is opened with
 * a one-time ticket fetched with the bearer token (FA-AUTH-11), so no
 * long-lived credential ever appears in a URL. Events are pushed into an RxJS
 * stream and signals only (zoneless, PF-40).
 *
 * Each attempt is pinned to the node the {@link NodeSelector} chose when the
 * attempt started: the ticket is issued there and the socket opened there,
 * since tickets are valid on the issuing node only (ADR 0036).
 */
@Injectable({ providedIn: 'root' })
export class LiveStreamService {
  private readonly api = inject(StreamApi);
  private readonly auth = inject(AuthService);
  private readonly selector = inject(NodeSelector);

  private socket: WebSocket | null = null;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private attempt = 0;
  private monitorIds: string[] = [];
  /** Bumped on every (re)connect so stale sockets and timers are ignored. */
  private generation = 0;
  private readonly eventsSubject = new Subject<StreamEvent>();

  private readonly stateSignal = signal<StreamState>('idle');
  private readonly resyncSignal = signal(0);

  /** All events of the caller's tenant, narrowed by {@link setMonitorFilter}. */
  readonly events: Observable<StreamEvent> = this.eventsSubject.asObservable();
  readonly state = this.stateSignal.asReadonly();
  /**
   * Increases after every reconnect. Views reload their data when it changes,
   * because events may have been missed in between (resynchronisation).
   */
  readonly resync = this.resyncSignal.asReadonly();

  constructor() {
    effect(() => {
      const signedIn = this.auth.authenticated();
      untracked(() => (signedIn ? this.start() : this.stop()));
    });
  }

  /** Narrows the stream to these monitors; an empty list means all monitors. */
  setMonitorFilter(ids: readonly string[]): void {
    this.monitorIds = [...ids];
    this.sendFilter();
  }

  private start(): void {
    if (this.socket || this.stateSignal() === 'connecting') {
      return;
    }
    this.attempt = 0;
    void this.connect(++this.generation);
  }

  private stop(): void {
    this.generation++;
    clearTimeout(this.retryTimer);
    const s = this.socket;
    this.socket = null;
    s?.close(1000, 'signed out');
    this.monitorIds = [];
    this.stateSignal.set('idle');
  }

  private async connect(gen: number): Promise<void> {
    this.stateSignal.set(this.attempt === 0 ? 'connecting' : 'reconnecting');
    // The ticket request is relative, so the base-URL interceptor sends it to the
    // node selected right now. Read it synchronously, before the request starts.
    const pinned = this.selector.current();
    let ticket: string;
    try {
      ticket = (await firstValueFrom(this.api.createStreamTicket())).ticket;
    } catch {
      if (gen === this.generation) {
        this.scheduleRetry(gen);
      }
      return;
    }
    if (gen !== this.generation) {
      return;
    }
    if (this.selector.current() !== pinned) {
      // The node changed while the ticket was in flight: the ticket belongs to
      // the old one. Retry with the usual backoff, so a node that keeps
      // changing cannot cause a tight loop.
      this.scheduleRetry(gen);
      return;
    }

    const socket = new WebSocket(streamUrlFor(pinned, ticket));
    this.socket = socket;
    let failureReported = false;
    const reportFailure = (): void => {
      if (!failureReported && pinned !== '') {
        failureReported = true;
        this.selector.reportFailure(pinned);
      }
    };
    socket.onopen = () => {
      if (gen !== this.generation) {
        return;
      }
      if (pinned !== '') {
        this.selector.reportSuccess(pinned);
      }
      const reconnected = this.attempt > 0;
      this.attempt = 0;
      this.stateSignal.set('open');
      this.sendFilter();
      if (reconnected) {
        this.resyncSignal.update((n) => n + 1);
      }
    };
    socket.onmessage = (msg) => {
      if (gen === this.generation) {
        const event = parseEvent(msg.data);
        if (event) {
          this.eventsSubject.next(event);
        }
      }
    };
    socket.onerror = () => {
      if (gen === this.generation) {
        reportFailure();
      }
    };
    socket.onclose = (ev) => {
      if (gen !== this.generation) {
        return;
      }
      this.socket = null;
      if (ev.code !== CLOSE_SESSION_ENDED && ev.code !== CLOSE_FORBIDDEN) {
        reportFailure();
      }
      void this.handleClose(gen, ev.code);
    };
  }

  private async handleClose(gen: number, code: number): Promise<void> {
    if (code === CLOSE_FORBIDDEN) {
      await this.auth.sessionExpired();
      return;
    }
    if (code === CLOSE_SESSION_ENDED) {
      // The access token behind the ticket expired; continue with a fresh one.
      if (!(await this.auth.refresh())) {
        await this.auth.sessionExpired();
        return;
      }
    }
    if (gen === this.generation) {
      this.scheduleRetry(gen);
    }
  }

  /** Exponential backoff with full jitter, so nodes are not hit in lockstep. */
  private scheduleRetry(gen: number): void {
    this.stateSignal.set('reconnecting');
    const cap = Math.min(BACKOFF_MAX_MS, BACKOFF_BASE_MS * 2 ** this.attempt);
    this.attempt++;
    clearTimeout(this.retryTimer);
    this.retryTimer = setTimeout(() => {
      if (gen === this.generation && this.auth.authenticated()) {
        void this.connect(gen);
      }
    }, Math.random() * cap);
  }

  private sendFilter(): void {
    if (this.socket?.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify({ monitorIds: this.monitorIds }));
    }
  }
}

/**
 * WebSocket URL of the stream on `origin` (`https` becomes `wss`, `http` becomes
 * `ws`). An empty origin means the page's own origin.
 */
export function streamUrlFor(origin: string, ticket: string): string {
  const url = new URL('/api/v1/stream', origin || window.location.origin);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  url.searchParams.set('ticket', ticket);
  return url.toString();
}

/** Accepts only well-formed events; anything else is dropped. */
export function parseEvent(data: unknown): StreamEvent | null {
  if (typeof data !== 'string') {
    return null;
  }
  try {
    const v: unknown = JSON.parse(data);
    if (
      typeof v === 'object' &&
      v !== null &&
      EVENT_TYPES.includes((v as StreamEvent).type) &&
      typeof (v as StreamEvent).ts === 'string'
    ) {
      return v as StreamEvent;
    }
  } catch {
    // ignore malformed messages
  }
  return null;
}
