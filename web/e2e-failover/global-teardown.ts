// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import fs from 'node:fs';
import path from 'node:path';
import { FAILED_MARKER, STATE_ENV, readState, stopProcess } from './support/cluster';

/**
 * Stops the frontend, the voters and the target. The data directories are
 * removed. The logs are removed too, unless a test failed: then the run
 * directory stays and its log paths are printed.
 */
export default async function globalTeardown(): Promise<void> {
  const statePath = process.env[STATE_ENV];
  if (!statePath || !fs.existsSync(statePath)) {
    return;
  }
  const state = readState();
  // Reverse start order: the frontend and the voters before the target.
  for (const p of [...state.processes].reverse()) {
    await stopProcess(p.pid);
  }

  for (const entry of fs.readdirSync(state.runDir)) {
    if (entry.startsWith('data-') || entry.startsWith('join-')) {
      fs.rmSync(path.join(state.runDir, entry), { recursive: true, force: true });
    }
  }

  if (fs.existsSync(path.join(state.runDir, FAILED_MARKER))) {
    const logs = state.processes.map((p) => `  ${p.name}: ${p.log}`).join('\n');
    console.log(`failover run failed; logs kept in ${state.runDir}:\n${logs}`);
    return;
  }
  fs.rmSync(state.runDir, { recursive: true, force: true });
}
