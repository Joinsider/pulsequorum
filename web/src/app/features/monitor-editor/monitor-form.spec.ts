// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { FormGroup } from '@angular/forms';
import { describe, expect, it } from 'vitest';
import { Monitor } from '../../api/gen/model';
import {
  TAG_LENGTH,
  TAG_LIMIT,
  createMonitorForm,
  monitorFormValue,
  monitorInputOf,
  newRequestKey,
  replaceConfig,
} from './monitor-form';
import { JsonSchema } from './schema-form';

const HTTP_SCHEMA: JsonSchema = {
  type: 'object',
  additionalProperties: false,
  required: ['url'],
  properties: {
    url: { type: 'string', format: 'uri' },
    method: { type: 'string', enum: ['GET', 'POST'], default: 'GET' },
    maxRedirects: { type: 'integer', minimum: 0, maximum: 30, default: 10 },
  },
};

const MONITOR: Monitor = {
  id: '6f1c7a2e-0000-4000-8000-000000000001',
  name: 'Shop',
  description: 'Front page',
  type: 'http',
  config: { url: 'https://shop.example', method: 'POST' },
  intervalSeconds: 60,
  timeoutSeconds: 30,
  retries: 2,
  recoveryThreshold: 1,
  resendIntervalMinutes: 0,
  assignment: { mode: 'count', count: 2 },
  consensusPolicy: 'any',
  tags: ['prod', 'eu'],
  notificationChannelIds: ['c1', 'c2'],
  paused: false,
  status: 'UP',
  statusSince: '2026-10-01T10:00:00Z',
  createdAt: '2026-10-01T10:00:00Z',
  updatedAt: '2026-10-01T10:00:00Z',
};

/** A form for the HTTP type with the given common values, config built from the schema. */
function filled(values: Partial<Monitor> = {}, config?: unknown): FormGroup {
  const form = createMonitorForm();
  form.patchValue(monitorFormValue({ ...MONITOR, ...values }));
  replaceConfig(form, HTTP_SCHEMA, config ?? MONITOR.config);
  return form;
}

describe('monitorFormValue', () => {
  it('uses the create defaults for an empty input', () => {
    expect(monitorFormValue({})).toEqual({
      name: '',
      description: '',
      type: '',
      intervalSeconds: 60,
      timeoutSeconds: null,
      retries: 1,
      recoveryThreshold: 1,
      resendIntervalMinutes: 0,
      paused: false,
      tags: '',
      notificationChannelIds: [],
      assignmentMode: 'all',
      assignmentCount: null,
      assignmentNodes: '',
      consensusPolicy: 'majority',
    });
  });

  it('turns lists into comma-separated text for the form', () => {
    const value = monitorFormValue(MONITOR);
    expect(value['tags']).toBe('prod, eu');
    expect(value['assignmentMode']).toBe('count');
    expect(value['assignmentCount']).toBe(2);
  });
});

describe('monitorInputOf', () => {
  it('builds the full PUT body including the notification channels', () => {
    const form = filled();
    const input = monitorInputOf(form, HTTP_SCHEMA);

    expect(input).toEqual({
      name: 'Shop',
      description: 'Front page',
      type: 'http',
      config: { url: 'https://shop.example', method: 'POST', maxRedirects: 10 },
      intervalSeconds: 60,
      timeoutSeconds: 30,
      retries: 2,
      recoveryThreshold: 1,
      resendIntervalMinutes: 0,
      paused: false,
      assignment: { mode: 'count', count: 2 },
      consensusPolicy: 'any',
      tags: ['prod', 'eu'],
      notificationChannelIds: ['c1', 'c2'],
    });
  });

  it('sends an empty channel list so that a PUT clears the assignment', () => {
    const input = monitorInputOf(filled({ notificationChannelIds: [] }), HTTP_SCHEMA);
    expect(input.notificationChannelIds).toEqual([]);
  });

  it('omits empty optional fields and sends assignment all by default', () => {
    const input = monitorInputOf(
      filled({ description: '  ', timeoutSeconds: undefined, assignment: undefined, tags: [] }),
      HTTP_SCHEMA,
    );

    expect(input).not.toHaveProperty('description');
    expect(input).not.toHaveProperty('timeoutSeconds');
    expect(input.assignment).toEqual({ mode: 'all' });
    expect(input.tags).toEqual([]);
  });

  it('sends the node list for mode nodes', () => {
    const form = filled({ assignment: { mode: 'nodes', nodes: ['node-a'] } });
    form.controls['assignmentNodes'].setValue('node-a, node-b');
    expect(monitorInputOf(form, HTTP_SCHEMA).assignment).toEqual({
      mode: 'nodes',
      nodes: ['node-a', 'node-b'],
    });
  });

  it('trims the name', () => {
    expect(monitorInputOf(filled({ name: '  Shop  ' }), HTTP_SCHEMA).name).toBe('Shop');
  });
});

