// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Generates the Angular API client from the OpenAPI contract (ADR 0003).
// Run `make api-gen` from the repository root.
import { defineConfig } from 'orval';

export default defineConfig({
  pulsequorum: {
    input: { target: '../api/openapi.yaml' },
    output: {
      mode: 'tags-split',
      client: 'angular',
      target: 'src/app/api/gen/endpoints',
      schemas: 'src/app/api/gen/model',
      baseUrl: '/api/v1',
      prettier: true,
      clean: true,
    },
  },
});
