// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { expect, test, type BrowserContext, type Page, type Request } from '@playwright/test';
import {
  ADMIN,
  ALL_ORIGINS,
  FRONTEND_ORIGIN,
  FRONTEND_PORT,
  TARGET_URL,
  VOTERS,
  addrOf,
  killVoter,
  markRunFailed,
  originOf,
  startFrontend,
} from './support/cluster';

// FA-UI-03: the SPA picks a healthy voter, switches when it fails, and keeps working.
// The steps build on each other (sign in, monitor, kill, failover, stream, external frontend).

const MONITOR_NAME = 'Failover HTTP target';
const MONITOR_AFTER_KILL = 'Created after the kill';

test.describe.configure({ mode: 'serial' });

interface SeenRequest {
  origin: string;
  pathname: string;
  method: string;
}

/** API requests of the embedded SPA, by origin. */
const apiRequests: SeenRequest[] = [];
const websocketUrls: string[] = [];

let context: BrowserContext | undefined;
let page: Page;
/** Voter the SPA used for the monitor list before the kill. */
let victimOrigin = '';
/** Voters still running after the kill. */
let survivors: string[] = [];

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext();
  page = await context.newPage();
  page.on('request', (req: Request) => {
    const url = new URL(req.url());
    if (url.pathname.startsWith('/api/v1/')) {
      apiRequests.push({ origin: url.origin, pathname: url.pathname, method: req.method() });
    }
  });
  page.on('websocket', (ws) => websocketUrls.push(ws.url()));
});

test.afterAll(async () => {
  await context?.close();
});

test.afterEach(async ({}, testInfo) => {
  if (testInfo.status !== testInfo.expectedStatus) {
    markRunFailed();
  }
});

/** Navigation entries live in the sidenav; the dashboard has similarly named buttons. */
const navLink = (p: Page, name: string) =>
  p.locator('#app-navigation').getByRole('link', { name, exact: true });

const isMonitorList = (url: string): boolean => new URL(url).pathname === '/api/v1/monitors';

/** Origin of the last monitor list request the SPA sent. */
function lastMonitorListOrigin(): string | undefined {
  return [...apiRequests]
    .reverse()
    .find((r) => r.method === 'GET' && r.pathname === '/api/v1/monitors')?.origin;
}

async function signIn(p: Page): Promise<void> {
  await p.getByLabel('Username', { exact: true }).fill(ADMIN.username);
  await p.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await p.getByRole('button', { name: 'Sign in' }).click();
  await expect(p.getByRole('heading', { name: 'Monitors', exact: true })).toBeVisible();
}

async function createHttpMonitor(p: Page, name: string): Promise<void> {
  await p.getByRole('link', { name: 'New monitor' }).first().click();
  await expect(p).toHaveURL(/\/monitors\/new$/);

  await p.getByLabel('Name', { exact: true }).fill(name);
  await p.getByLabel('Monitor type').click();
  await p.getByRole('option', { name: 'HTTP(S)' }).click();
  await p.getByLabel('URL', { exact: true }).fill(`${TARGET_URL}/`);
  await p.getByLabel('Interval (seconds)').fill('20');
  await p.getByLabel('Retries', { exact: true }).fill('0');
  await p.getByRole('button', { name: 'Save', exact: true }).click();

  await expect(p).toHaveURL(/\/monitors\/[^/]+$/);
  await expect(p.getByRole('heading', { name, exact: true })).toBeVisible();
}

test('config.json lists all three voters and the admin signs in', async () => {
  // Own PUBLIC_URL first, then the other voters from the node registry (ADR 0036).
  const res = await fetch(`${addrOf(VOTERS[0])}/config.json`);
  expect(res.ok).toBeTruthy();
  const cfg = (await res.json()) as { nodes: string[] };
  expect(cfg.nodes).toHaveLength(3);
  expect(cfg.nodes[0]).toBe(originOf(VOTERS[0]));
  expect([...cfg.nodes].sort()).toEqual([...ALL_ORIGINS].sort());

  await page.goto('/');
  await signIn(page);
});

test('an HTTP monitor created in the editor is listed and turns Up', async () => {
  await createHttpMonitor(page, MONITOR_NAME);
  await expect(page.locator('app-status-badge').first()).toHaveText('Up', { timeout: 60_000 });

  await navLink(page, 'Dashboard').click();
  await expect(page.getByText(MONITOR_NAME, { exact: true }).first()).toBeVisible();
});

