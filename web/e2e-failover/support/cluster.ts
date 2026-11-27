// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';

/** Built by `make build`; the failover run never builds the binary itself. */
export const BINARY = path.resolve(__dirname, '..', '..', '..', 'bin', 'pulsequorum');

/** Environment variable that carries the path of the failover state file to the workers. */
export const STATE_ENV = 'UC_FAILOVER_STATE';

/** Written into the run directory when a test failed; keeps the logs after teardown. */
export const FAILED_MARKER = 'FAILED';

/** slog text format prints `setup_token=<token>`; the value may be quoted. */
const SETUP_TOKEN_PATTERN = /setup_token=("?)([^\s"]+)\1/;

export const STARTUP_TIMEOUT_MS = 90_000;
const STOP_TIMEOUT_MS = 15_000;

export interface VoterSpec {
  readonly index: number;
  readonly httpPort: number;
  readonly clusterPort: number;
}

/**
 * Three voters on localhost with distinct ports. Cookies ignore ports, so the
 * refresh cookie of one voter is sent to the others (same site, host-only cookie).
 */
export const VOTERS: readonly VoterSpec[] = [
  { index: 1, httpPort: 18181, clusterPort: 19181 },
  { index: 2, httpPort: 18182, clusterPort: 19182 },
  { index: 3, httpPort: 18183, clusterPort: 19183 },
];

/** Public origin of a voter, as the browser sees it and as config.json lists it. */
export function originOf(v: VoterSpec): string {
  return `http://localhost:${v.httpPort}`;
}

/** All voter origins in voter order. */
export const ALL_ORIGINS: readonly string[] = VOTERS.map(originOf);

/** Address the Node side uses to reach a voter (the voters bind 127.0.0.1). */
export function addrOf(v: VoterSpec): string {
  return `http://127.0.0.1:${v.httpPort}`;
}

/** Port of the external frontend started by the failover test. */
export const FRONTEND_PORT = 18190;
export const FRONTEND_ORIGIN = `http://localhost:${FRONTEND_PORT}`;

/** Local HTTP target for the monitors; separate from the core E2E target port. */
export const TARGET_PORT = 28492;
export const TARGET_URL = `http://127.0.0.1:${TARGET_PORT}`;

/** Test values only; the cluster is a throwaway instance in a temp directory. */
export const ADMIN = {
  username: 'failover-admin',
  password: 'failover-test-password-1',
};

export interface ProcessRecord {
  name: string;
  pid: number;
  log: string;
}

export interface FailoverState {
  runDir: string;
  processes: ProcessRecord[];
}

export function stateFilePath(): string {
  const statePath = process.env[STATE_ENV];
  if (!statePath) {
    throw new Error(`${STATE_ENV} is not set; the tests must run through global-setup.ts`);
  }
  return statePath;
}

export function readState(): FailoverState {
  return JSON.parse(fs.readFileSync(stateFilePath(), 'utf8')) as FailoverState;
}

export function writeState(state: FailoverState): void {
  fs.writeFileSync(stateFilePath(), JSON.stringify(state, null, 2));
}

/** Records a started process so that teardown can stop it. */
export function registerProcess(record: ProcessRecord): void {
  const state = readState();
  state.processes.push(record);
  writeState(state);
}

export function findProcess(name: string): ProcessRecord {
  const record = readState().processes.find((p) => p.name === name);
  if (!record) {
    throw new Error(`no process named ${name} in the run state`);
  }
  return record;
}

/** Marks the run as failed so that teardown keeps the logs. */
export function markRunFailed(): void {
  fs.writeFileSync(path.join(readState().runDir, FAILED_MARKER), 'failed\n');
}

/**
 * Starts a process with stdout and stderr going to logPath. Every chunk is also
 * handed to onOutput, which the caller uses to read the setup token.
 */
