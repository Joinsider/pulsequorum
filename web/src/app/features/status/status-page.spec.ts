// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { Observable, of, throwError } from 'rxjs';
import { SystemService } from '../../api/gen/endpoints/system/system.service';
import { Health } from '../../api/gen/model';
import { StatusPage } from './status-page';

const HEALTH: Health = {
  status: 'ok',
  role: 'voter',
  nodeName: 'node-a',
  version: '1.2.3',
  commit: 'abc1234',
  degraded: false,
};

function setup(result: Observable<Health>): ComponentFixture<StatusPage> {
  TestBed.configureTestingModule({
    imports: [StatusPage],
    providers: [{ provide: SystemService, useValue: { getHealth: () => result } }],
  });
  const fixture = TestBed.createComponent(StatusPage);
  fixture.detectChanges();
  return fixture;
}

describe('StatusPage', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('renders the node details on success', () => {
    const fixture = setup(of(HEALTH));
    const text = (fixture.nativeElement as HTMLElement).textContent ?? '';

    expect(text).toContain('node-a');
    expect(text).toContain('voter');
    expect(text).toContain('1.2.3');
    expect(text).toContain('abc1234');
    expect(text).toContain('Status: ok');
    expect(text).not.toContain('Backend not reachable');
  });

  it('renders an error message when the backend is not reachable', () => {
    const fixture = setup(throwError(() => new HttpErrorResponse({ status: 0 })));
    const text = (fixture.nativeElement as HTMLElement).textContent ?? '';

    expect(text).toContain('Backend not reachable');
    expect(text).not.toContain('node-a');
  });
});
