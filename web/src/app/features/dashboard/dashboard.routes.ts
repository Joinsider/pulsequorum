// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Component } from '@angular/core';
import { Routes } from '@angular/router';

/** Placeholder until the feature is implemented (AP-1.14). */
@Component({ template: '<p>dashboard</p>' })
class Placeholder {}

export const routes: Routes = [{ path: '', component: Placeholder }];
