// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  HttpClient,
  HttpHandlerFn,
  HttpRequest,
  provideHttpClient,
  withInterceptors,
} from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { apiBaseUrlInterceptor } from '../api/api-base-url.interceptor';
import { NodeSelector } from './node-selector.service';
import { FAILOVER_RETRIED, failoverInterceptor, retryAfterMs } from './failover.interceptor';

const N1 = 'https://n1.example';
const N2 = 'https://n2.example';

/**
 * Stands in for the auth interceptor: adds a fresh bearer token to relative
 * API calls, and keeps an existing header only if the request already has one.
 */
const fakeAuth = (req: HttpRequest<unknown>, next: HttpHandlerFn) => {
  if (!req.url.startsWith('/api/') || req.headers.has('Authorization')) {
    return next(req);
  }
  return next(req.clone({ setHeaders: { Authorization: 'Bearer fresh' } }));
};

/** A selector that switches to the other node on every reported failure. */
function fakeSelector() {
  const current = signal(N1);
  const nodes = signal([N1, N2]);
  return {
    current,
    nodes,
    reportFailure: vi.fn((origin: string) => {
      if (origin === current()) {
        current.set(current() === N1 ? N2 : N1);
      }
      return current();
    }),
    reportSuccess: vi.fn(),
  };
}

