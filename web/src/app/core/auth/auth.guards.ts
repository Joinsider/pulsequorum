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

/** Hides admin-only pages; this is convenience only, the server enforces roles (FA-UI-05). */
export const adminGuard: CanActivateFn = () => {
  const auth = inject(AuthService);
  return auth.canAdmin() ? true : inject(Router).createUrlTree(['/']);
};
