// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { MatTooltipModule } from '@angular/material/tooltip';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { UsersService } from '../../api/gen/endpoints/users/users.service';
import { User } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { confirm } from '../../shared/confirm-dialog';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { UserDialog, UserDialogData } from './user-dialog';
import { UserRights, assignableRoles, userErrorKey, userRights } from './user-rules';

/** One list row: the user, what the actor may do with it, and whether it is the actor. */
export interface UserRow {
  readonly user: User;
  readonly rights: UserRights;
  readonly self: boolean;
}

/** Tenant user management (FA-AUTH-09). Only reachable for admins (adminGuard). */
@Component({
  selector: 'app-users-page',
  imports: [
    MatButtonModule,
    MatTooltipModule,
    Icon,
    PageHeader,
    RelativeTimePipe,
    TranslocoDirective,
  ],
  templateUrl: './users-page.html',
  styleUrl: './users-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class UsersPage {
  private readonly api = inject(UsersService);
  private readonly auth = inject(AuthService);
  private readonly dialog = inject(MatDialog);
  private readonly snack = inject(MatSnackBar);
  private readonly transloco = inject(TranslocoService);

  private readonly me = this.auth.me;
  protected readonly users = signal<User[]>([]);
  protected readonly loading = signal(true);
  protected readonly loadError = signal<string | null>(null);

  protected readonly rows = computed<UserRow[]>(() => {
    const me = this.me();
    if (!me) {
      return [];
    }
    const all = this.users();
    return [...all]
      .sort((a, b) => a.username.localeCompare(b.username))
      .map((user) => ({
        user,
        rights: userRights(user, me, all),
        self: user.id === me.id,
      }));
  });

  constructor() {
    void this.load();
  }

  /** Reloads the tenant's users. Called on start, after changes and by the retry button. */
  protected async load(): Promise<void> {
    try {
      this.users.set(await firstValueFrom(this.api.listUsers()));
      this.loadError.set(null);
    } catch (err) {
      this.loadError.set(userErrorKey(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    const saved = await this.openDialog({
      mode: 'create',
      assignable: assignableRoles(this.me()?.roles ?? []),
    });
    if (saved) {
      this.notify('users.saved.created');
      await this.load();
    }
  }

  protected async edit(row: UserRow): Promise<void> {
    const saved = await this.openDialog({
      mode: 'edit',
      assignable: assignableRoles(this.me()?.roles ?? []),
      user: row.user,
      rights: row.rights,
    });
    if (saved) {
      this.notify('users.saved.updated');
      await this.load();
    }
  }

  protected async remove(row: UserRow): Promise<void> {
    const confirmed = await confirm(this.dialog, {
      titleKey: 'users.deleteTitle',
      messageKey: 'users.deleteText',
      confirmKey: 'common.delete',
      danger: true,
    });
    if (!confirmed) {
      return;
    }
    try {
      await firstValueFrom(this.api.deleteUser(row.user.id));
      this.notify('users.saved.deleted');
      await this.load();
    } catch (err) {
      this.notify(userErrorKey(err));
    }
  }

  private async openDialog(data: UserDialogData): Promise<boolean> {
    const saved = await firstValueFrom(
      this.dialog
        .open<UserDialog, UserDialogData, boolean>(UserDialog, {
          data,
          width: 'min(32rem, calc(100vw - 2rem))',
          autoFocus: 'first-tabbable',
          restoreFocus: true,
        })
        .afterClosed(),
      { defaultValue: false },
    );
    return saved === true;
  }

  private notify(key: string): void {
    this.snack.open(this.transloco.translate(key), undefined, { duration: 4000 });
  }
}
