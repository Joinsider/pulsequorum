// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, OnInit, inject } from '@angular/core';
import { Router } from '@angular/router';
import { TranslocoDirective } from '@jsverse/transloco';
import { AuthService } from '../../core/auth/auth.service';

/**
 * Landing page of the identity provider redirect (`/auth/callback`). It
 * finishes the sign-in, then continues to the page the user came from.
 */
@Component({
  selector: 'app-auth-callback-page',
  imports: [TranslocoDirective],
  template: `
    <main class="auth-page" *transloco="let t">
      <section class="auth-card" role="status">
        <p class="auth-card__subtitle">{{ t('auth.callback.loading') }}</p>
      </section>
    </main>
  `,
  styleUrl: './auth-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class AuthCallbackPage implements OnInit {
  private readonly auth = inject(AuthService);
  private readonly router = inject(Router);

  ngOnInit(): void {
    void this.auth.restore().then(() => this.router.navigateByUrl(this.auth.takeReturnUrl()));
  }
}
