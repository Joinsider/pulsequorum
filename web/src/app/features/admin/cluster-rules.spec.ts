// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { describe, expect, it } from 'vitest';
import { ClusterNode } from '../../api/gen/model';
import {
  clockSeverity,
  clusterErrorKey,
  faultTolerance,
  formatBytes,
  formatDuration,
  formatOffset,
  healthOf,
  isProbe,
  lagSeverity,
  nodeLabel,
  quorumOf,
  removalImpact,
  sortNodes,
  ssrfMismatch,
  statusOf,
} from './cluster-rules';

/** Builds a registered voter by default; override the fields a case needs. */
function node(id: string, overrides: Partial<ClusterNode> = {}): ClusterNode {
  return { id, address: `${id}:7000`, voter: true, leader: false, self: false, ...overrides };
}

/** `count` voters with n1 as leader, and `extra` non-voters. */
function cluster(count: number, extra = 0): ClusterNode[] {
  const voters = Array.from({ length: count }, (_, i) =>
    node(`v${i + 1}`, { leader: i === 0, self: i === 0 }),
  );
  const learners = Array.from({ length: extra }, (_, i) => node(`l${i + 1}`, { voter: false }));
  return [...voters, ...learners];
}

describe('quorumOf', () => {
  it.each([
    [0, 1],
    [1, 1],
    [2, 2],
    [3, 2],
    [4, 3],
    [5, 3],
    [6, 4],
  ])('%i voters need %i for a quorum', (voters, quorum) => {
    expect(quorumOf(voters)).toBe(quorum);
  });
});

describe('faultTolerance', () => {
  it.each([
    [1, 0],
    [2, 0],
    [3, 1],
    [4, 1],
    [5, 2],
    [7, 3],
  ])('%i voters tolerate %i failures', (voters, tolerated) => {
    expect(faultTolerance(voters)).toBe(tolerated);
  });

  it('never goes below zero', () => {
    expect(faultTolerance(0)).toBe(0);
  });
});

describe('sortNodes', () => {
  it('puts the leader first, then voters, then non-voters, each by name', () => {
    const sorted = sortNodes([
      node('c', { name: 'charlie', voter: false }),
      node('b', { name: 'bravo' }),
      node('a', { name: 'alpha', leader: true }),
      node('d', { name: 'delta' }),
      node('e', { name: 'echo', voter: false }),
    ]);
    expect(sorted.map((n) => n.id)).toEqual(['a', 'b', 'd', 'c', 'e']);
  });

  it('falls back to the id when a node has no name', () => {
    const sorted = sortNodes([node('zz'), node('aa'), node('mm')]);
    expect(sorted.map((n) => n.id)).toEqual(['aa', 'mm', 'zz']);
  });

  it('puts probes after voters and non-voters, sorted by name', () => {
    const sorted = sortNodes([
      node('p2', { name: 'probe-z', voter: false, role: 'probe' }),
      node('l1', { name: 'learner-1', voter: false }),
      node('p1', { name: 'probe-a', voter: false, role: 'probe' }),
      node('v1', { name: 'voter-1', leader: true }),
    ]);
    expect(sorted.map((n) => n.id)).toEqual(['v1', 'l1', 'p1', 'p2']);
  });

  it('does not modify the input array', () => {
    const input = [node('b'), node('a', { leader: true })];
    sortNodes(input);
    expect(input.map((n) => n.id)).toEqual(['b', 'a']);
  });
});

describe('nodeLabel', () => {
  it('prefers the name over the id', () => {
    expect(nodeLabel(node('id-1', { name: 'frankfurt-1' }))).toBe('frankfurt-1');
    expect(nodeLabel(node('id-1'))).toBe('id-1');
  });
});

describe('removalImpact', () => {
  it.each([
    // [voters, expected votersLeft, expected tolerated, expected riskKey]
    [2, 1, 0, 'admin.cluster.risk.noReserve'],
    [3, 2, 0, 'admin.cluster.risk.noReserve'],
    [4, 3, 1, null],
    [5, 4, 1, 'admin.cluster.risk.even'],
    [6, 5, 2, null],
  ])(
    'removing a follower from %i voters leaves %i',
    (voters, votersLeft, toleratesAfter, riskKey) => {
      const nodes = cluster(voters);
      const target = nodes.find((n) => !n.leader)!;
      expect(removalImpact(nodes, target)).toEqual({
        blockedKey: null,
        votersLeft,
        toleratesAfter,
        riskKey,
      });
    },
  );

  it('blocks the leader', () => {
    const nodes = cluster(3);
    const impact = removalImpact(nodes, nodes[0]);
    expect(impact.blockedKey).toBe('admin.cluster.blocked.leader');
  });

  it('blocks the last voter, a single node that is not marked leader', () => {
    const nodes = [node('solo', { leader: false })];
    expect(removalImpact(nodes, nodes[0]).blockedKey).toBe('admin.cluster.blocked.lastVoter');
  });

  it('blocks the leader of a single-voter cluster with the leader reason first', () => {
    const nodes = cluster(1);
    const impact = removalImpact(nodes, nodes[0]);
    expect(impact.blockedKey).toBe('admin.cluster.blocked.leader');
    expect(impact.votersLeft).toBe(0);
    expect(impact.toleratesAfter).toBe(0);
    expect(impact.riskKey).toBe('admin.cluster.risk.noReserve');
  });

  it('removing a non-voter does not change the voter count', () => {
    const nodes = cluster(3, 2);
    const impact = removalImpact(
      nodes,
      nodes.find((n) => !n.voter)!,
    );
    expect(impact).toEqual({
      blockedKey: null,
      votersLeft: 3,
      toleratesAfter: 1,
      riskKey: null,
    });
  });

  it('removing a non-voter from a two-voter cluster still reports the missing reserve', () => {
    const nodes = cluster(2, 1);
    const impact = removalImpact(
      nodes,
      nodes.find((n) => !n.voter)!,
    );
    expect(impact.votersLeft).toBe(2);
    expect(impact.riskKey).toBe('admin.cluster.risk.noReserve');
  });

  it('is not blocked for a voter that is neither leader nor last voter', () => {
    const nodes = cluster(2);
    expect(removalImpact(nodes, nodes[1]).blockedKey).toBeNull();
  });

  it('removing a probe has no quorum impact or risk warning even with two voters', () => {
    const probe = node('p1', { voter: false, role: 'probe' });
    const nodes = [...cluster(2), probe];
    expect(removalImpact(nodes, probe)).toEqual({
      blockedKey: null,
      votersLeft: 2,
      toleratesAfter: 0,
      riskKey: null,
    });
  });
});

