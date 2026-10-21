// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import {
  AbstractControl,
  NonNullableFormBuilder,
  ReactiveFormsModule,
  ValidationErrors,
  Validators,
} from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { Router, RouterLink } from '@angular/router';
import { TranslocoDirective } from '@jsverse/transloco';
import { AuthService } from '../../core/auth/auth.service';
import { authErrorKey } from './auth-error';

/** Minimum length enforced by the server (internal/auth/password.go). */
export const MIN_PASSWORD_LENGTH = 12;

function passwordsMatch(group: AbstractControl): ValidationErrors | null {
  const pw = group.get('password')?.value;
  const repeat = group.get('passwordRepeat')?.value;
  return pw && repeat && pw !== repeat ? { mismatch: true } : null;
}

/** First start: create the platform admin with the one-time setup token from the log (FA-AUTH-01). */
@Component({
  selector: 'app-setup-page',
  imports: [
    ReactiveFormsModule,
    MatFormFieldModule,
    MatInputModule,
    MatButtonModule,
    RouterLink,
    TranslocoDirective,
  ],
  templateUrl: './setup-page.html',
  styleUrl: './auth-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class SetupPage {
  private readonly auth = inject(AuthService);
  private readonly router = inject(Router);

  protected readonly minLength = MIN_PASSWORD_LENGTH;
  protected readonly busy = signal(false);
  protected readonly error = signal<{ key: string; retryAfter?: number } | null>(null);

  protected readonly form = inject(NonNullableFormBuilder).group(
    {
      setupToken: ['', Validators.required],
      username: ['', [Validators.required, Validators.minLength(3), Validators.maxLength(64)]],
      displayName: [''],
      password: ['', [Validators.required, Validators.minLength(MIN_PASSWORD_LENGTH)]],
      passwordRepeat: ['', Validators.required],
    },
    { validators: passwordsMatch },
  );

  protected async submit(): Promise<void> {
    if (this.form.invalid || this.busy()) {
      this.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    const v = this.form.getRawValue();
    try {
      await this.auth.setup(
        v.setupToken.trim(),
        v.username.trim(),
        v.password,
        v.displayName.trim(),
      );
      await this.router.navigateByUrl('/');
    } catch (err) {
      this.error.set(authErrorKey(err));
    } finally {
      this.busy.set(false);
    }
  }
}
