// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { Routes } from '@angular/router';

/** Notification channels: list (`/channels`), creation (`/channels/new`) and editing (`/channels/:channelId`). */
export const routes: Routes = [
  {
    path: '',
    loadComponent: () => import('./channels-page').then((m) => m.ChannelsPage),
  },
  {
    path: 'new',
    loadComponent: () => import('./channel-editor-page').then((m) => m.ChannelEditorPage),
  },
  {
    path: ':channelId',
    loadComponent: () => import('./channel-editor-page').then((m) => m.ChannelEditorPage),
  },
];
