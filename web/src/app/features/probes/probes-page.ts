// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ChangeDetectionStrategy,
  Component,
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
import { catchError, firstValueFrom, forkJoin, of } from 'rxjs';
import { ProbesService } from '../../api/gen/endpoints/probes/probes.service';
import { TenantService } from '../../api/gen/endpoints/tenant/tenant.service';
import { OwnTenant, PrivateProbe } from '../../api/gen/model';
import { confirm } from '../../shared/confirm-dialog';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { problemOf } from '../../shared/problem';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { formatBytes, formatDuration } from '../admin/cluster-rules';
import { openProbeTokenDialog } from './probe-token-dialog';

/** Node capability a probe announces while its local target policy (PROBE_ALLOW_TARGETS) is active (ADR 0044). */
const TARGET_POLICY_CAPABILITY = 'target-policy';

export interface PrivateProbeRow {
  readonly probe: PrivateProbe;
  /** False means the probe runs without a local target restriction ("any"). */
  readonly targetPolicy: boolean;
  readonly bufferMetrics: {
    readonly unacked: number;
    readonly bytesText: string;
    readonly oldestText: string | null;
    readonly droppedTotal: number;
  } | null;
}

/** Tenant-admin view for private probes and join token creation (FA-PRB-05, ADR 0035). */
@Component({
  selector: 'app-probes-page',
  imports: [
    MatButtonModule,
    MatTooltipModule,
    Icon,
    PageHeader,
    RelativeTimePipe,
    TranslocoDirective,
  ],
  templateUrl: './probes-page.html',
  styleUrl: './probes-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class ProbesPage implements OnInit {
  private readonly api = inject(ProbesService);
  private readonly tenantApi = inject(TenantService);
  private readonly dialog = inject(MatDialog);
  private readonly snack = inject(MatSnackBar);
  private readonly transloco = inject(TranslocoService);

  protected readonly probes = signal<PrivateProbe[] | null>(null);
  protected readonly tenant = signal<OwnTenant | null>(null);
  protected readonly loading = signal(true);
  protected readonly loadError = signal(false);
  protected readonly actionError = signal<string | null>(null);
  protected readonly busy = signal(false);

  protected readonly quota = computed<{
    used: number;
    max: number | null;
    reached: boolean;
  } | null>(() => {
    const t = this.tenant();
    if (!t) {
      return null;
    }
    const max = t.effectiveLimits.maxPrivateProbes ?? null;
    const used = t.usage.privateProbes ?? this.probes()?.length ?? 0;
    if (max === null && used === 0) {
      return null;
    }
    return { used, max, reached: max !== null && used >= max };
  });

  protected readonly rows = computed<PrivateProbeRow[]>(() =>
    (this.probes() ?? []).map((probe) => {
      const buf = probe.probeBuffer;
      return {
        probe,
        targetPolicy: probe.capabilities.includes(TARGET_POLICY_CAPABILITY),
        bufferMetrics: buf
          ? {
              unacked: buf.unacked,
              bytesText: formatBytes(buf.bytes),
              oldestText:
                buf.oldestUnackedMs !== undefined ? formatDuration(buf.oldestUnackedMs) : null,
              droppedTotal: buf.droppedTotal,
            }
          : null,
      };
    }),
  );

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    this.loadError.set(false);
    try {
      const res = await firstValueFrom(
        forkJoin({
          probes: this.api.listPrivateProbes(),
          tenant: this.tenantApi.getOwnTenant().pipe(catchError(() => of(null))),
        }),
      );
      this.probes.set(res.probes);
      this.tenant.set(res.tenant);
    } catch {
      this.loadError.set(true);
    } finally {
      this.loading.set(false);
    }
  }

  protected async createToken(): Promise<void> {
    this.actionError.set(null);
    if (await openProbeTokenDialog(this.dialog)) {
      await this.load();
    }
  }

  protected async remove(probe: PrivateProbe): Promise<void> {
    if (this.busy()) {
      return;
    }
    const ok = await confirm(this.dialog, {
      titleKey: 'probes.remove.title',
      messageKey: 'probes.remove.text',
      confirmKey: 'probes.remove.confirm',
      danger: true,
    });
    if (!ok) {
      return;
    }
    this.busy.set(true);
    this.actionError.set(null);
    try {
      await firstValueFrom(this.api.removePrivateProbe(probe.id));
      this.snack.open(this.transloco.translate('probes.removed'), undefined, { duration: 4000 });
      await this.load();
    } catch (err) {
      this.actionError.set(problemOf(err)?.detail ?? this.transloco.translate('common.error'));
    } finally {
      this.busy.set(false);
    }
  }
}
