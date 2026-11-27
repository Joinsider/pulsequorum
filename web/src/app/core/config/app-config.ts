// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

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
   * Backend node origins (`https://host[:port]`) the SPA may send requests and
   * bearer tokens to (ADR 0036). An empty list means "use the same origin as
   * the page" without failover. Entries are checked with `normalizeOrigin`;
   * the node list the backend reports can only narrow this list.
   */
  nodes: string[];
  auth: AuthConfig;
}
