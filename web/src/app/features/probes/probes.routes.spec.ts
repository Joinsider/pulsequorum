// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { describe, expect, it } from 'vitest';
import { ProbesPage } from './probes-page';
import { routes } from './probes.routes';

describe('probes.routes', () => {
  it('maps the root path to ProbesPage', () => {
    expect(routes).toEqual([{ path: '', component: ProbesPage }]);
  });
});
