// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import fs from 'node:fs';
import { STATE_ENV, type RunState } from './support/state';

const STOP_TIMEOUT_MS = 15_000;

/** Stops the embedded server and the target server started in global-setup.ts. The run directory and its server log are kept. */
export default async function globalTeardown(): Promise<void> {
  const statePath = process.env[STATE_ENV];
  if (!statePath || !fs.existsSync(statePath)) {
    return;
  }
  const state = JSON.parse(fs.readFileSync(statePath, 'utf8')) as RunState;
  // The server first, so it cannot write into a target that is already gone.
  for (const pid of [state.serverPid, state.targetPid]) {
    await stopProcess(pid);
  }
}

/** Sends SIGTERM and waits for the process to exit; SIGKILL after the timeout. */
async function stopProcess(pid: number): Promise<void> {
  if (!pid || !isAlive(pid)) {
    return;
  }
  process.kill(pid, 'SIGTERM');
  const deadline = Date.now() + STOP_TIMEOUT_MS;
  while (isAlive(pid) && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  if (isAlive(pid)) {
    process.kill(pid, 'SIGKILL');
  }
}

function isAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}
