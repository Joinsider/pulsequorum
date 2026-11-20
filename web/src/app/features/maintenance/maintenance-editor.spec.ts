// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { FormGroup } from '@angular/forms';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MaintenanceEditor } from './maintenance-editor';

/** Internal members are read through bracket access, the way the template reaches them. */
type EditorInternals = {
  form: FormGroup;
  save: () => Promise<void>;
};

describe('MaintenanceEditor', () => {
  let fixture: ComponentFixture<MaintenanceEditor>;
  let http: HttpTestingController;
  let editor: EditorInternals;

  /** Lets the pending monitor request resolve and renders the form. */
  async function settle(): Promise<void> {
    await new Promise((resolve) => setTimeout(resolve));
    fixture.detectChanges();
  }

  beforeEach(async () => {
    await TestBed.configureTestingModule({
      imports: [
        MaintenanceEditor,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    }).compileComponents();
    fixture = TestBed.createComponent(MaintenanceEditor);
    http = TestBed.inject(HttpTestingController);
    fixture.detectChanges();
    http.expectOne((r) => r.url === '/api/v1/monitors').flush({ items: [], nextCursor: null });
    await settle();
    editor = fixture.componentInstance as unknown as EditorInternals;
  });

  afterEach(() => {
    http.verify();
  });

  it('writes the cron expression of a weekly preset and follows its inputs', () => {
    const f = editor.form;
    f.controls['kind'].setValue('recurring');
    f.controls['preset'].setValue('weekly');
    expect(f.controls['cron'].value).toBe('0 3 * * 0');
    f.controls['time'].setValue('03:30');
    f.controls['weekday'].setValue(2);
    expect(f.controls['cron'].value).toBe('30 3 * * 2');
  });

  it('switches the preset to custom when the cron expression is edited by hand', () => {
    const f = editor.form;
    f.controls['kind'].setValue('recurring');
    f.controls['preset'].setValue('daily');
    f.controls['cron'].setValue('*/15 * * * *');
    expect(f.controls['preset'].value).toBe('custom');
    expect(f.controls['cron'].valid).toBe(true);
  });

  it('flags an invalid cron expression and a duration out of range', () => {
    const f = editor.form;
    f.controls['kind'].setValue('recurring');
    f.controls['preset'].setValue('custom');
    f.controls['cron'].setValue('61 * * * *');
    f.controls['durationMinutes'].setValue(0);
    expect(f.controls['cron'].errors).toEqual({ cron: true });
    expect(f.controls['durationMinutes'].errors).toEqual({ range: true });
  });

  it('rejects a one-time window whose end is not after its start, and does not submit', async () => {
    const f = editor.form;
    f.controls['name'].setValue('Patch night');
    f.controls['startsAt'].setValue('2026-07-01T10:00');
    f.controls['endsAt'].setValue('2026-07-01T09:00');
    expect(f.controls['endsAt'].errors).toEqual({ order: true });
    await editor.save();
    http.expectNone((r) => r.method === 'POST');
  });

  it('maps a 422 field error of the schedule onto the flat control', async () => {
    const f = editor.form;
    f.controls['name'].setValue('Patch night');
    f.controls['startsAt'].setValue('2026-07-01T10:00');
    f.controls['endsAt'].setValue('2026-07-01T12:00');
    const saving = editor.save();
    http
      .expectOne((r) => r.method === 'POST' && r.url === '/api/v1/maintenance')
      .flush(
        {
          type: 'about:blank',
          title: 'Validation Failed',
          status: 422,
          errors: [{ field: '/schedule/timezone', message: 'unknown time zone Mars/Base' }],
        },
        { status: 422, statusText: 'Unprocessable Entity' },
      );
    await saving;
    expect(f.controls['timezone'].errors).toEqual({ server: 'unknown time zone Mars/Base' });
  });
});
