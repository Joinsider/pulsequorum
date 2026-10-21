// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import {
  AbstractControl,
  FormControl,
  FormGroup,
  ValidationErrors,
  ValidatorFn,
  Validators,
} from '@angular/forms';
import {
  Assignment,
  AssignmentMode,
  ConsensusPolicy,
  MonitorInput,
  MonitorInputConfig,
} from '../../api/gen/model';
import { JsonSchema, buildControl, controlToValue, parseList } from './schema-form';

/** Limits of MonitorInput (api/openapi.yaml). */
export const TAG_LIMIT = 20;
export const TAG_LENGTH = 64;
/** FA-MON-03: the timeout must not exceed this share of the interval. */
export const TIMEOUT_SHARE = 0.8;

/**
 * Builds the common monitor form. The `config` group is a placeholder until
 * the type's schema is known; use {@link replaceConfig} to install it.
 * Call {@link monitorFormValue} + `patchValue` to fill the values.
 */
export function createMonitorForm(): FormGroup {
  const form = new FormGroup({
    name: new FormControl('', {
      nonNullable: true,
      validators: [Validators.required, Validators.maxLength(200)],
    }),
    description: new FormControl('', {
      nonNullable: true,
      validators: [Validators.maxLength(2000)],
    }),
    type: new FormControl('', { nonNullable: true, validators: [Validators.required] }),
    intervalSeconds: new FormControl<number | null>(null, [
      Validators.required,
      integer,
      Validators.min(20),
      Validators.max(86400),
    ]),
    timeoutSeconds: new FormControl<number | null>(null, [
      integer,
      Validators.min(1),
      timeoutLimit,
    ]),
    retries: new FormControl<number | null>(null, [integer, Validators.min(0), Validators.max(10)]),
    recoveryThreshold: new FormControl<number | null>(null, [
      integer,
      Validators.min(1),
      Validators.max(10),
    ]),
    resendIntervalMinutes: new FormControl<number | null>(null, [integer, Validators.min(0)]),
    paused: new FormControl(false, { nonNullable: true }),
    tags: new FormControl('', { nonNullable: true, validators: [tagsValidator] }),
    notificationChannelIds: new FormControl<string[]>([], { nonNullable: true }),
    assignmentMode: new FormControl<AssignmentMode>('all', { nonNullable: true }),
    assignmentCount: new FormControl<number | null>(null, [
      integer,
      Validators.min(1),
      countRequired,
    ]),
    assignmentNodes: new FormControl('', { nonNullable: true, validators: [nodesRequired] }),
    consensusPolicy: new FormControl<ConsensusPolicy>('majority', { nonNullable: true }),
    config: new FormGroup({}),
  });

  // Cross-field rules: re-check the dependent control when its partner changes.
  const c = form.controls;
  c.intervalSeconds.valueChanges.subscribe(() => c.timeoutSeconds.updateValueAndValidity());
  c.assignmentMode.valueChanges.subscribe(() => {
    c.assignmentCount.updateValueAndValidity();
    c.assignmentNodes.updateValueAndValidity();
  });
  c.timeoutSeconds.updateValueAndValidity({ emitEvent: false });
  c.assignmentCount.updateValueAndValidity({ emitEvent: false });
  c.assignmentNodes.updateValueAndValidity({ emitEvent: false });
  return form;
}

/** Replaces the type-specific config group. `value` undefined applies the schema defaults. */
export function replaceConfig(form: FormGroup, schema: JsonSchema, value?: unknown): void {
  form.setControl('config', buildControl(schema, value, true));
}

/** Form values for the common fields of a monitor; missing input fields get the create defaults. */
export function monitorFormValue(input: Partial<MonitorInput>): Record<string, unknown> {
  const assignment: Assignment = input.assignment ?? { mode: 'all' };
  return {
    name: input.name ?? '',
    description: input.description ?? '',
    type: input.type ?? '',
    intervalSeconds: input.intervalSeconds ?? 60,
    timeoutSeconds: input.timeoutSeconds ?? null,
    retries: input.retries ?? 1,
    recoveryThreshold: input.recoveryThreshold ?? 1,
    resendIntervalMinutes: input.resendIntervalMinutes ?? 0,
    paused: input.paused ?? false,
    tags: (input.tags ?? []).join(', '),
    notificationChannelIds: input.notificationChannelIds ?? [],
    assignmentMode: assignment.mode,
    assignmentCount: assignment.count ?? null,
    assignmentNodes: (assignment.nodes ?? []).join(', '),
    consensusPolicy: input.consensusPolicy ?? 'majority',
  };
}

