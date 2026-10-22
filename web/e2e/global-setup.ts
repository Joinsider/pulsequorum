// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  SERVER_PORT,
  SERVER_URL,
  STATE_ENV,
  TARGET_PORT,
  TARGET_URL,
  type RunState,
} from './support/state';

/** Built by `make build`; the E2E run never builds the binary itself. */
const SERVER_BINARY = path.resolve(__dirname, '..', '..', 'bin', 'uptimecluster');
const STARTUP_TIMEOUT_MS = 60_000;
/** slog text format prints `setup_token=<token>`; the value may be quoted. */
const TOKEN_PATTERN = /setup_token=("?)([^\s"]+)\1/;

/**
 * Starts the target server and the embedded binary with a fresh data
 * directory, captures the one-time setup token from its log and waits until
 * /readyz answers. Everything started here is stopped in global-teardown.ts.
 */
export default async function globalSetup(): Promise<void> {
  if (!fs.existsSync(SERVER_BINARY)) {
    throw new Error(`${SERVER_BINARY} not found; run "make build" first (make web-e2e does this)`);
  }

  const runDir = fs.mkdtempSync(path.join(os.tmpdir(), 'uptimecluster-e2e-'));
  const dataDir = path.join(runDir, 'data');
  fs.mkdirSync(dataDir);
  const serverLog = path.join(runDir, 'server.log');
  const logStream = fs.createWriteStream(serverLog);

  const target = spawn(
    process.execPath,
    [path.join(__dirname, 'support', 'target-server.mjs'), String(TARGET_PORT)],
    { stdio: 'ignore' },
  );
  let server: ChildProcess | undefined;

  try {
    await waitFor(`target ${TARGET_URL}`, async () => (await fetch(`${TARGET_URL}/`)).ok);

    server = spawn(SERVER_BINARY, [], {
      cwd: runDir,
      env: {
        PATH: process.env.PATH ?? '',
        HOME: process.env.HOME ?? '',
        DATA_DIR: dataDir,
        HTTP_ADDR: `127.0.0.1:${SERVER_PORT}`,
        NODE_NAME: 'e2e',
        LOG_FORMAT: 'text',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    const token = await captureSetupToken(server, logStream, serverLog);

    await waitFor(`${SERVER_URL}/readyz`, async () => (await fetch(`${SERVER_URL}/readyz`)).ok);

    fs.writeFileSync(path.join(runDir, 'setup-token'), token, { mode: 0o600 });
    const state: RunState = {
      runDir,
      serverPid: server.pid ?? 0,
      targetPid: target.pid ?? 0,
      serverLog,
    };
    const statePath = path.join(runDir, 'state.json');
    fs.writeFileSync(statePath, JSON.stringify(state, null, 2));
    process.env[STATE_ENV] = statePath;
  } catch (err) {
    target.kill('SIGKILL');
    server?.kill('SIGKILL');
    throw new Error(`E2E startup failed (server log: ${serverLog}): ${String(err)}`);
  }
}

/** Reads the server output until the setup token line appears; the output keeps going to the log. */
function captureSetupToken(
  server: ChildProcess,
  logStream: fs.WriteStream,
  serverLog: string,
): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    let buffer = '';
    const timer = setTimeout(() => {
      reject(new Error(`no setup token printed within ${STARTUP_TIMEOUT_MS} ms (${serverLog})`));
    }, STARTUP_TIMEOUT_MS);
    const onData = (chunk: Buffer): void => {
      const text = chunk.toString('utf8');
      logStream.write(text);
      buffer += text;
      const match = TOKEN_PATTERN.exec(buffer);
      if (match) {
        clearTimeout(timer);
        resolve(match[2]);
      }
    };
    server.stdout?.on('data', onData);
    server.stderr?.on('data', onData);
    server.once('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`server exited with code ${code} during startup (${serverLog})`));
    });
  });
}

/** Polls until check() resolves to true; failed attempts (e.g. connection refused) count as not ready. */
async function waitFor(what: string, check: () => Promise<boolean>): Promise<void> {
  const deadline = Date.now() + STARTUP_TIMEOUT_MS;
  for (;;) {
    try {
      if (await check()) {
        return;
      }
    } catch {
      // not reachable yet
    }
    if (Date.now() > deadline) {
      throw new Error(`${what} did not become ready within ${STARTUP_TIMEOUT_MS} ms`);
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
}