export function spawnLogged(opts: {
  command: string;
  args: string[];
  env: NodeJS.ProcessEnv;
  logPath: string;
  onOutput?: (text: string) => void;
}): ChildProcess {
  const log = fs.createWriteStream(opts.logPath);
  const child = spawn(opts.command, opts.args, {
    env: opts.env,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const onData = (chunk: Buffer): void => {
    log.write(chunk);
    opts.onOutput?.(chunk.toString('utf8'));
  };
  child.stdout?.on('data', onData);
  child.stderr?.on('data', onData);
  child.on('error', (err) => log.write(`spawn error: ${String(err)}\n`));
  child.on('exit', () => log.end());
  return child;
}

/** Extracts the setup token from text output, or undefined while it has not been printed. */
export function findSetupToken(text: string): string | undefined {
  return SETUP_TOKEN_PATTERN.exec(text)?.[2];
}

export function voterEnv(
  runDir: string,
  v: VoterSpec,
  join?: { seed: string; tokenFile: string },
): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {
    PATH: process.env.PATH ?? '',
    HOME: process.env.HOME ?? '',
    NODE_NAME: `voter-${v.index}`,
    NODE_ROLE: 'voter',
    AUTH_MODE: 'local',
    DATA_DIR: path.join(runDir, `data-${v.index}`),
    HTTP_ADDR: `127.0.0.1:${v.httpPort}`,
    CLUSTER_ADDR: `127.0.0.1:${v.clusterPort}`,
    CLUSTER_ADVERTISE: `127.0.0.1:${v.clusterPort}`,
    PUBLIC_URL: originOf(v),
    // The external frontend is a cross-origin caller of every voter.
    CORS_ORIGINS: FRONTEND_ORIGIN,
    LOG_FORMAT: 'text',
  };
  if (join) {
    env.CLUSTER_JOIN = join.seed;
    env.JOIN_TOKEN_FILE = join.tokenFile;
  }
  return env;
}

export interface ApiResult {
  status: number;
  body: unknown;
}

/** Minimal JSON call from the Node side; never logs the request or the response. */
export async function apiRequest(
  base: string,
  pathname: string,
  opts: { method?: string; token?: string; body?: unknown } = {},
): Promise<ApiResult> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (opts.token) {
    headers.Authorization = `Bearer ${opts.token}`;
  }
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }
  const res = await fetch(`${base}${pathname}`, {
    method: opts.method ?? 'GET',
    headers,
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
    signal: AbortSignal.timeout(10_000),
  });
  const text = await res.text();
  let body: unknown = undefined;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  return { status: res.status, body };
}

/** Polls until check() resolves to true. Failed attempts (connection refused) count as not yet. */
export async function waitFor(
  what: string,
  check: () => Promise<boolean>,
  timeoutMs = STARTUP_TIMEOUT_MS,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    try {
      if (await check()) {
        return;
      }
    } catch {
      // not reachable yet
    }
    if (Date.now() > deadline) {
      throw new Error(`${what} did not become ready within ${timeoutMs} ms`);
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
}

/** Reports whether the voter answers on its readiness endpoint. */
export async function isReady(v: VoterSpec): Promise<boolean> {
  const res = await fetch(`${addrOf(v)}/readyz`, { signal: AbortSignal.timeout(2_000) });
  return res.ok;
}

export function isAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

/** Sends SIGTERM and waits for the process to exit; SIGKILL after the timeout. */
export async function stopProcess(pid: number): Promise<void> {
  if (!pid || !isAlive(pid)) {
    return;
  }
  process.kill(pid, 'SIGTERM');
  await waitFor(`process ${pid} to stop`, async () => !isAlive(pid), STOP_TIMEOUT_MS).catch(
    () => undefined,
  );
  if (isAlive(pid)) {
    process.kill(pid, 'SIGKILL');
  }
}

/**
 * Kills a voter with SIGKILL, as a crashed node would die, and waits until its
 * HTTP port stops answering. The voter is not restarted during the run.
 */
export async function killVoter(index: number): Promise<void> {
  const v = VOTERS.find((x) => x.index === index);
  if (!v) {
    throw new Error(`unknown voter ${index}`);
  }
  const record = findProcess(`voter-${index}`);
  process.kill(record.pid, 'SIGKILL');
  await waitFor(
    `${originOf(v)} to stop answering`,
    async () => {
      try {
        await fetch(`${addrOf(v)}/readyz`, { signal: AbortSignal.timeout(1_000) });
        return false;
      } catch {
        return true;
      }
    },
    STOP_TIMEOUT_MS,
  );
}

/**
 * Starts the external frontend (`pulsequorum frontend`) with the given voter
 * origins and waits until it serves the SPA. Teardown stops it.
 */
export async function startFrontend(nodes: readonly string[]): Promise<void> {
  const runDir = readState().runDir;
  const logPath = path.join(runDir, 'frontend.log');
  const child = spawnLogged({
    command: BINARY,
    args: ['frontend'],
    env: {
      PATH: process.env.PATH ?? '',
      HOME: process.env.HOME ?? '',
      FRONTEND_NODES: nodes.join(','),
      FRONTEND_LISTEN: `127.0.0.1:${FRONTEND_PORT}`,
      AUTH_MODE: 'local',
    },
    logPath,
  });
  if (child.pid === undefined) {
    throw new Error('frontend did not start');
  }
  registerProcess({ name: 'frontend', pid: child.pid, log: logPath });
  await waitFor(`frontend ${FRONTEND_ORIGIN}`, async () => {
    const res = await fetch(`http://127.0.0.1:${FRONTEND_PORT}/config.json`, {
      signal: AbortSignal.timeout(2_000),
    });
    return res.ok;
  });
}
