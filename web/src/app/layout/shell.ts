// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { BreakpointObserver } from '@angular/cdk/layout';
import { MatMenuModule } from '@angular/material/menu';
import { MatButtonModule } from '@angular/material/button';
import { MatDrawerMode, MatSidenavModule } from '@angular/material/sidenav';
import { MatListModule } from '@angular/material/list';
import { MatToolbarModule } from '@angular/material/toolbar';
import { MatTooltipModule } from '@angular/material/tooltip';
import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { TranslocoPipe, TranslocoService } from '@jsverse/transloco';
import { catchError, map, of } from 'rxjs';
import { SystemService } from '../api/gen/endpoints/system/system.service';
import { AuthService } from '../core/auth/auth.service';
import { LANGUAGES, Language, storeLanguage } from '../core/i18n/language';
import { LiveStreamService } from '../core/stream/live-stream.service';
import { Icon } from '../shared/icon/icon';
import { IconName } from '../shared/icon/icons';
import { ThemeChoice, applyTheme, nextTheme, readStoredTheme, storeTheme } from './theme';

/** Public source code of the AGPL-licensed project (RB-LIC-04). */
export const SOURCE_URL = 'https://github.com/Joinsider/uptime-cluster';

/** Below this width the navigation is an overlay instead of a side panel. */
const WIDE_QUERY = '(min-width: 960px)';

interface NavItem {
  path: string;
  labelKey: string;
  icon: IconName;
  adminOnly?: boolean;
  /** Platform admins only (cluster administration, FA-UI-05). */
  platformOnly?: boolean;
  /** Only with local password accounts (ADR 0008); hidden in OIDC mode. */
  localOnly?: boolean;
}

const NAV_ITEMS: readonly NavItem[] = [
  { path: '/', labelKey: 'nav.dashboard', icon: 'dashboard' },
  { path: '/incidents', labelKey: 'nav.incidents', icon: 'warning' },
  { path: '/channels', labelKey: 'nav.channels', icon: 'notifications' },
  { path: '/maintenance', labelKey: 'nav.maintenance', icon: 'build' },
  { path: '/users', labelKey: 'nav.users', icon: 'group', adminOnly: true, localOnly: true },
  { path: '/admin/cluster', labelKey: 'nav.cluster', icon: 'hub', platformOnly: true },
  {
    path: '/admin/system-channels',
    labelKey: 'nav.systemChannels',
    icon: 'notifications',
    platformOnly: true,
  },
  { path: '/account', labelKey: 'nav.account', icon: 'person' },
];

const THEME_ICON: Record<ThemeChoice, IconName> = {
  system: 'contrast',
  light: 'lightMode',
  dark: 'darkMode',
};

/**
 * Application frame for signed-in pages: toolbar, navigation drawer and content
 * area. The drawer is a side panel from 960 px and an overlay below.
 */
@Component({
  selector: 'app-shell',
  imports: [
    RouterLink,
    RouterLinkActive,
    RouterOutlet,
    MatButtonModule,
    MatListModule,
    MatMenuModule,
    MatSidenavModule,
    MatToolbarModule,
    MatTooltipModule,
    TranslocoPipe,
    Icon,
  ],
  templateUrl: './shell.html',
  styleUrl: './shell.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class Shell {
  protected readonly auth = inject(AuthService);
  /** Injected so the live stream starts once the user is signed in. */
  protected readonly live = inject(LiveStreamService);
  private readonly transloco = inject(TranslocoService);
  private readonly system = inject(SystemService);
  private readonly breakpoints = inject(BreakpointObserver);

  protected readonly navItems = NAV_ITEMS;
  protected readonly languages = LANGUAGES;
  protected readonly sourceUrl = SOURCE_URL;
  protected readonly liveState = this.live.state;
  protected readonly lang = this.transloco.activeLang;

  protected readonly wide = toSignal(
    this.breakpoints.observe(WIDE_QUERY).pipe(map((state) => state.matches)),
    { initialValue: false },
  );
  protected readonly sidenavMode = computed<MatDrawerMode>(() => (this.wide() ? 'side' : 'over'));
  /** Collapsed state on wide screens, open state on narrow ones. */
  private readonly collapsed = signal(false);
  private readonly drawerOpen = signal(false);
  protected readonly navOpened = computed(() =>
    this.wide() ? !this.collapsed() : this.drawerOpen(),
  );

  protected readonly theme = signal<ThemeChoice>(readStoredTheme());
  protected readonly themeIcon = computed(() => THEME_ICON[this.theme()]);
  /** Emits when the active language's translations are loaded, so labels built in code update too. */
  private readonly translations = toSignal(this.transloco.selectTranslation(), {
    initialValue: null,
  });
  protected readonly themeLabel = computed(() => {
    this.translations();
    return this.transloco.translate('shell.theme', {
      mode: this.transloco.translate(`shell.themes.${this.theme()}`),
    });
  });

  protected readonly version = toSignal(
    this.system.getHealth().pipe(
      map((health) => health.version),
      catchError(() => of(null)),
    ),
    { initialValue: null },
  );

  /** Whether the navigation entry is offered to the current user; the server enforces every permission. */
  protected isVisible(item: NavItem): boolean {
    return (
      (!item.adminOnly || this.auth.canAdmin()) &&
      (!item.platformOnly || this.auth.canPlatformAdmin()) &&
      (!item.localOnly || this.auth.mode() === 'local')
    );
  }

  protected toggleNav(): void {
    if (this.wide()) {
      this.collapsed.update((c) => !c);
    } else {
      this.drawerOpen.update((o) => !o);
    }
  }

  protected onOpenedChange(opened: boolean): void {
    if (this.wide()) {
      this.collapsed.set(!opened);
    } else {
      this.drawerOpen.set(opened);
    }
  }

  /** Closes the overlay after navigating on narrow screens. */
  protected onNavigate(): void {
    if (!this.wide()) {
      this.drawerOpen.set(false);
    }
  }

  protected cycleTheme(): void {
    const next = nextTheme(this.theme());
    this.theme.set(next);
    applyTheme(next);
    storeTheme(next);
  }

  protected setLanguage(lang: Language): void {
    void this.transloco.setActiveLang(lang);
    storeLanguage(lang);
  }
}
