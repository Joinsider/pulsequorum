// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  ADMIN,
  ALL_ORIGINS,
  BINARY,
  STATE_ENV,
  STARTUP_TIMEOUT_MS,
  TARGET_PORT,
  TARGET_URL,
  VOTERS,
  addrOf,
  apiRequest,
  findSetupToken,
  isReady,
  originOf,
  registerProcess,
  spawnLogged,
  stopProcess,
  voterEnv,
  waitFor,
  writeState,
  type FailoverState,
  type VoterSpec,
} from './support/cluster';

/** Join tokens are printable, URL-safe text; anything else is not passed on. */
const JOIN_TOKEN_PATTERN = /^[A-Za-z0-9._~:-]{1,512}$/;
/**
 * Starts a three-voter cluster from the built binary: voter 1 bootstraps and
 * prints the setup token, the admin is created through the API, voters 2 and 3
 * join with fresh join tokens. Everything started here is stopped in
 * global-teardown.ts. On a startup error all processes are stopped and the logs kept.
 */
export default async function globalSetup(): Promise<void> {
  if (!fs.existsSync(BINARY)) {
    throw new Error(
      `${BINARY} not found; run "make build" first (make web-e2e-failover does this)`,
    );
  }

  const runDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pulsequorum-failover-'));
  const state: FailoverState = { runDir, processes: [] };
  process.env[STATE_ENV] = path.join(runDir, 'state.json');
  writeState(state);

  try {
    const target = spawnLogged({
      command: process.execPath,
      args: [
        path.join(__dirname, '..', 'e2e', 'support', 'target-server.mjs'),
        String(TARGET_PORT),
      ],
      env: { PATH: process.env.PATH ?? '' },
      logPath: path.join(runDir, 'target.log'),
    });
    registerProcess({ name: 'target', pid: target.pid ?? 0, log: path.join(runDir, 'target.log') });
    await waitFor(`target ${TARGET_URL}`, async () => (await fetch(`${TARGET_URL}/`)).ok);

    await startCluster(runDir);
    await waitForVoterList();
  } catch (err) {
    for (const p of readStateSafely(runDir)?.processes ?? []) {
      await stopProcess(p.pid);
    }
    throw new Error(`failover startup failed, logs kept in ${runDir}: ${String(err)}`);
  }
}

/** Bootstraps voter 1, creates the admin, joins voters 2 and 3. */
async function startCluster(runDir: string): Promise<void> {
  const [first, ...others] = VOTERS;
  const token = await startBootstrapVoter(runDir, first);
  const accessToken = await createAdmin(first, token);

  for (const v of others) {
    await joinVoter(runDir, v, accessToken);
  }
}

async function startBootstrapVoter(runDir: string, v: VoterSpec): Promise<string> {
  const logPath = path.join(runDir, `voter-${v.index}.log`);
  let buffer = '';
  let setupToken: string | undefined;
  const child = spawnLogged({
    command: BINARY,
    args: [],
    env: voterEnv(runDir, v),
    logPath,
    onOutput: (text) => {
      if (setupToken === undefined) {
        buffer = (buffer + text).slice(-8192);
        setupToken = findSetupToken(buffer);
      }
    },
  });
  fs.mkdirSync(path.join(runDir, `data-${v.index}`), { recursive: true });
  registerProcess({ name: `voter-${v.index}`, pid: child.pid ?? 0, log: logPath });

  await waitFor(`setup token of ${originOf(v)}`, async () => {
    if (child.exitCode !== null) {
      throw new Error(`voter ${v.index} exited with code ${child.exitCode}`);
    }
    return setupToken !== undefined;
  });
  await waitFor(`${originOf(v)} ready`, () => isReady(v));
  if (setupToken === undefined) {
    throw new Error('setup token missing after wait');
  }
  return setupToken;
}

/** Creates the first platform admin with the one-time setup token. The token is not stored. */
async function createAdmin(v: VoterSpec, setupToken: string): Promise<string> {
  const res = await apiRequest(addrOf(v), '/api/v1/auth/setup', {
    method: 'POST',
    body: {
      setupToken,
      username: ADMIN.username,
      displayName: 'Failover admin',
      password: ADMIN.password,
    },
  });
  if (res.status !== 201) {
    throw new Error(`setup on ${originOf(v)} returned HTTP ${res.status}`);
  }
  return (res.body as { accessToken: string }).accessToken;
}

/** Joins voter v with a fresh one-time token created on voter 1 (the leader, as only voter 1 exists yet). */
async function joinVoter(runDir: string, v: VoterSpec, accessToken: string): Promise<void> {
  const seed = VOTERS[0];
  const tok = await apiRequest(addrOf(seed), '/api/v1/admin/join-tokens', {
    method: 'POST',
    token: accessToken,
    body: { role: 'voter', ttlMinutes: 10 },
  });
  if (tok.status !== 201) {
    throw new Error(`join token on ${originOf(seed)} returned HTTP ${tok.status}`);
  }
  // The token is a secret: it only goes to the child's environment, never to disk.
  const joinToken = (tok.body as { token?: unknown }).token;
  if (typeof joinToken !== 'string' || !JOIN_TOKEN_PATTERN.test(joinToken)) {
    throw new Error(`join token on ${originOf(seed)} has an unexpected format`);
  }
  fs.mkdirSync(path.join(runDir, `data-${v.index}`), { recursive: true });

  const logPath = path.join(runDir, `voter-${v.index}.log`);
  const child = spawnLogged({
    command: BINARY,
    args: [],
    env: voterEnv(runDir, v, { seed: `127.0.0.1:${seed.clusterPort}`, joinToken }),
    logPath,
  });
  registerProcess({ name: `voter-${v.index}`, pid: child.pid ?? 0, log: logPath });

  await waitFor(`${originOf(v)} ready after join`, async () => {
    if (child.exitCode !== null) {
      throw new Error(`voter ${v.index} exited with code ${child.exitCode} during join`);
    }
    return isReady(v);
  });
}

/** Waits until voter 1 lists all voters with their public URL (command level 11, ADR 0036). */
async function waitForVoterList(): Promise<void> {
  await waitFor(
    'all voters listed in /api/v1/cluster/nodes',
    async () => {
      const res = await apiRequest(addrOf(VOTERS[0]), '/api/v1/cluster/nodes');
      if (res.status !== 200) {
        return false;
      }
      const nodes = (res.body as { nodes: { url: string }[] }).nodes ?? [];
      const urls = new Set(nodes.map((n) => n.url));
      return ALL_ORIGINS.every((o) => urls.has(o));
    },
    STARTUP_TIMEOUT_MS,
  );
}

function readStateSafely(runDir: string): FailoverState | undefined {
  try {
    return JSON.parse(fs.readFileSync(path.join(runDir, 'state.json'), 'utf8')) as FailoverState;
  } catch {
    return undefined;
  }
}
