// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { MonitorStatus } from '../../api/gen/model';

/**
 * CSS variable holding the colour of a monitor status (defined in styles.scss).
 * Use it for text, borders and chart series; it adapts to light and dark mode.
 */
export function statusColorVar(status: MonitorStatus): string {
  return `var(--status-${status.toLowerCase()})`;
}
