// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

/**
 * Build version of this SPA. TODO: the release build should inject the version
 * (for example a generated file written by `make web-build` from `git describe`).
 * The Makefile is not wired for that yet, so the value stays `dev`, which is not
 * comparable and therefore never shows the notice.
 */
export const SPA_VERSION = 'dev';

/** `v1.2.3`, `1.2.3-rc.1`, `v0.7.1-3-gabc1234-dirty`: major and minor are compared. */
const RELEASE = /^v?(\d+)\.(\d+)(?:\.\d+)?(?:[-+].*)?$/;

/**
 * Whether the node and the frontend differ in major or minor version (ADR 0036,
 * rolling updates). Patch level, pre-release and build metadata are ignored.
 * Unknown or non-release versions such as `dev` or an empty string never mismatch.
 */
export function versionMismatch(spa: string, node: string): boolean {
  const a = parseMajorMinor(spa);
  const b = parseMajorMinor(node);
  if (a === null || b === null) {
    return false;
  }
  return a.major !== b.major || a.minor !== b.minor;
}

function parseMajorMinor(version: string): { major: number; minor: number } | null {
  const match = RELEASE.exec(version.trim());
  if (match === null) {
    return null;
  }
  return { major: Number(match[1]), minor: Number(match[2]) };
}
