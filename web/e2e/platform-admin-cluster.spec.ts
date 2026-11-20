// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { expect, test, type BrowserContext, type Page } from '@playwright/test';
import { ADMIN } from './support/state';

// Runs after core-flows.spec.ts (file order), which creates the platform admin.
// It signs in in a fresh browser context of its own.
test.describe.configure({ mode: 'serial' });

let context: BrowserContext;
let page: Page;

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext();
  page = await context.newPage();
});

test.afterAll(async () => {
  await context.close();
});

test('the platform admin opens the cluster page from the navigation', async () => {
  await page.goto('/login');
  await page.getByLabel('Username', { exact: true }).fill(ADMIN.username);
  await page.getByLabel('Password', { exact: true }).fill(ADMIN.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Monitors', exact: true })).toBeVisible();

  await page.locator('#app-navigation').getByRole('link', { name: 'Cluster', exact: true }).click();
  await expect(page).toHaveURL(/\/admin\/cluster$/);
  await expect(page.getByRole('heading', { name: 'Cluster', exact: true })).toBeVisible();
});

test('the single node is marked as leader and as this node, and cannot be removed', async () => {
  const nodes = page.locator('[data-testid="cluster-node"]');
  await expect(nodes).toHaveCount(1);
  await expect(nodes.getByText('Leader', { exact: true })).toBeVisible();
  await expect(nodes.getByText('This node', { exact: true })).toBeVisible();

  await expect(nodes.getByRole('button', { name: /^Remove:/ })).toBeDisabled();
  await expect(nodes.getByRole('button', { name: /^Replace:/ })).toBeDisabled();
});

test('adding a voter shows a one-time join token until Done closes the dialog', async () => {
  await page.getByRole('button', { name: 'Add voter', exact: true }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();

  await dialog.getByRole('button', { name: 'Create token', exact: true }).click();
  await expect(page.locator('[data-testid=join-token]')).not.toHaveValue('');

  await dialog.getByRole('button', { name: 'Done', exact: true }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
});
