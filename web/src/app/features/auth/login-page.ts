// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, computed, inject, input, signal } from '@angular/core';
import { NonNullableFormBuilder, ReactiveFormsModule, Validators } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { Router, RouterLink } from '@angular/router';
import { TranslocoDirective } from '@jsverse/transloco';
import { AuthService, safeReturnUrl } from '../../core/auth/auth.service';
import { authErrorKey } from './auth-error';

@Component({
  selector: 'app-login-page',
  imports: [
    ReactiveFormsModule,
    MatFormFieldModule,
    MatInputModule,
    MatButtonModule,
    RouterLink,
    TranslocoDirective,
  ],
  templateUrl: './login-page.html',
  styleUrl: './auth-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class LoginPage {
  private readonly auth = inject(AuthService);
  private readonly router = inject(Router);

  /** Query parameters (router input binding). */
  readonly returnUrl = input<string>();
  readonly expired = input<string>();

  /** In OIDC mode the identity provider takes the credentials; the page offers one button. */
  protected readonly oidc = computed(() => this.auth.mode() === 'oidc');
  protected readonly busy = signal(false);
  protected readonly error = signal<{ key: string; retryAfter?: number } | null>(null);

  protected readonly form = inject(NonNullableFormBuilder).group({
    username: ['', Validators.required],
    password: ['', Validators.required],
  });

  protected signIn(): void {
    void this.auth.login(this.returnUrl());
  }

  protected async submit(): Promise<void> {
    if (this.form.invalid || this.busy()) {
      this.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    this.error.set(null);
    const { username, password } = this.form.getRawValue();
    try {
      await this.auth.login(username, password);
      await this.router.navigateByUrl(safeReturnUrl(this.returnUrl()));
    } catch (err) {
      this.form.controls.password.reset();
      this.error.set(authErrorKey(err));
    } finally {
      this.busy.set(false);
    }
  }
}
