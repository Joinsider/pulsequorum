// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

/** Authentication settings delivered by the backend in `config.json`. */
export interface AuthConfig {
  mode: 'local' | 'oidc';
  issuer?: string;
  clientId?: string;
  scopes?: string[];
}

/** Runtime configuration served as `/config.json` next to the SPA. */
export interface AppConfig {
  /**
   * Backend node base URLs. An empty list means "use the same origin as the page".
   * The first entry is used as the API base URL.
   */
  nodes: string[];
  auth: AuthConfig;
}
