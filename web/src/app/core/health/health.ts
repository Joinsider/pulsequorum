// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

/** Response of `GET /api/v1/health`. */
export interface Health {
  status: string;
  role: 'voter' | 'probe';
  nodeName: string;
  version: string;
  commit: string;
}