test('the voter the SPA uses is killed with SIGKILL', async () => {
  const origin = lastMonitorListOrigin();
  expect(origin, 'the SPA requested the monitor list from a voter').toBeTruthy();
  const victim = VOTERS.find((v) => originOf(v) === origin);
  if (!victim) {
    throw new Error(`no voter for origin ${origin}`);
  }

  victimOrigin = originOf(victim);
  survivors = ALL_ORIGINS.filter((o) => o !== victimOrigin);
  await killVoter(victim.index);
  // The page itself may be served by this voter. Its JavaScript keeps running in
  // the open tab, so the next steps use in-app navigation, not a reload.
});

test('after the kill the monitor list loads from a surviving voter', async () => {
  // Open the monitor, then return to the list: both calls are new requests.
  await page.getByText(MONITOR_NAME, { exact: true }).first().click();
  await expect(page.getByRole('heading', { name: MONITOR_NAME, exact: true })).toBeVisible();

  const listed = page.waitForResponse(
    (r) => isMonitorList(r.url()) && r.request().method() === 'GET' && r.ok(),
    { timeout: 60_000 },
  );
  await navLink(page, 'Dashboard').click();
  const response = await listed;

  const origin = new URL(response.url()).origin;
  expect(origin).not.toBe(victimOrigin);
  expect(survivors).toContain(origin);
  await expect(page.getByText(MONITOR_NAME, { exact: true }).first()).toBeVisible();
});

test('a monitor created after the kill is stored exactly once', async ({ request }) => {
  const survivor = survivors[0];
  // Killing the Raft leader leaves the cluster without a leader for a moment.
  // Login and writes then fail with 503 "no leader available, retry", and the SPA
  // switches the node only once per request (ADR 0036). So wait until a survivor
  // accepts a login and reports a leader.
  let accessToken = '';
  await expect
    .poll(
      async () => {
        const login = await request.post(`${survivor}/api/v1/auth/login`, {
          data: { username: ADMIN.username, password: ADMIN.password },
        });
        if (!login.ok()) {
          return false;
        }
        accessToken = ((await login.json()) as { accessToken: string }).accessToken;
        const nodes = await request.get(`${survivor}/api/v1/admin/nodes`, {
          headers: { Authorization: `Bearer ${accessToken}` },
        });
        return nodes.ok() && ((await nodes.json()) as { leaderId: string }).leaderId !== '';
      },
      { timeout: 60_000 },
    )
    .toBe(true);

  await navLink(page, 'Dashboard').click();
  await createHttpMonitor(page, MONITOR_AFTER_KILL);

  // Count on a surviving voter. Duplicates from a retried POST (Idempotency-Key,
  // ADR 0036) would show up here.

  const list = await request.get(`${survivor}/api/v1/monitors`, {
    headers: { Authorization: `Bearer ${accessToken}` },
  });
  expect(list.ok()).toBeTruthy();
  const { items } = (await list.json()) as { items: { name: string }[] };
  expect(items.map((m) => m.name).sort()).toEqual([MONITOR_NAME, MONITOR_AFTER_KILL].sort());
});

test('the live stream reconnects to a surviving voter', async () => {
  const onSurvivor = (url: string): boolean =>
    survivors.some((o) => url.startsWith(`${o.replace(/^http/, 'ws')}/api/v1/stream`));

  await expect.poll(() => websocketUrls.some(onSurvivor), { timeout: 60_000 }).toBeTruthy();
  // The shell shows a disconnect indicator while the stream is not open.
  await expect(page.locator('.live-indicator')).toHaveCount(0, { timeout: 30_000 });
});

test('an external frontend on the surviving voters signs in and lists the monitors', async ({
  browser,
}) => {
  await startFrontend(survivors);

  const cfgRes = await fetch(`http://127.0.0.1:${FRONTEND_PORT}/config.json`);
  const cfg = (await cfgRes.json()) as { nodes: string[] };
  expect([...cfg.nodes].sort()).toEqual([...survivors].sort());

  const external = await browser.newContext();
  try {
    const p = await external.newPage();
    await p.goto(`${FRONTEND_ORIGIN}/`);
    await signIn(p);
    await expect(p.getByText(MONITOR_NAME, { exact: true }).first()).toBeVisible();
    await expect(p.getByText(MONITOR_AFTER_KILL, { exact: true }).first()).toBeVisible();
  } finally {
    await external.close();
  }
});
