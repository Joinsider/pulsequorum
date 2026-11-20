// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { AbstractSecurityStorage } from 'angular-auth-oidc-client';
import { describe, expect, it } from 'vitest';
import { appConfig } from './app.config';
import { SplitSecurityStorage } from './core/auth/split-security-storage';

describe('appConfig', () => {
  it('resolves the library storage to SplitSecurityStorage (NFA-SEC-04)', () => {
    TestBed.configureTestingModule({
      providers: [...appConfig.providers, provideHttpClientTesting()],
    });

    expect(TestBed.inject(AbstractSecurityStorage)).toBeInstanceOf(SplitSecurityStorage);
  });
});
