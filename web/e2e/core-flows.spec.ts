// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { expect, test, type BrowserContext, type Page } from '@playwright/test';
import {
  ADMIN,
  CHANNEL_NAME,
  CHANNEL_URL,
  MONITOR_NAME,
  TARGET_URL,
  readSetupToken,
} from './support/state';

// The steps build on each other (admin, session, monitor, incident, channel),
// so they run in order inside one browser context.
test.describe.configure({ mode: 'serial' });

/** Navigation entries live in the sidenav; the dashboard has similarly named buttons. */
const navLink = (name: string) =>
  page.locator('#app-navigation').getByRole('link', { name, exact: true });

let context: BrowserContext;
let page: Page;

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext();
  page = await context.newPage();
});

test.afterAll(async () => {
  await context.close();
});

test('setup creates the first administrator and opens the dashboard', async () => {
  const token = readSetupToken();

  await page.goto('/setup');
  await page.getByLabel('Setup token').fill(token);
  await page.getByLabel('Username', { exact: true }).fill(ADMIN.username);
  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByLabel('Repeat password').fill(ADMIN.password);
  await page.getByRole('button', { name: 'Create administrator' }).click();

  await expect(page.getByRole('heading', { name: 'Monitors', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: ADMIN.username })).toBeVisible();
});

test('sign out, a wrong password is rejected, the right one signs in', async () => {
  await page.getByRole('button', { name: ADMIN.username }).click();
  await page.getByRole('menuitem', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);

  await page.getByLabel('Username', { exact: true }).fill(ADMIN.username);
  await page.getByLabel('Password', { exact: true }).fill(`wrong-${ADMIN.password}`);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(
    page.getByRole('alert').filter({ hasText: 'Invalid username or password.' }),
  ).toBeVisible();

  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Monitors', exact: true })).toBeVisible();
});

test('an HTTP monitor created in the editor turns Up without a reload', async () => {
  await page.getByRole('link', { name: 'New monitor' }).first().click();
  await expect(page).toHaveURL(/\/monitors\/new$/);

  await page.getByLabel('Name', { exact: true }).fill(MONITOR_NAME);
  await page.getByLabel('Monitor type').click();
  await page.getByRole('option', { name: 'HTTP(S)' }).click();
  await page.getByLabel('URL', { exact: true }).fill(`${TARGET_URL}/`);
  await page.getByLabel('Interval (seconds)').fill('20');
  await page.getByLabel('Retries', { exact: true }).fill('0');
  await page.getByRole('button', { name: 'Save', exact: true }).click();

  await expect(page).toHaveURL(/\/monitors\/[^/]+$/);
  await expect(page.getByRole('heading', { name: MONITOR_NAME, exact: true })).toBeVisible();

  // Live update over WebSocket: the page is not reloaded while waiting.
  await expect(page.locator('app-status-badge').first()).toHaveText('Up', { timeout: 45_000 });
});

test('target answering 503 turns the monitor Down live and opens an incident', async ({
  request,
}) => {
  const down = await request.get(`${TARGET_URL}/__down`);
  expect(down.ok()).toBeTruthy();

  // Two intervals (40 s) plus 20 s of slack.
  await expect(page.locator('app-status-badge').first()).toHaveText('Down', { timeout: 60_000 });

  await navLink('Incidents').click();
  await expect(page.getByRole('heading', { name: 'Incidents', exact: true })).toBeVisible();
  await expect(page.getByRole('main').getByText(MONITOR_NAME).first()).toBeVisible();

  const up = await request.get(`${TARGET_URL}/__up`);
  expect(up.ok()).toBeTruthy();
});

test('a notification channel is listed with a masked target', async () => {
  await navLink('Notification channels').click();
  await page.getByRole('link', { name: 'New channel' }).click();
  await expect(page).toHaveURL(/\/channels\/new$/);

  await page.getByLabel('Name', { exact: true }).fill(CHANNEL_NAME);
  await page.getByLabel('Service', { exact: true }).click();
  await page.getByRole('option', { name: 'Expert: enter URL' }).click();
  await page.getByLabel('Shoutrrr URL').fill(CHANNEL_URL);
  await page.getByRole('checkbox', { name: 'Down' }).check();
  await page.getByRole('button', { name: 'Save', exact: true }).click();

  await expect(page).toHaveURL(/\/channels$/);
  const card = page.locator('li.card').filter({
    has: page.getByRole('heading', { name: CHANNEL_NAME, exact: true }),
  });
  await expect(card).toBeVisible();
  await expect(card.locator('code')).toContainText('***');
  await expect(card.locator('code')).not.toContainText('e2e-secret');
});

test('German labels and the dark theme are applied', async () => {
  await page.getByRole('button', { name: 'Language' }).click();
  await page.getByRole('menuitem', { name: 'Deutsch' }).click();
  await expect(navLink('Benachrichtigungskanäle')).toBeVisible();

  const themeToggle = page.getByRole('button', { name: /^(Theme|Design):/ });
  await themeToggle.click(); // system -> light
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await themeToggle.click(); // light -> dark
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
});
