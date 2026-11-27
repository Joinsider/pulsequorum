// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { defineConfig, devices } from '@playwright/test';
import { originOf, VOTERS } from './e2e-failover/support/cluster';

/**
 * Client failover E2E (FA-UI-03, ADR 0036): three voters on localhost, one of
 * them is killed during the run. Separate from playwright.config.ts, so the
 * default `npx playwright test` does not start a cluster. Nothing is built
 * here: `make web-e2e-failover` runs `make build` first.
 */
export default defineConfig({
  testDir: './e2e-failover',
  testMatch: '**/*.spec.ts',
  // The steps share one cluster and kill a voter, so they run in order, one worker.
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 300_000,
  expect: { timeout: 30_000 },
  globalSetup: './e2e-failover/global-setup.ts',
  globalTeardown: './e2e-failover/global-teardown.ts',
  outputDir: './test-results',
  reporter: [['list'], ['html', { open: 'never', outputFolder: './playwright-report' }]],
  use: {
    baseURL: originOf(VOTERS[0]),
    locale: 'en-US',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
