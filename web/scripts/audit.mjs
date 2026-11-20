// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// CI gate around `npm audit`: fails on high or critical advisories, except
// the ones listed in audit-exceptions.json. Every exception names the
// advisory, the reason and an expiry date; an expired exception fails again,
// and so does an exception for an advisory that is no longer reported.
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

const LEVELS = new Set(['high', 'critical']);
const exceptions = JSON.parse(readFileSync(new URL('./audit-exceptions.json', import.meta.url)));
const today = new Date().toISOString().slice(0, 10);

let raw;
try {
  raw = execFileSync('npm', ['audit', '--json'], { encoding: 'utf8', maxBuffer: 64 << 20 });
} catch (err) {
  // npm audit exits non-zero when it finds anything; the JSON is still on stdout.
  raw = err.stdout;
}
if (!raw) {
  console.error('npm audit produced no output');
  process.exit(1);
}
const report = JSON.parse(raw);
if (report.error) {
  console.error('npm audit failed:', report.error.summary ?? report.error);
  process.exit(1);
}

// Root advisories only: transitive entries ("via" by package name) repeat them.
const advisories = new Map();
for (const vuln of Object.values(report.vulnerabilities ?? {})) {
  for (const via of vuln.via) {
    if (typeof via === 'object' && LEVELS.has(via.severity)) {
      const id = via.url.split('/').pop();
      advisories.set(id, { ...via, id });
    }
  }
}

let failed = false;
for (const adv of advisories.values()) {
  const ex = exceptions.find((e) => e.advisory === adv.id);
  if (!ex) {
    console.error(`FAIL ${adv.id} (${adv.severity}) ${adv.name} ${adv.range}: ${adv.title}`);
    failed = true;
  } else if (ex.expires < today) {
    console.error(`FAIL ${adv.id}: the exception expired on ${ex.expires}; review it`);
    failed = true;
  } else {
    console.log(`allowed ${adv.id} ${adv.name} until ${ex.expires}: ${ex.reason}`);
  }
}
for (const ex of exceptions) {
  if (!advisories.has(ex.advisory)) {
    console.error(`FAIL ${ex.advisory}: no longer reported; remove the exception`);
    failed = true;
  }
}
if (failed) {
  process.exit(1);
}
console.log(`npm audit: no unexcepted high or critical advisories (${advisories.size} excepted)`);
