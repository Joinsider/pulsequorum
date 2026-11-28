#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 The Pulsequorum Authors
#
# Gate around grype (ADR 0036 6B): fails on high or critical findings in a
# container image, except the ones listed in .github/image-scan-exceptions.json.
# Same rules as web/scripts/audit.mjs: every exception names the vulnerability,
# the reason and an expiry date; an expired exception fails again, and so does
# an exception for a vulnerability that is no longer reported.
#
# Usage: image-scan.sh IMAGE
#   GRYPE                  grype binary (default: grype)
#   IMAGE_SCAN_EXCEPTIONS  exception file (default: .github/image-scan-exceptions.json)
#   IMAGE_SCAN_TODAY       date for the expiry check, YYYY-MM-DD (default: today, UTC; for tests)
set -euo pipefail

image="${1:?usage: image-scan.sh IMAGE}"
grype="${GRYPE:-grype}"
exceptions="${IMAGE_SCAN_EXCEPTIONS:-$(dirname "$0")/../image-scan-exceptions.json}"
today="${IMAGE_SCAN_TODAY:-$(date -u +%F)}"

report="$(mktemp)"
trap 'rm -f "$report"' EXIT

# The image comes from the local Docker daemon; it is never pulled or pushed here.
"$grype" "docker:$image" --output json --quiet > "$report"

result="$(jq -r --slurpfile exfile "$exceptions" --arg today "$today" '
  def valid_date: type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$");
  def valid_exception:
    type == "object"
    and (.id | type == "string" and length > 0)
    and (.reason | type == "string" and length > 0)
    and (.expires | valid_date);

  ($exfile[0]) as $ex
  | if ($ex | type) != "array" then "FAIL exception file is not a JSON array"
    else
      [ .matches[]
        # The casing of severity depends on the data provider.
        | select(.vulnerability.severity | ascii_downcase | . == "high" or . == "critical")
        | { id: .vulnerability.id,
            severity: .vulnerability.severity,
            pkg: "\(.artifact.name) \(.artifact.version)",
            fix: ((.vulnerability.fix.versions // []) | join(", ")) } ]
      | unique_by([.id, .pkg]) as $found
      | ($found | map(.id) | unique) as $ids
      | ( $ex[] | select(valid_exception | not)
          | "FAIL invalid exception \(tojson): needs id, reason and expires (YYYY-MM-DD)" ),
        ( $found[] as $f
          | ([ $ex[] | select(valid_exception and .id == $f.id) ] | first) as $e
          | if $e == null then
              "FAIL \($f.id) (\($f.severity)) \($f.pkg), fixed in: \(if $f.fix == "" then "no fix yet" else $f.fix end)"
            elif $e.expires < $today then
              "FAIL \($f.id): the exception expired on \($e.expires); review it"
            else
              "allowed \($f.id) \($f.pkg) until \($e.expires): \($e.reason)"
            end ),
        ( $ex[] | select(valid_exception) | select(.id as $i | $ids | index($i) | not)
          | "FAIL \(.id): no longer reported; remove the exception" )
    end
' "$report")"

total="$(jq '.matches | length' "$report")"
if [[ -n "$result" ]]; then
  printf '%s\n' "$result"
fi
if grep -q '^FAIL' <<< "$result"; then
  echo "image scan of $image failed ($total findings in total, see above)" >&2
  exit 1
fi
echo "image scan of $image passed ($total findings in total, none high or critical without a valid exception)"
