// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfigService } from './config.service';

describe('ConfigService', () => {
  let service: ConfigService;
  let httpMock: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    service = TestBed.inject(ConfigService);
    httpMock = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    httpMock.verify();
    vi.restoreAllMocks();
  });

  it('loads the configuration from /config.json', async () => {
    const loading = service.load();
    httpMock.expectOne('/config.json').flush({
      nodes: ['http://node-a:8080'],
      auth: {
        mode: 'oidc',
        issuer: 'https://id.example.test',
        clientId: 'uptime',
        scopes: ['openid', 'profile', 'offline_access'],
      },
    });
    await loading;

    expect(service.config()).toEqual({
      nodes: ['http://node-a:8080'],
      auth: {
        mode: 'oidc',
        issuer: 'https://id.example.test',
        clientId: 'uptime',
        scopes: ['openid', 'profile', 'offline_access'],
      },
    });
  });

  it('falls back to a local-only configuration and warns when loading fails', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);

    const loading = service.load();
    httpMock.expectOne('/config.json').flush('not found', { status: 404, statusText: 'Not Found' });
    await loading;

    expect(service.config()).toEqual({ nodes: [], auth: { mode: 'local' } });
    expect(warn).toHaveBeenCalled();
  });

  it('returns an empty API base URL for an empty node list', async () => {
    const loading = service.load();
    httpMock.expectOne('/config.json').flush({ nodes: [], auth: { mode: 'local' } });
    await loading;

    expect(service.apiBaseUrl()).toBe('');
  });

  it('returns the first node without trailing slashes as API base URL', async () => {
    const loading = service.load();
    httpMock.expectOne('/config.json').flush({
      nodes: ['http://node-a:8080//', 'http://node-b:8080'],
      auth: { mode: 'local' },
    });
    await loading;

    expect(service.apiBaseUrl()).toBe('http://node-a:8080');
  });
});
