// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Routes } from '@angular/router';
import { provideEchartsCore } from 'ngx-echarts';

export const routes: Routes = [
  {
    path: '',
    providers: [
      // ECharts is loaded on first use and tree-shaken to the parts we need.
      provideEchartsCore({ echarts: () => import('./echarts-setup').then((m) => m.default) }),
    ],
    loadComponent: () => import('./monitor-detail-page').then((m) => m.MonitorDetailPage),
  },
];