/**
 * Assembles the request body from the form. The config is converted from its
 * schema-driven controls; the body always carries the full state because PUT
 * replaces the monitor (notification channels included).
 */
export function monitorInputOf(form: FormGroup, configSchema: JsonSchema): MonitorInput {
  const v = form.getRawValue() as Record<string, unknown>;
  const config = controlToValue(configSchema, form.controls['config'], true);
  const description = String(v['description'] ?? '').trim();
  const timeout = v['timeoutSeconds'];

  const input: MonitorInput = {
    name: String(v['name'] ?? '').trim(),
    type: String(v['type'] ?? ''),
    config: (isRecord(config) ? config : {}) as MonitorInputConfig,
    intervalSeconds: Number(v['intervalSeconds']),
    retries: Number(v['retries']),
    recoveryThreshold: Number(v['recoveryThreshold']),
    resendIntervalMinutes: Number(v['resendIntervalMinutes']),
    paused: v['paused'] === true,
    assignment: assignmentOf(v),
    consensusPolicy: v['consensusPolicy'] as ConsensusPolicy,
    tags: parseList(String(v['tags'] ?? '')),
    notificationChannelIds: [...((v['notificationChannelIds'] as string[] | null) ?? [])],
  };
  if (description !== '') {
    input.description = description;
  }
  if (timeout !== null && timeout !== undefined && timeout !== '') {
    input.timeoutSeconds = Number(timeout);
  }
  return input;
}

/** A fresh idempotency key; falls back to getRandomValues where randomUUID needs a secure context. */
export function newRequestKey(): string {
  if (typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function assignmentOf(v: Record<string, unknown>): Assignment {
  const mode = v['assignmentMode'] as AssignmentMode;
  if (mode === 'count') {
    return { mode, count: Number(v['assignmentCount']) };
  }
  if (mode === 'nodes') {
    return { mode, nodes: parseList(String(v['assignmentNodes'] ?? '')) };
  }
  return { mode: 'all' };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

const integer: ValidatorFn = (c) => {
  const v = c.value;
  if (v === null || v === undefined || v === '') {
    return null;
  }
  return Number.isInteger(Number(v)) ? null : { integer: true };
};

/** Timeout may not exceed 80 % of the interval (FA-MON-03). */
const timeoutLimit: ValidatorFn = (c: AbstractControl): ValidationErrors | null => {
  const timeout = c.value as number | null;
  const interval = c.parent?.get('intervalSeconds')?.value as number | null | undefined;
  if (timeout === null || timeout === undefined || typeof interval !== 'number') {
    return null;
  }
  const max = Math.floor(interval * TIMEOUT_SHARE);
  return timeout > max ? { timeoutLimit: { max } } : null;
};

const tagsValidator: ValidatorFn = (c) => {
  const tags = parseList(String(c.value ?? ''));
  if (tags.length > TAG_LIMIT) {
    return { tagCount: { max: TAG_LIMIT } };
  }
  return tags.some((t) => t.length > TAG_LENGTH) ? { tagLength: { max: TAG_LENGTH } } : null;
};

const countRequired: ValidatorFn = (c) => {
  const mode = c.parent?.get('assignmentMode')?.value as AssignmentMode | undefined;
  const count = c.value as number | null;
  return mode === 'count' && (count === null || count === undefined)
    ? { countRequired: true }
    : null;
};

const nodesRequired: ValidatorFn = (c) => {
  const mode = c.parent?.get('assignmentMode')?.value as AssignmentMode | undefined;
  return mode === 'nodes' && parseList(String(c.value ?? '')).length === 0
    ? { nodesRequired: true }
    : null;
};
