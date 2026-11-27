// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { describe, expect, it } from 'vitest';
import { versionMismatch } from './version-check';

describe('versionMismatch', () => {
  it.each([
    ['1.2.3', '1.2.3', false, 'same version'],
    ['1.2.3', '1.2.9', false, 'patch level differs'],
    ['1.2.3', '1.3.0', true, 'minor differs'],
    ['1.2.3', '2.2.3', true, 'major differs'],
    ['v1.2.3', '1.2.0', false, 'v prefix is ignored'],
    ['v1.2.3', 'v1.2.0-rc.1', false, 'pre-release is ignored'],
    ['1.2.3', '1.2.3+build.7', false, 'build metadata is ignored'],
    ['0.7.1', 'v0.7.1-3-gabc1234-dirty', false, 'git describe output with a suffix'],
    ['0.7.1', 'v0.8.0-3-gabc1234', true, 'git describe output, minor differs'],
    ['1.2', '1.2.0', false, 'patch may be missing'],
    ['1.2.3', '1.2', false, 'node version without patch'],
    ['dev', '1.2.3', false, 'unknown frontend version'],
    ['1.2.3', 'dev', false, 'unknown node version'],
    ['1.2.3', '', false, 'empty node version'],
    ['', '1.2.3', false, 'empty frontend version'],
    ['1.2.3', 'abc1234', false, 'commit hash without a release tag'],
    ['1.2.3', ' 1.3.0 ', true, 'surrounding whitespace is trimmed'],
  ])('%s vs %s mismatch=%s (%s)', (spa, node, expected) => {
    expect(versionMismatch(spa, node)).toBe(expected);
  });
});