describe('isProbe', () => {
  it('returns true only when role is probe', () => {
    expect(isProbe(node('p1', { role: 'probe', voter: false }))).toBe(true);
    expect(isProbe(node('v1', { role: 'voter' }))).toBe(false);
    expect(isProbe(node('v2'))).toBe(false);
  });
});

describe('healthOf', () => {
  it.each([
    [undefined, 'unknown'],
    [true, 'healthy'],
    [false, 'unhealthy'],
  ] as const)('healthy=%s is %s', (healthy, expected) => {
    expect(healthOf(node('n', { healthy }))).toBe(expected);
  });
});

describe('lagSeverity', () => {
  it.each([
    [undefined, 'none'],
    [0, 'ok'],
    [59_999, 'ok'],
    [60_000, 'warn'],
    [299_999, 'warn'],
    [300_000, 'critical'],
    [3_600_000, 'critical'],
  ] as const)('%s ms is %s', (ms, expected) => {
    expect(lagSeverity(ms)).toBe(expected);
  });
});

describe('formatDuration', () => {
  it.each([
    [0, '0 ms'],
    [850, '850 ms'],
    [999.9, '999 ms'],
    [1000, '1 s'],
    [12_000, '12 s'],
    [59_999, '59 s'],
    [60_000, '1 min'],
    [240_000, '4 min'],
    [3_599_999, '59 min'],
    [3_600_000, '1 h'],
    [7_200_000, '2 h'],
  ])('%s ms reads as "%s"', (ms, text) => {
    expect(formatDuration(ms)).toBe(text);
  });
  it.each([
    [0, '0 B'],
    [512, '512 B'],
    [1023, '1023 B'],
    [1024, '1 KB'],
    [1536, '1.5 KB'],
    [1024 * 1024, '1 MB'],
    [1.25 * 1024 * 1024, '1.3 MB'],
    [1024 * 1024 * 1024, '1 GB'],
    [2.5 * 1024 * 1024 * 1024, '2.5 GB'],
  ])('formatBytes(%s) reads as "%s"', (bytes, text) => {
    expect(formatBytes(bytes)).toBe(text);
  });
});

describe('clusterErrorKey', () => {
  it.each([
    [403, 'admin.error.forbidden'],
    [404, 'admin.cluster.error.notMember'],
    [409, 'admin.cluster.error.conflict'],
    [503, 'admin.error.unavailable'],
    [500, 'admin.error.generic'],
    [undefined, 'admin.error.generic'],
  ] as const)('status %s maps to %s', (status, key) => {
    expect(clusterErrorKey(status)).toBe(key);
  });
});

describe('statusOf', () => {
  it('reads the status of an HTTP error', () => {
    expect(statusOf(new HttpErrorResponse({ status: 503 }))).toBe(503);
  });

  it('returns undefined for other errors', () => {
    expect(statusOf(new Error('boom'))).toBeUndefined();
  });
});

describe('clockSeverity', () => {
  it.each([
    [undefined, 'none'],
    [0, 'ok'],
    [1_999, 'ok'],
    [-1_999, 'ok'],
    [2_000, 'warn'],
    [-2_000, 'warn'],
    [-90_000, 'warn'],
  ] as const)('%s ms is %s', (ms, want) => {
    expect(clockSeverity(ms)).toBe(want);
  });
});

describe('formatOffset', () => {
  it.each([
    [0, '+0 ms'],
    [-450, '-450 ms'],
    [2_300, '+2.3 s'],
    [-89_999, '-90.0 s'],
    [90_000, '+90.0 s'],
  ])('formats %s ms as %s', (ms, want) => {
    expect(formatOffset(ms)).toBe(want);
  });
});

describe('ssrfMismatch', () => {
  const node = (id: string, ssrfProtection?: boolean): ClusterNode => ({
    id,
    address: `${id}:7000`,
    voter: true,
    leader: false,
    self: false,
    ssrfProtection,
  });

  it.each([
    ['no node answered', [node('a'), node('b')], false],
    ['all nodes on', [node('a', true), node('b', true)], false],
    ['all nodes off', [node('a', false), node('b', false)], false],
    ['on and off', [node('a', true), node('b', false)], true],
    ['unanswered nodes are ignored', [node('a', true), node('b'), node('c', true)], false],
    ['off next to an unanswered node', [node('a', false), node('b'), node('c', true)], true],
  ])('%s', (_name, nodes, want) => {
    expect(ssrfMismatch(nodes)).toBe(want);
  });
});
