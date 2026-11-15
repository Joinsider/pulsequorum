// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ClusterNode } from '../../api/gen/model';

/**
 * Pure rules of the cluster page. The server enforces every rule again; the UI
 * uses these to explain and disable actions early (FA-CLU-08, FA-CLU-11).
 */

/** Voters needed for a majority of `voters` voters. */
export function quorumOf(voters: number): number {
  return Math.floor(voters / 2) + 1;
}

/** Voter failures the cluster survives with `voters` voters. */
export function faultTolerance(voters: number): number {
  return Math.max(0, voters - quorumOf(voters));
}

/** Leader first, then voters, then non-voters; inside each group by name (fallback: id). */
export function sortNodes(nodes: readonly ClusterNode[]): ClusterNode[] {
  return [...nodes].sort(
    (a, b) => roleRank(a) - roleRank(b) || nodeLabel(a).localeCompare(nodeLabel(b)),
  );
}

function roleRank(node: ClusterNode): number {
  if (node.leader) {
    return 0;
  }
  return node.voter ? 1 : 2;
}

/** Display name of a node: its registry name, or its id if it has none. */
export function nodeLabel(node: ClusterNode): string {
  return node.name || node.id;
}

/** Outcome of removing one node, computed from the current member list. */
export interface RemovalImpact {
  /** Translation key of the reason the removal is refused, or null. */
  blockedKey: string | null;
  /** Voters left after the removal. */
  votersLeft: number;
  /** Failures the cluster would tolerate with those voters. */
  toleratesAfter: number;
  /** Translation key of a quorum risk worth confirming, or null. */
  riskKey: string | null;
}

/**
 * What removing `target` does to the quorum. `nodes` is the full member list.
 * A removal is blocked for the leader (no leadership transfer in the UI yet) and
 * for the last voter. Removing a non-voter leaves the voter count unchanged.
 */
export function removalImpact(nodes: readonly ClusterNode[], target: ClusterNode): RemovalImpact {
  const voters = nodes.filter((n) => n.voter).length;
  const votersLeft = target.voter ? voters - 1 : voters;

  let blockedKey: string | null = null;
  if (target.leader) {
    blockedKey = 'admin.cluster.blocked.leader';
  } else if (target.voter && voters <= 1) {
    blockedKey = 'admin.cluster.blocked.lastVoter';
  }

  let riskKey: string | null = null;
  if (votersLeft < 3) {
    riskKey = 'admin.cluster.risk.noReserve';
  } else if (votersLeft % 2 === 0) {
    riskKey = 'admin.cluster.risk.even';
  }

  return {
    blockedKey,
    votersLeft,
    toleratesAfter: faultTolerance(votersLeft),
    riskKey,
  };
}

/** Health as the leader decided it; `unknown` while the node has not registered. */
export type NodeHealth = 'healthy' | 'unhealthy' | 'unknown';

export function healthOf(node: ClusterNode): NodeHealth {
  if (node.healthy === undefined) {
    return 'unknown';
  }
  return node.healthy ? 'healthy' : 'unhealthy';
}

/** Severity of the replication lag (FA-REP-05): below 60 s ok, below 5 min warn, else critical. */
export type LagSeverity = 'ok' | 'warn' | 'critical' | 'none';

export function lagSeverity(ms: number | undefined): LagSeverity {
  if (ms === undefined) {
    return 'none';
  }
  if (ms < 60_000) {
    return 'ok';
  }
  if (ms < 300_000) {
    return 'warn';
  }
  return 'critical';
}

/** Short human text for a duration: "850 ms", "12 s", "4 min", "2 h". Rounds down. */
export function formatDuration(ms: number): string {
  if (ms < 1000) {
    return `${Math.floor(ms)} ms`;
  }
  if (ms < 60_000) {
    return `${Math.floor(ms / 1000)} s`;
  }
  if (ms < 3_600_000) {
    return `${Math.floor(ms / 60_000)} min`;
  }
  return `${Math.floor(ms / 3_600_000)} h`;
}

/** Translation key for a failed cluster call, by HTTP status. */
export function clusterErrorKey(status: number | undefined): string {
  switch (status) {
    case 403:
      return 'admin.error.forbidden';
    case 404:
      return 'admin.cluster.error.notMember';
    case 409:
      return 'admin.cluster.error.conflict';
    case 503:
      return 'admin.error.unavailable';
    default:
      return 'admin.error.generic';
  }
}

/** HTTP status of a failed call, or undefined for anything that is not an HTTP error. */
export function statusOf(err: unknown): number | undefined {
  return err instanceof HttpErrorResponse ? err.status : undefined;
}
