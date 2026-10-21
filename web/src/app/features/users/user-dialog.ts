// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { NonNullableFormBuilder, ReactiveFormsModule, Validators } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MAT_DIALOG_DATA, MatDialogModule, MatDialogRef } from '@angular/material/dialog';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { MatSelectModule } from '@angular/material/select';
import { MatSlideToggleModule } from '@angular/material/slide-toggle';
import { TranslocoDirective } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { UsersService } from '../../api/gen/endpoints/users/users.service';
import { Role, User } from '../../api/gen/model';
import { applyFieldErrors, problemOf } from '../../shared/problem';
import {
  MAX_PASSWORD_LENGTH,
  MIN_PASSWORD_LENGTH,
  passwordsMatch,
  updatePatch,
  userErrorKey,
  UserRights,
} from './user-rules';

/** Input of the dialog: create a user, or edit one with the actor's rights on it. */
export interface UserDialogData {
  mode: 'create' | 'edit';
  /** Roles the actor may assign. */
  assignable: readonly Role[];
  user?: User;
  rights?: UserRights;
}

/** Create or edit a user. Closes with `true` if the server saved a change. */
@Component({
  selector: 'app-user-dialog',
  imports: [
    ReactiveFormsModule,
    MatDialogModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    MatSelectModule,
    MatSlideToggleModule,
    TranslocoDirective,
  ],
  templateUrl: './user-dialog.html',
  styleUrl: './user-dialog.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class UserDialog {
  private readonly api = inject(UsersService);
  private readonly ref = inject<MatDialogRef<UserDialog, boolean>>(MatDialogRef);
  protected readonly data = inject<UserDialogData>(MAT_DIALOG_DATA);

  protected readonly create = this.data.mode === 'create';
  protected readonly user = this.data.user;
  protected readonly rights = this.data.rights;
  protected readonly lockKey = this.rights?.lockKey ?? null;
  protected readonly minLength = MIN_PASSWORD_LENGTH;
  protected readonly maxLength = MAX_PASSWORD_LENGTH;

  /** The current role stays selectable even if it is no longer assignable. */
  protected readonly roleOptions: Role[] = (() => {
    const options = [...this.data.assignable];
    if (this.user && !options.includes(this.user.role)) {
      options.push(this.user.role);
    }
    return options;
  })();

  protected readonly busy = signal(false);
  protected readonly error = signal<string | null>(null);

  protected readonly form = inject(NonNullableFormBuilder).group(
    {
      username: [
        { value: this.user?.username ?? '', disabled: !this.create },
        [Validators.required, Validators.minLength(3), Validators.maxLength(64)],
      ],
      displayName: [this.user?.displayName ?? '', [Validators.maxLength(128)]],
      role: [(this.user?.role ?? 'tenant-viewer') as Role, [Validators.required]],
      disabled: [this.user?.disabled ?? false],
      password: [
        '',
        this.create
          ? [
              Validators.required,
              Validators.minLength(MIN_PASSWORD_LENGTH),
              Validators.maxLength(MAX_PASSWORD_LENGTH),
            ]
          : [Validators.minLength(MIN_PASSWORD_LENGTH), Validators.maxLength(MAX_PASSWORD_LENGTH)],
      ],
      passwordRepeat: [''],
    },
    { validators: passwordsMatch },
  );

  constructor() {
    if (this.rights && !this.rights.canChangeRole) {
      this.form.controls.role.disable();
    }
    if (this.rights && !this.rights.canDisable) {
      this.form.controls.disabled.disable();
    }
    if (!this.create && !this.rights?.canResetPassword) {
      this.form.controls.password.disable();
      this.form.controls.passwordRepeat.disable();
    }
  }

  protected async submit(): Promise<void> {
    if (this.busy()) {
      return;
    }
    this.error.set(null);
    if (this.form.invalid) {
      this.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    const v = this.form.getRawValue();
    try {
      if (this.create) {
        await firstValueFrom(
          this.api.createUser({
            username: v.username.trim(),
            displayName: v.displayName.trim() || undefined,
            password: v.password,
            role: v.role,
          }),
        );
        this.ref.close(true);
        return;
      }
      if (!this.user || !this.rights) {
        this.ref.close(false);
        return;
      }
      const patch = updatePatch(this.user, v, this.rights);
      if (Object.keys(patch).length === 0) {
        this.ref.close(false);
        return;
      }
      await firstValueFrom(this.api.updateUser(this.user.id, patch));
      this.ref.close(true);
    } catch (err) {
      this.error.set(userErrorKey(err));
      const problem = problemOf(err);
      if (problem?.errors?.length) {
        applyFieldErrors(this.form, problem);
      }
    } finally {
      this.busy.set(false);
    }
  }
}
