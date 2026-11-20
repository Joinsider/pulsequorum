// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  inject,
  input,
  OnInit,
  signal,
} from '@angular/core';
import { NonNullableFormBuilder, ReactiveFormsModule } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { MatTooltipModule } from '@angular/material/tooltip';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { Observable, Subscription } from 'rxjs';
import { AuditService } from '../../api/gen/endpoints/audit/audit.service';
import { TenantAdminService } from '../../api/gen/endpoints/tenant-admin/tenant-admin.service';
import { AuditEntry, AuditPage as AuditPageResponse } from '../../api/gen/model';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import {
  AuditFilter,
  AuditScope,
  EMPTY_AUDIT_FILTER,
  actorKindKey,
  auditErrorKey,
  buildAuditParams,
  formatAuditTime,
  shortRequestId,
} from './audit-rules';

/**
 * Audit log (FA-DAT-01, ADR 0031). The `tenant` scope lists the caller's tenant
 * (`/audit`), the `platform` scope lists all tenants with a tenant filter
 * (`/admin/audit`). Entries carry no field values or secrets.
 */
@Component({
  selector: 'app-audit-page',
  imports: [
    ReactiveFormsModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    MatTooltipModule,
    Icon,
    PageHeader,
    RelativeTimePipe,
    TranslocoDirective,
  ],
  templateUrl: './audit-page.html',
  styleUrl: './audit-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class AuditPage implements OnInit {
  /** Set by the route data (`scope`, withComponentInputBinding). */
  readonly scope = input<AuditScope>('tenant');

  private readonly auditApi = inject(AuditService);
  private readonly adminApi = inject(TenantAdminService);
  private readonly transloco = inject(TranslocoService);
  private readonly destroyRef = inject(DestroyRef);
  private readonly fb = inject(NonNullableFormBuilder);

  protected readonly actorKindKey = actorKindKey;
  protected readonly shortRequestId = shortRequestId;

  protected readonly form = this.fb.group({
    tenant: '',
    action: '',
    objectKind: '',
    objectId: '',
    actor: '',
    from: '',
    to: '',
  });

  /** The filter the list currently shows; changes only on submit. */
  protected readonly filter = signal<AuditFilter>(EMPTY_AUDIT_FILTER);
  protected readonly entries = signal<AuditEntry[]>([]);
  protected readonly nextCursor = signal<string | null>(null);
  protected readonly loading = signal(true);
  protected readonly loadingMore = signal(false);
  /** Translation key of a failed first page, or null. */
  protected readonly loadError = signal<string | null>(null);
  /** Translation key of a failed "load more", or null. */
  protected readonly moreError = signal<string | null>(null);

  private current: Subscription | null = null;

  constructor() {
    this.destroyRef.onDestroy(() => this.current?.unsubscribe());
  }

  ngOnInit(): void {
    this.load();
  }

  /** Applies the form's filter and reloads the first page. */
  protected apply(): void {
    this.filter.set(this.form.getRawValue());
    this.load();
  }

  protected reset(): void {
    this.form.reset();
    this.apply();
  }

  /** Loads the first page of the current filter, replacing the list. */
  protected load(): void {
    this.current?.unsubscribe();
    this.loading.set(true);
    this.loadingMore.set(false);
    this.loadError.set(null);
    this.moreError.set(null);
    this.current = this.fetch(null).subscribe({
      next: (page) => {
        this.entries.set(page.items);
        this.nextCursor.set(page.nextCursor ?? null);
        this.loading.set(false);
      },
      error: (err: unknown) => {
        this.entries.set([]);
        this.nextCursor.set(null);
        this.loading.set(false);
        this.loadError.set(auditErrorKey(err));
      },
    });
  }

  /** Appends the next page behind the cursor of the last one. */
  protected loadMore(): void {
    const cursor = this.nextCursor();
    if (!cursor || this.loadingMore()) {
      return;
    }
    this.current?.unsubscribe();
    this.loadingMore.set(true);
    this.moreError.set(null);
    this.current = this.fetch(cursor).subscribe({
      next: (page) => {
        this.entries.update((list) => [...list, ...page.items]);
        this.nextCursor.set(page.nextCursor ?? null);
        this.loadingMore.set(false);
      },
      error: (err: unknown) => {
        this.loadingMore.set(false);
        this.moreError.set(auditErrorKey(err));
      },
    });
  }

  protected formatTime(iso: string): string {
    return formatAuditTime(iso, this.transloco.getActiveLang());
  }

  private fetch(cursor: string | null): Observable<AuditPageResponse> {
    if (this.scope() === 'platform') {
      return this.adminApi.listAdminAudit(buildAuditParams(this.filter(), cursor));
    }
    // The tenant is derived from the token on the server; the tenant field is platform-only.
    return this.auditApi.listAudit(buildAuditParams({ ...this.filter(), tenant: '' }, cursor));
  }
}
