// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  OnInit,
  computed,
  inject,
  signal,
} from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { MatTooltipModule } from '@angular/material/tooltip';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { ClusterService } from '../../api/gen/endpoints/cluster/cluster.service';
import { ClusterNode, ClusterNodeList, NodeRemoval } from '../../api/gen/model';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { problemOf } from '../../shared/problem';
import { openJoinTokenDialog } from './join-token-dialog';
import { openRemoveNodeDialog, RemoveNodeDialogData } from './remove-node-dialog';
import { SystemAlertsSection } from './system-alerts-section';
import {
  clusterErrorKey,
  faultTolerance,
  formatBytes,
  formatDuration,
  healthOf,
  isProbe,
  LagSeverity,
  ClockSeverity,
  clockSeverity,
  formatOffset,
  lagSeverity,
  NodeHealth,
  quorumOf,
  removalImpact,
  RemovalImpact,
  sortNodes,
  ssrfMismatch,
  statusOf,
} from './cluster-rules';

/** Interval of the background reload while the tab is visible. */
export const POLL_INTERVAL_MS = 10_000;

/** One node card: the node plus everything the template needs about it. */
export interface NodeRow {
  readonly node: ClusterNode;
  readonly name: string;
  readonly isProbe: boolean;
  readonly impact: RemovalImpact;
  readonly health: NodeHealth;
  readonly lagSeverity: LagSeverity;
  /** Formatted lag, or null when there is none to show ("–"). */
  readonly lagText: string | null;
  readonly clockSeverity: ClockSeverity;
  /** Signed clock offset, or null while no fresh measurement exists ("–"). */
  readonly clockText: string | null;
  /** Display name or short id of the voter a probe is connected to. */
  readonly connectedVoterLabel: string | null;
  /** Formatted probe buffer status, or null when absent. */
  readonly bufferMetrics: {
    readonly unacked: number;
    readonly bytesText: string;
    readonly oldestText: string | null;
    readonly droppedTotal: number;
  } | null;
}

/** Summary figures above the node list. */
export interface ClusterSummary {
  readonly voters: number;
  readonly probes: number;
  readonly quorum: number;
  readonly tolerates: number;
  /** Leader name or id; null while no leader is known. */
  readonly leaderLabel: string | null;
  readonly commandLevel: number | null;
}

/** An error of the last action, shown above the node list. */
export interface ActionError {
  readonly key: string;
  readonly detail: string | null;
}

/** Shortens a node id for display when the node has no name. */
export function shortId(id: string): string {
  return id.slice(0, 8);
}

/** Name for display: the registry name, or the short id until the node registered. */
export function displayName(node: ClusterNode): string {
  return node.name || shortId(node.id);
}

/**
 * Platform admin view of the cluster (FA-UI-05, FA-CLU-08, FA-CLU-11): members,
 * quorum, adding a voter, removing and replacing nodes. Refreshes every
 * 10 seconds while the tab is visible.
 */
