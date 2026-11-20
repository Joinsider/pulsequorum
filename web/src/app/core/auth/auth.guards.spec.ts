// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import {
  ActivatedRouteSnapshot,
  CanActivateFn,
  Route,
  RouterStateSnapshot,
  UrlTree,
  provideRouter,
} from '@angular/router';
import { afterEach, describe, expect, it } from 'vitest';
import { routes } from '../../app.routes';
import { AuthService } from './auth.service';

const ROUTE = {} as ActivatedRouteSnapshot;
const STATE = { url: '/' } as RouterStateSnapshot;

/** Finds a route in the app table, with the shell's children searched too. */
function routeConfig(path: string): Route {
  const shell = routes.find((r) => r.path === '');
  const found =
    routes.find((r) => r.path === path) ?? shell?.children?.find((r) => r.path === path);
  if (!found) {
    throw new Error(`no route ${path}`);
  }
  return found;
}

/** Runs the route's synchronous guards in order, as the router does; true if all let it through. */
function reachable(path: string): boolean | UrlTree {
  const guards = (routeConfig(path).canActivate ?? []) as CanActivateFn[];
  for (const guard of guards) {
    const result = TestBed.runInInjectionContext(() => guard(ROUTE, STATE));
    if (result !== true) {
      return result as boolean | UrlTree;
    }
  }
  return true;
}

describe('local-only and admin routes', () => {
  function configure(mode: 'local' | 'oidc', canAdmin: boolean) {
    TestBed.configureTestingModule({
      providers: [
        provideRouter([]),
        {
          provide: AuthService,
          useValue: {
            mode: signal(mode).asReadonly(),
            canAdmin: signal(canAdmin).asReadonly(),
          },
        },
      ],
    });
  }

  afterEach(() => TestBed.resetTestingModule());

  it('lets an admin reach the users page in local mode', () => {
    configure('local', true);
    expect(reachable('users')).toBe(true);
  });

  it('does not let an admin reach the users page in OIDC mode, and redirects to the dashboard', () => {
    configure('oidc', true);
    const result = reachable('users');
    expect(result).toBeInstanceOf(UrlTree);
    expect((result as UrlTree).toString()).toBe('/');
  });

  it('keeps the users page from non-admins in local mode', () => {
    configure('local', false);
    expect(reachable('users')).toBeInstanceOf(UrlTree);
  });

  it('does not let anyone reach the first-start setup page in OIDC mode', () => {
    configure('oidc', true);
    expect(reachable('setup')).toBeInstanceOf(UrlTree);
  });

  it('lets the setup page through in local mode', () => {
    configure('local', true);
    const guards = (routeConfig('setup').canActivate ?? []) as CanActivateFn[];
    const first = TestBed.runInInjectionContext(() => guards[0](ROUTE, STATE));
    expect(first).toBe(true);
  });
});
