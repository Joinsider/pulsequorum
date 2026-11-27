// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpClient, provideHttpClient, withInterceptors } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, describe, expect, it } from 'vitest';
import { NodeSelector } from '../failover/node-selector.service';
import { apiBaseUrlInterceptor } from './api-base-url.interceptor';

function setup(baseUrl: string): { http: HttpClient; controller: HttpTestingController } {
  TestBed.configureTestingModule({
    providers: [
      { provide: NodeSelector, useValue: { current: () => baseUrl } },
      provideHttpClient(withInterceptors([apiBaseUrlInterceptor])),
      provideHttpClientTesting(),
    ],
  });
  return { http: TestBed.inject(HttpClient), controller: TestBed.inject(HttpTestingController) };
}

describe('apiBaseUrlInterceptor', () => {
  afterEach(() => TestBed.resetTestingModule());

  it('prefixes API calls with the configured node', () => {
    const { http, controller } = setup('https://mon1.example.com');
    http.get('/api/v1/health').subscribe();
    const req = controller.expectOne('https://mon1.example.com/api/v1/health');
    expect(req.request.method).toBe('GET');
    req.flush({});
    controller.verify();
  });

  it('keeps API calls same-origin without configured nodes', () => {
    const { http, controller } = setup('');
    http.get('/api/v1/health').subscribe();
    const req = controller.expectOne('/api/v1/health');
    expect(req.request.method).toBe('GET');
    req.flush({});
    controller.verify();
  });

  it('does not touch non-API URLs', () => {
    const { http, controller } = setup('https://mon1.example.com');
    http.get('/config.json').subscribe();
    const req = controller.expectOne('/config.json');
    expect(req.request.method).toBe('GET');
    req.flush({});
    controller.verify();
  });
});