@Component({
  selector: 'app-cluster-page',
  imports: [
    MatButtonModule,
    MatTooltipModule,
    Icon,
    PageHeader,
    SystemAlertsSection,
    TranslocoDirective,
  ],
  templateUrl: './cluster-page.html',
  styleUrl: './cluster-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class ClusterPage implements OnInit {
  private readonly api = inject(ClusterService);
  private readonly dialog = inject(MatDialog);
  private readonly snack = inject(MatSnackBar);
  private readonly transloco = inject(TranslocoService);
  private readonly destroyRef = inject(DestroyRef);

  /** Last successful list; kept during reloads and after a failed poll. */
  protected readonly data = signal<ClusterNodeList | null>(null);
  protected readonly loading = signal(true);
  /** Translation key of a failed list call, or null. */
  protected readonly loadError = signal<string | null>(null);
  protected readonly actionError = signal<ActionError | null>(null);
  /** True while a removal or replacement runs; disables all node actions. */
  protected readonly busy = signal(false);

  private loadInFlight = false;

  protected readonly rows = computed<NodeRow[]>(() => {
    const data = this.data();
    if (!data) {
      return [];
    }
    const nodeById = new Map(data.nodes.map((n) => [n.id, n]));
    return sortNodes(data.nodes).map((node) => {
      const lagMs = node.replicationLagMs;
      let connectedVoterLabel: string | null = null;
      if (node.connectedVoterId) {
        const voter = nodeById.get(node.connectedVoterId);
        connectedVoterLabel = voter ? displayName(voter) : shortId(node.connectedVoterId);
      }
      const buf = node.probeBuffer;
      const bufferMetrics = buf
        ? {
            unacked: buf.unacked,
            bytesText: formatBytes(buf.bytes),
            oldestText:
              buf.oldestUnackedMs !== undefined ? formatDuration(buf.oldestUnackedMs) : null,
            droppedTotal: buf.droppedTotal,
          }
        : null;
      return {
        node,
        name: displayName(node),
        isProbe: isProbe(node),
        impact: removalImpact(data.nodes, node),
        health: healthOf(node),
        lagSeverity: lagSeverity(lagMs),
        lagText: lagMs === undefined ? null : formatDuration(lagMs),
        clockSeverity: clockSeverity(node.clockOffsetMs),
        clockText: node.clockOffsetMs === undefined ? null : formatOffset(node.clockOffsetMs),
        connectedVoterLabel,
        bufferMetrics,
      };
    });
  });

  /** True while the answered nodes report different SSRF settings (ADR 0030). */
  protected readonly ssrfDiffers = computed(() => {
    const data = this.data();
    return data ? ssrfMismatch(data.nodes) : false;
  });

  protected readonly summary = computed<ClusterSummary | null>(() => {
    const data = this.data();
    if (!data) {
      return null;
    }
    const voters = data.nodes.filter((n) => n.voter).length;
    const probes = data.nodes.filter(isProbe).length;
    const leader = data.nodes.find((n) => n.leader);
    let leaderLabel: string | null = null;
    if (data.leaderId) {
      leaderLabel = leader ? displayName(leader) : data.leaderId;
    }
    return {
      voters,
      probes,
      quorum: quorumOf(voters),
      tolerates: faultTolerance(voters),
      leaderLabel,
      commandLevel: data.commandLevel ?? null,
    };
  });

  ngOnInit(): void {
    void this.load();
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') {
        void this.load();
      }
    }, POLL_INTERVAL_MS);
    this.destroyRef.onDestroy(() => clearInterval(timer));
  }

  /** Reloads the member list. Skips the call if one is already running. */
  protected async load(): Promise<void> {
    if (this.loadInFlight) {
      return;
    }
    this.loadInFlight = true;
    try {
      this.data.set(await firstValueFrom(this.api.listClusterNodes()));
      this.loadError.set(null);
    } catch (err) {
      this.loadError.set(clusterErrorKey(statusOf(err)));
    } finally {
      this.loadInFlight = false;
      this.loading.set(false);
    }
  }

  protected async addVoter(): Promise<void> {
    const saved = await openJoinTokenDialog(this.dialog, { joinAddress: this.joinAddress() });
    if (saved) {
      await this.load();
    }
  }

  protected async remove(row: NodeRow): Promise<void> {
    if (row.impact.blockedKey || this.busy()) {
      return;
    }
    const confirmed = await openRemoveNodeDialog(this.dialog, this.dialogData('remove', row));
    if (!confirmed) {
      return;
    }
    const removal = await this.removeNode(row);
    if (removal) {
      this.notifyRemoved(removal);
    }
    await this.load();
  }

  /** Removes the node first; only after success the join token for its successor is issued. */
  protected async replace(row: NodeRow): Promise<void> {
    if (row.impact.blockedKey || this.busy()) {
      return;
    }
    const confirmed = await openRemoveNodeDialog(this.dialog, this.dialogData('replace', row));
    if (!confirmed) {
      return;
    }
    const removal = await this.removeNode(row);
    await this.load();
    if (!removal) {
      return;
    }
    const saved = await openJoinTokenDialog(this.dialog, {
      joinAddress: this.joinAddress(row.node.id),
      hintKey: 'admin.cluster.replace.joinHint',
    });
    if (saved) {
      await this.load();
    }
  }

  /** Runs the removal call; on failure shows the error and returns null. */
  private async removeNode(row: NodeRow): Promise<NodeRemoval | null> {
    this.busy.set(true);
    this.actionError.set(null);
    try {
      return await firstValueFrom(this.api.removeClusterNode(row.node.id));
    } catch (err) {
      this.actionError.set({
        key: clusterErrorKey(statusOf(err)),
        detail: problemOf(err)?.detail ?? null,
      });
      return null;
    } finally {
      this.busy.set(false);
    }
  }

  /**
   * Address for a joining node: the leader, else the answering node, else any
   * voter. A node being removed is never suggested.
   */
  private joinAddress(excludeId?: string): string | undefined {
    const nodes = (this.data()?.nodes ?? []).filter((n) => n.id !== excludeId && n.address);
    const pick =
      nodes.find((n) => n.leader) ?? nodes.find((n) => n.self) ?? nodes.find((n) => n.voter);
    return pick?.address;
  }

  private dialogData(mode: 'remove' | 'replace', row: NodeRow): RemoveNodeDialogData {
    return {
      mode,
      nodeName: row.name,
      isProbe: row.isProbe,
      votersLeft: row.impact.votersLeft,
      toleratesAfter: row.impact.toleratesAfter,
      riskKey: row.impact.riskKey,
    };
  }

  private notifyRemoved(removal: NodeRemoval): void {
    const text = this.transloco.translate('admin.cluster.saved.removed', {
      count: removal.votersLeft,
    });
    this.snack.open(removal.warning ? `${text} ${removal.warning}` : text, undefined, {
      duration: removal.warning ? 10_000 : 5_000,
    });
  }
}
