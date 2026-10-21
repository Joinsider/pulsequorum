// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Component } from '@angular/core';
import { Routes } from '@angular/router';

/** Placeholder until the feature is implemented (AP-1.14). */
@Component({ template: '<p>monitor-editor</p>' })
class Placeholder {}

export const createRoutes: Routes = [{ path: '', component: Placeholder }];
export const editRoutes: Routes = [{ path: '', component: Placeholder }];
