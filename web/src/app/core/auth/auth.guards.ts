// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { inject } from '@angular/core';
import { CanActivateFn, Router } from '@angular/router';
import { AuthService } from './auth.service';

/** Lets signed-in users through and sends everyone else to the login. */
export const authGuard: CanActivateFn = async (_route, state) => {
  const auth = inject(AuthService);
  const router = inject(Router);
  await auth.restore();
  if (auth.authenticated()) {
    return true;
  }
  return router.createUrlTree(['/login'], {
    queryParams: state.url === '/' ? {} : { returnUrl: state.url },
  });
};

/** Keeps signed-in users away from the login and setup pages. */
export const guestGuard: CanActivateFn = async () => {
  const auth = inject(AuthService);
  await auth.restore();
  return auth.authenticated() ? inject(Router).createUrlTree(['/']) : true;
};

/**
 * Local password accounts only (ADR 0008): user management and the first-start
 * setup do not exist in OIDC mode, where the identity provider owns the accounts.
 * The server answers 409 there; this keeps the pages out of reach.
 */
export const localAuthGuard: CanActivateFn = () => {
  const auth = inject(AuthService);
  return auth.mode() === 'local' ? true : inject(Router).createUrlTree(['/']);
};

/** Hides admin-only pages; this is convenience only, the server enforces roles (FA-UI-05). */
export const adminGuard: CanActivateFn = () => {
  const auth = inject(AuthService);
  return auth.canAdmin() ? true : inject(Router).createUrlTree(['/']);
};

/** Hides the platform admin area (/admin); convenience only, the server enforces the role (FA-UI-05). */
export const platformAdminGuard: CanActivateFn = () => {
  const auth = inject(AuthService);
  return auth.canPlatformAdmin() ? true : inject(Router).createUrlTree(['/']);
};
