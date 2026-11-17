// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { NonNullableFormBuilder, ReactiveFormsModule, Validators } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { TranslocoDirective } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { AuthService as AuthApi } from '../../api/gen/endpoints/auth/auth.service';
import { AuthService } from '../../core/auth/auth.service';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import {
  MAX_PASSWORD_LENGTH,
  MIN_PASSWORD_LENGTH,
  PasswordFailure,
  passwordFailure,
  passwordsMatch,
} from './password';

@Component({
  selector: 'app-account-page',
  imports: [
    ReactiveFormsModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    Icon,
    PageHeader,
    TranslocoDirective,
  ],
  templateUrl: './account-page.html',
  styleUrl: './account-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class AccountPage {
  private readonly api = inject(AuthApi);
  private readonly auth = inject(AuthService);

  protected readonly me = this.auth.me;
  /** OIDC mode: the account name comes from the identity provider's claims, not the username. */
  protected readonly oidc = computed(() => this.auth.mode() === 'oidc');
  protected readonly displayName = this.auth.displayName;
  /**
   * Passwords of external identity providers are changed there, not here. In OIDC
   * mode the backend answers 409 to password changes, so the form stays hidden.
   */
  protected readonly localAuth = computed(
    () => this.auth.mode() === 'local' && this.me()?.authMode === 'local',
  );
  protected readonly minLength = MIN_PASSWORD_LENGTH;
  protected readonly maxLength = MAX_PASSWORD_LENGTH;

  protected readonly busy = signal(false);
  protected readonly changed = signal(false);
  protected readonly failure = signal<PasswordFailure | null>(null);

  protected readonly form = inject(NonNullableFormBuilder).group(
    {
      currentPassword: ['', Validators.required],
      newPassword: [
        '',
        [
          Validators.required,
          Validators.minLength(MIN_PASSWORD_LENGTH),
          Validators.maxLength(MAX_PASSWORD_LENGTH),
        ],
      ],
      repeatPassword: ['', Validators.required],
    },
    { validators: passwordsMatch },
  );

  protected async submit(): Promise<void> {
    if (this.busy()) {
      return;
    }
    this.changed.set(false);
    this.failure.set(null);
    if (this.form.invalid) {
      this.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    const { currentPassword, newPassword } = this.form.getRawValue();
    try {
      // withCredentials: the server rotates the refresh cookie in its response.
      await firstValueFrom(
        this.api.changeMyPassword({ currentPassword, newPassword }, { withCredentials: true }),
      );
      this.form.reset();
      this.changed.set(true);
    } catch (err) {
      this.failure.set(passwordFailure(err));
    } finally {
      this.busy.set(false);
    }
  }
}
