// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import fs from 'node:fs';
import path from 'node:path';

/** Address of the embedded server started by global-setup.ts. */
export const SERVER_PORT = 28490;
export const SERVER_URL = `http://127.0.0.1:${SERVER_PORT}`;

/** Local HTTP target for the monitors; answers 200 until /__down is requested. */
export const TARGET_PORT = 28491;
export const TARGET_URL = `http://127.0.0.1:${TARGET_PORT}`;

/** Environment variable that carries the path of the run state file to the workers. */
export const STATE_ENV = 'UC_E2E_STATE';

/** Test values only; the server is a throwaway instance in a temp directory. */
export const ADMIN = {
  username: 'e2eadmin',
  password: 'e2e-test-password-1',
};

export const MONITOR_NAME = 'E2E HTTP target';
export const CHANNEL_NAME = 'E2E webhook channel';
/** Shoutrrr generic URL with credentials, so the list must show a masked target. */
export const CHANNEL_URL = `generic+http://e2e:e2e-secret@127.0.0.1:${TARGET_PORT}/hook`;

/** Written by global-setup.ts; PIDs are used by global-teardown.ts. */
export interface RunState {
  runDir: string;
  serverPid: number;
  targetPid: number;
  serverLog: string;
}

export function readRunState(): RunState {
  const statePath = process.env[STATE_ENV];
  if (!statePath) {
    throw new Error(`${STATE_ENV} is not set; the tests must run through global-setup.ts`);
  }
  return JSON.parse(fs.readFileSync(statePath, 'utf8')) as RunState;
}

/** The one-time setup token that the server printed at its first start. */
export function readSetupToken(): string {
  const token = fs.readFileSync(path.join(readRunState().runDir, 'setup-token'), 'utf8').trim();
  if (!token) {
    throw new Error('setup token file is empty');
  }
  return token;
}