describe('form validation', () => {
  it('requires a name and an interval of at least 20 seconds', () => {
    const form = filled({ name: '' });
    expect(form.controls['name'].hasError('required')).toBe(true);

    form.controls['intervalSeconds'].setValue(19);
    expect(form.controls['intervalSeconds'].hasError('min')).toBe(true);
    form.controls['intervalSeconds'].setValue(86401);
    expect(form.controls['intervalSeconds'].hasError('max')).toBe(true);
    form.controls['intervalSeconds'].setValue(20);
    expect(form.controls['intervalSeconds'].valid).toBe(true);
  });

  it('limits the timeout to 80 % of the interval and re-checks it when the interval changes', () => {
    const form = filled({ intervalSeconds: 60, timeoutSeconds: 48 });
    expect(form.controls['timeoutSeconds'].valid).toBe(true);

    form.controls['timeoutSeconds'].setValue(49);
    expect(form.controls['timeoutSeconds'].errors).toEqual({ timeoutLimit: { max: 48 } });

    form.controls['intervalSeconds'].setValue(100);
    expect(form.controls['timeoutSeconds'].valid).toBe(true);
  });

  it('checks the timeout on creation with a prefilled interval', () => {
    const form = createMonitorForm();
    form.patchValue({ intervalSeconds: 20, timeoutSeconds: 17 });
    expect(form.controls['timeoutSeconds'].hasError('timeoutLimit')).toBe(true);
  });

  it('keeps retries within 0 to 10', () => {
    const form = filled();
    form.controls['retries'].setValue(11);
    expect(form.controls['retries'].hasError('max')).toBe(true);
    form.controls['retries'].setValue(10);
    expect(form.controls['retries'].valid).toBe(true);
  });

  it('limits tags by count and length', () => {
    const form = filled();
    const many = Array.from({ length: TAG_LIMIT + 1 }, (_, i) => `t${i}`).join(',');
    form.controls['tags'].setValue(many);
    expect(form.controls['tags'].hasError('tagCount')).toBe(true);

    form.controls['tags'].setValue('x'.repeat(TAG_LENGTH + 1));
    expect(form.controls['tags'].hasError('tagLength')).toBe(true);

    form.controls['tags'].setValue('prod, eu');
    expect(form.controls['tags'].valid).toBe(true);
  });

  it('requires a node count for mode count and node IDs for mode nodes', () => {
    const form = filled({ assignment: { mode: 'all' } });
    form.controls['assignmentMode'].setValue('count');
    expect(form.controls['assignmentCount'].hasError('countRequired')).toBe(true);

    form.controls['assignmentMode'].setValue('nodes');
    expect(form.controls['assignmentNodes'].hasError('nodesRequired')).toBe(true);

    form.controls['assignmentNodes'].setValue('node-a');
    expect(form.controls['assignmentNodes'].valid).toBe(true);

    form.controls['assignmentMode'].setValue('all');
    expect(form.controls['assignmentNodes'].valid).toBe(true);
    expect(form.controls['assignmentCount'].valid).toBe(true);
  });

  it('marks the config controls of the type schema as invalid', () => {
    const form = filled({}, { url: '' });
    expect((form.controls['config'] as FormGroup).controls['url'].hasError('required')).toBe(true);
  });
});

describe('newRequestKey', () => {
  it('returns a version 4 UUID', () => {
    expect(newRequestKey()).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    );
  });

  it('returns a different key on every call', () => {
    expect(newRequestKey()).not.toBe(newRequestKey());
  });
});
