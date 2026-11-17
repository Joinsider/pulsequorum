// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import { OwnTenant } from '../../api/gen/model';
import { hintKey, usageRows } from './tenant-usage';

function tenant(overrides: Partial<OwnTenant> = {}): OwnTenant {
  return {
    id: 'acme',
    name: 'Acme',
    plan: 'starter',
    effectiveLimits: { maxMonitors: 10, minIntervalSeconds: 60, maxChannels: null },
    usage: { monitors: 3, channels: 1, minIntervalSeconds: 60 },
    exceeded: [],
    ...overrides,
  };
}

describe('usageRows', () => {
  it('reports ok rows below the limits, with unlimited channels', () => {
    expect(usageRows(tenant())).toEqual([
      { key: 'monitors', used: 3, limit: 10, state: 'ok' },
      { key: 'channels', used: 1, limit: null, state: 'ok' },
      { key: 'interval', used: 60, limit: 60, state: 'ok' },
    ]);
  });

  it('reports reached when usage equals the limit', () => {
    const rows = usageRows(tenant({ usage: { monitors: 10, channels: 0 } }));
    expect(rows[0]).toMatchObject({ key: 'monitors', state: 'reached' });
  });

  it('reports exceeded from the server list, which takes precedence over reached', () => {
    const rows = usageRows(
      tenant({
        usage: { monitors: 12, channels: 0, minIntervalSeconds: 20 },
        exceeded: [
          { limit: 'maxMonitors', allowed: 10, actual: 12 },
          { limit: 'minIntervalSeconds', allowed: 60, actual: 20 },
        ],
      }),
    );
    expect(rows[0]).toMatchObject({ key: 'monitors', state: 'exceeded' });
    expect(rows[2]).toMatchObject({ key: 'interval', used: 20, limit: 60, state: 'exceeded' });
  });

  it('shows no interval when there are no monitors', () => {
    const rows = usageRows(tenant({ usage: { monitors: 0, channels: 0 } }));
    expect(rows[2]).toMatchObject({ used: null, state: 'ok' });
  });
});

describe('hintKey', () => {
  it('has no hint for ok rows', () => {
    expect(hintKey(usageRows(tenant())[0])).toBeNull();
  });

  it('names the state and the limit in the translation key', () => {
    const rows = usageRows(
      tenant({
        usage: { monitors: 10, channels: 0 },
        exceeded: [{ limit: 'maxChannels', allowed: 0, actual: 1 }],
      }),
    );
    expect(hintKey(rows[0])).toBe('dashboard.usage.hints.reached.monitors');
    expect(hintKey(rows[1])).toBe('dashboard.usage.hints.exceeded.channels');
  });
});