describe('failoverInterceptor', () => {
  let http: HttpClient;
  let ctl: HttpTestingController;
  let selector: ReturnType<typeof fakeSelector>;

  function setup(): void {
    selector = fakeSelector();
    TestBed.configureTestingModule({
      providers: [
        { provide: NodeSelector, useValue: selector },
        provideHttpClient(withInterceptors([fakeAuth, apiBaseUrlInterceptor, failoverInterceptor])),
        provideHttpClientTesting(),
      ],
    });
    http = TestBed.inject(HttpClient);
    ctl = TestBed.inject(HttpTestingController);
  }

  beforeEach(() => setup());

  afterEach(() => {
    try {
      ctl.verify();
    } finally {
      TestBed.resetTestingModule();
      vi.useRealTimers();
    }
  });

  it('repeats a GET once on the next node after a network error', () => {
    let body: unknown;
    http.get('/api/v1/monitors').subscribe((v) => (body = v));

    ctl.expectOne(N1 + '/api/v1/monitors').error(new ProgressEvent('error'));
    ctl.expectOne(N2 + '/api/v1/monitors').flush({ items: [] });

    expect(body).toEqual({ items: [] });
    expect(selector.reportFailure).toHaveBeenCalledWith(N1);
    expect(selector.reportSuccess).toHaveBeenCalledWith(N2);
  });

  it('retries with a fresh token and the relative URL, not the stale one', () => {
    http
      .get('/api/v1/monitors', { headers: { Authorization: 'Bearer stale' } })
      .subscribe({ error: () => undefined });

    ctl.expectOne(N1 + '/api/v1/monitors').error(new ProgressEvent('error'));
    const retry = ctl.expectOne(N2 + '/api/v1/monitors');

    // The stale header was removed; the auth stand-in added a fresh one to the relative URL.
    expect(retry.request.headers.get('Authorization')).toBe('Bearer fresh');
    retry.flush({});
  });

  it('does not repeat a POST without Idempotency-Key, but reports the node', () => {
    let error: unknown;
    http.post('/api/v1/monitors', {}).subscribe({ error: (e) => (error = e) });

    ctl.expectOne(N1 + '/api/v1/monitors').error(new ProgressEvent('error'));

    expect(error).toBeDefined();
    expect(selector.reportFailure).toHaveBeenCalledWith(N1);
  });

  it('repeats a POST with Idempotency-Key using the same key', () => {
    http.post('/api/v1/monitors', {}, { headers: { 'Idempotency-Key': 'k-42' } }).subscribe();

    ctl.expectOne(N1 + '/api/v1/monitors').error(new ProgressEvent('error'));
    const retry = ctl.expectOne(N2 + '/api/v1/monitors');

    expect(retry.request.method).toBe('POST');
    expect(retry.request.headers.get('Idempotency-Key')).toBe('k-42');
    retry.flush({});
  });

  it('never repeats an auth call, even with a key', () => {
    let error: unknown;
    http
      .post('/api/v1/auth/refresh', {}, { headers: { 'Idempotency-Key': 'k-1' } })
      .subscribe({ error: (e) => (error = e) });

    ctl.expectOne(N1 + '/api/v1/auth/refresh').error(new ProgressEvent('error'));

    expect(error).toBeDefined();
    expect(selector.reportFailure).toHaveBeenCalledWith(N1);
  });

  it.each([401, 403, 404, 500])('never repeats a %i response', (status) => {
    let error: unknown;
    http.get('/api/v1/monitors').subscribe({ error: (e) => (error = e) });

    ctl.expectOne(N1 + '/api/v1/monitors').flush(null, { status, statusText: 'x' });

    expect(error).toBeDefined();
    expect(selector.reportFailure).not.toHaveBeenCalled();
  });

  it('does not repeat a second failure', () => {
    let error: unknown;
    http.get('/api/v1/monitors').subscribe({ error: (e) => (error = e) });

    ctl.expectOne(N1 + '/api/v1/monitors').error(new ProgressEvent('error'));
    ctl.expectOne(N2 + '/api/v1/monitors').error(new ProgressEvent('error'));

    expect(error).toBeDefined();
    expect(selector.reportFailure).toHaveBeenCalledTimes(2);
  });

  it('marks the retry so it cannot switch again', () => {
    http.get('/api/v1/monitors').subscribe({ error: () => undefined });

    ctl.expectOne(N1 + '/api/v1/monitors').error(new ProgressEvent('error'));
    const retry = ctl.expectOne(N2 + '/api/v1/monitors');

    expect(retry.request.context.get(FAILOVER_RETRIED)).toBe(true);
    retry.flush({});
  });

  it('honors Retry-After on 503, waiting at most the cap', () => {
    vi.useFakeTimers();
    let body: unknown;
    http.get('/api/v1/monitors').subscribe((v) => (body = v));

    ctl
      .expectOne(N1 + '/api/v1/monitors')
      .flush(null, { status: 503, statusText: 'Unavailable', headers: { 'Retry-After': '2' } });
    ctl.expectNone(N2 + '/api/v1/monitors');

    vi.advanceTimersByTime(1_999);
    ctl.expectNone(N2 + '/api/v1/monitors');

    vi.advanceTimersByTime(1);
    ctl.expectOne(N2 + '/api/v1/monitors').flush({ ok: true });
    expect(body).toEqual({ ok: true });
  });

  it('does not honor Retry-After on other failures', () => {
    http.get('/api/v1/monitors').subscribe({ error: () => undefined });

    ctl
      .expectOne(N1 + '/api/v1/monitors')
      .flush(null, { status: 502, statusText: 'Bad Gateway', headers: { 'Retry-After': '9' } });

    ctl.expectOne(N2 + '/api/v1/monitors').flush({});
  });

  it('does not change a request whose node is not one of the selector nodes', () => {
    selector.nodes.set([N2]);
    let error: unknown;
    http.get('/api/v1/monitors').subscribe({ error: (e) => (error = e) });

    ctl.expectOne(N1 + '/api/v1/monitors').error(new ProgressEvent('error'));

    expect(error).toBeDefined();
    expect(selector.reportFailure).not.toHaveBeenCalled();
  });

  it('keeps same-origin requests as they are', () => {
    selector.current.set('');
    selector.nodes.set([]);
    let error: unknown;
    http.get('/api/v1/monitors').subscribe({ error: (e) => (error = e) });

    ctl.expectOne('/api/v1/monitors').error(new ProgressEvent('error'));

    expect(error).toBeDefined();
    expect(selector.reportFailure).not.toHaveBeenCalled();
  });

  it('does not repeat when the selector keeps the same node', () => {
    selector.reportFailure.mockImplementation(() => N1);
    let error: unknown;
    http.get('/api/v1/monitors').subscribe({ error: (e) => (error = e) });

    ctl.expectOne(N1 + '/api/v1/monitors').error(new ProgressEvent('error'));

    expect(error).toBeDefined();
  });
});

describe('retryAfterMs', () => {
  it.each([
    [null, 0],
    ['', 0],
    ['abc', 0],
    ['2', 2_000],
    ['0', 0],
    ['60', 5_000],
  ])('reads %s as %i ms', (value, expected) => {
    expect(retryAfterMs(value)).toBe(expected);
  });

  it('reads an HTTP date in the future and caps it', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    expect(retryAfterMs(new Date('2026-01-01T00:00:03Z').toUTCString())).toBe(3_000);
    expect(retryAfterMs(new Date('2025-12-31T23:59:00Z').toUTCString())).toBe(0);
    vi.useRealTimers();
  });
});
