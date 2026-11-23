#!/usr/bin/env bash
# Runs the AS-07 cases (ADR 0030) against the environment started by setup.sh.
#
#   check.sh [on|off]    default: on; must match SSRF_PROTECTION of the running node
#
# Logs in through the REST API, creates one monitor or channel per case, reads
# the result, deletes it again and prints a PASS/FAIL table. Exit code 1 if any
# case fails. Tokens and passwords stay in shell variables and are never printed.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GEN="$HERE/.generated"
ADMIN_ENV="$GEN/admin.env"
BASE="http://127.0.0.1:18080/api/v1"
MODE="${1:-on}"
INTERVAL_S=20           # minimum monitor interval (openapi)
RESULT_TIMEOUT_S=90     # max wait for the first scheduled result of one case
REBIND_CHECKS=10        # scheduled results needed for case h
REBIND_TIMEOUT_S=420
CANARY_CONTAINER="ssrf-target"
CANARY_LOG="/var/log/canary.log"
readonly EXPECTED_SSRF_BLOCKED='DOWN ssrf_blocked'

die() { echo "check.sh: $*" >&2; exit 1; }
say() { echo "check.sh: $*" >&2; }

case "$MODE" in
  on | off) ;;
  *) die "usage: check.sh [on|off], got '$MODE'" ;;
esac
for tool in podman curl jq; do
  command -v "$tool" >/dev/null 2>&1 || die "required tool not found: $tool"
done
[[ -f "$ADMIN_ENV" ]] || die "missing $ADMIN_ENV; run setup.sh first"

actual_mode="$(podman inspect ssrf-node --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null |
  sed -n 's/^SSRF_PROTECTION=//p' || true)"
if [[ "$actual_mode" != "$MODE" ]]; then
  die "node runs with SSRF_PROTECTION=${actual_mode:-unknown}, check.sh was called with $MODE; run: setup.sh $MODE"
fi

# shellcheck disable=SC1090
source "$ADMIN_ENV"

TMP="$(mktemp -d)"
TOKEN=""
HTTP_CODE=""
BODY=""
LAST_ID=""
MON_IDS=""
CH_IDS=""
FAILS=0
RESULTS_FILE="$TMP/results"
: >"$RESULTS_FILE"

cleanup() {
  local id
  for id in $MON_IDS; do
    curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/monitors/$id" 2>/dev/null || true
  done
  for id in $CH_IDS; do
    curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/notifications/$id" 2>/dev/null || true
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

# api METHOD PATH [JSON]: sets HTTP_CODE and BODY.
api() {
  local method="$1" path="$2" payload="${3-}"
  local -a args=(-sS -o "$TMP/body" -w '%{http_code}' -X "$method" "$BASE$path")
  if [[ -n "$TOKEN" ]]; then
    args+=(-H "Authorization: Bearer $TOKEN")
  fi
  if [[ -n "$payload" ]]; then
    args+=(-H 'Content-Type: application/json' --data-binary @-)
    HTTP_CODE="$(printf '%s' "$payload" | curl "${args[@]}" 2>/dev/null)" || HTTP_CODE="000"
  else
    HTTP_CODE="$(curl "${args[@]}" 2>/dev/null)" || HTTP_CODE="000"
  fi
  BODY="$(cat "$TMP/body" 2>/dev/null || true)"
}

# pick ON_VALUE OFF_VALUE: the expectation for the current mode.
pick() {
  local on_value="$1" off_value="$2"
  if [[ "$MODE" == "on" ]]; then echo "$on_value"; else echo "$off_value"; fi
}

# record CASE CHECK EXPECTED ACTUAL: stores one row and counts failures.
record() {
  local case_id="$1" check="$2" expected="$3" actual="$4" verdict="FAIL"
  if matches "$actual" "$expected"; then verdict="PASS"; fi
  printf '%s\t%s\t%s\t%s\t%s\n' "$case_id" "$check" "$expected" "$actual" "$verdict" >>"$RESULTS_FILE"
  if [[ "$verdict" != "PASS" ]]; then
    FAILS=$((FAILS + 1))
  fi
  say "$case_id $verdict: $actual"
}

# record_bool CASE CHECK EXPECTED ACTUAL OK(0|1): for checks with their own pass rule.
record_bool() {
  local case_id="$1" check="$2" expected="$3" actual="$4" ok="$5" verdict="FAIL"
  if [[ "$ok" == "1" ]]; then verdict="PASS"; fi
  printf '%s\t%s\t%s\t%s\t%s\n' "$case_id" "$check" "$expected" "$actual" "$verdict" >>"$RESULTS_FILE"
  if [[ "$verdict" != "PASS" ]]; then
    FAILS=$((FAILS + 1))
  fi
  say "$case_id $verdict: $actual"
}

# matches ACTUAL EXPECTED: EXPECTED is an exact string, or "not:TEXT" meaning
# "ACTUAL does not contain TEXT".
matches() {
  local actual="$1" expected="$2"
  case "$expected" in
    not:*) [[ "$actual" != *"${expected#not:}"* ]] ;;
    *) [[ "$actual" == "$expected" ]] ;;
  esac
}

problem_fields() {
  jq -r '[.errors[]?.field] | join(",")' <<<"$BODY" 2>/dev/null || true
}

# create_monitor NAME TYPE CONFIG_JSON: sets HTTP_CODE, BODY and LAST_ID (on 201).
create_monitor() {
  local name="$1" type="$2" config_json="$3" payload
  payload="$(jq -n --arg n "$name" --arg t "$type" --argjson c "$config_json" --argjson i "$INTERVAL_S" \
    '{name: $n, type: $t, config: $c, intervalSeconds: $i, timeoutSeconds: 10}')"
  api POST /monitors "$payload"
  LAST_ID=""
  if [[ "$HTTP_CODE" == "201" ]]; then
    LAST_ID="$(jq -r '.id' <<<"$BODY")"
    MON_IDS="$MON_IDS $LAST_ID"
  fi
}

delete_monitor() {
  local id="$1"
  api DELETE "/monitors/$id"
}

# latest_heartbeat ID TIMEOUT_S: prints the newest heartbeat as JSON; fails on timeout.
latest_heartbeat() {
  local id="$1" timeout_s="$2" deadline hb
  deadline=$(($(date +%s) + timeout_s))
  while :; do
    api GET "/monitors/$id/heartbeats?limit=1000"
    hb="$(jq -c '.items | last // empty' <<<"$BODY" 2>/dev/null || true)"
    if [[ -n "$hb" ]]; then
      printf '%s\n' "$hb"
      return 0
    fi
    if (($(date +%s) >= deadline)); then
      return 1
    fi
    sleep 3
  done
}

# summary_of HEARTBEAT_JSON: "STATUS errorClass", or "STATUS -" without error class.
summary_of() {
  local hb_json="$1"
  jq -r '.status + " " + (.errorClass // "-")' <<<"$hb_json"
}

# result_case ID EXPECTED TYPE CONFIG_JSON LABEL: one monitor, first scheduled result.
result_case() {
  local id="$1" expected="$2" type="$3" config="$4" label="$5" hb actual="none"
  say "case $id: $label"
  create_monitor "ssrf-$id" "$type" "$config"
  if [[ -z "$LAST_ID" ]]; then
    record "$id" "$label" "$expected" "monitor not saved, HTTP $HTTP_CODE"
    return 0
  fi
  if hb="$(latest_heartbeat "$LAST_ID" "$RESULT_TIMEOUT_S")"; then
    actual="$(summary_of "$hb")"
  else
    actual="no result within ${RESULT_TIMEOUT_S}s"
  fi
  record "$id" "$label" "$expected" "$actual"
  delete_monitor "$LAST_ID"
}

# reject_case ID TYPE CONFIG_JSON FIELD EXPECT_REJECT(yes|no) LABEL
# With protection on, the target is rejected (422 at FIELD). Otherwise it is saved (201).
reject_case() {
  local id="$1" type="$2" config="$3" field="$4" expect_reject="$5" label="$6"
  local expected actual ok=0
  say "case $id: $label"
  create_monitor "ssrf-$id" "$type" "$config"
  if [[ "$expect_reject" == "yes" ]]; then
    expected="HTTP 422, field $field"
    actual="HTTP $HTTP_CODE, fields [$(problem_fields)]"
    if [[ "$HTTP_CODE" == "422" && ",$(problem_fields)," == *",$field,"* ]]; then ok=1; fi
  else
    expected="HTTP 201"
    actual="HTTP $HTTP_CODE"
    if [[ "$HTTP_CODE" == "201" ]]; then ok=1; fi
  fi
  record_bool "$id" "$label" "$expected" "$actual" "$ok"
  if [[ -n "$LAST_ID" ]]; then
    delete_monitor "$LAST_ID"
  fi
}

# canary_hits: requests the internal canary has logged so far.
canary_hits() {
  local n
  n="$(podman exec "$CANARY_CONTAINER" cat "$CANARY_LOG" 2>/dev/null | wc -l | tr -d ' ')"
  if ! [[ "$n" =~ ^[0-9]+$ ]]; then
    die "cannot read the canary log of $CANARY_CONTAINER"
  fi
  echo "$n"
}

# canary_case ID LABEL BEFORE AFTER: protection on means no hit at all, off means at least one.
canary_case() {
  local id="$1" label="$2" before="$3" after="$4" delta ok=0 expected
  delta=$((after - before))
  if [[ "$MODE" == "on" ]]; then
    expected="0 canary hits"
    if ((delta == 0)); then ok=1; fi
  else
    expected=">= 1 canary hit"
    if ((delta >= 1)); then ok=1; fi
  fi
  record_bool "$id" "$label" "$expected" "$delta canary hit(s)" "$ok"
}

# ---------------------------------------------------------------------------
# Login. The access token lives in TOKEN only.
login_body="$(jq -n --arg u "$SSRF_ADMIN_USER" --arg p "$SSRF_ADMIN_PASSWORD" '{username: $u, password: $p}')"
api POST /auth/login "$login_body"
unset login_body
[[ "$HTTP_CODE" == "200" ]] || die "login failed with HTTP $HTTP_CODE"
TOKEN="$(jq -r '.accessToken' <<<"$BODY")"
[[ -n "$TOKEN" && "$TOKEN" != "null" ]] || die "login returned no access token"

# Case h runs in the background of the other cases, so its results accumulate.
say "case h: starting the rebind monitor early"
create_monitor "ssrf-h-rebind" http '{"url":"http://rebind.ssrf.test/hit"}'
REBIND_ID="$LAST_ID"
[[ -n "$REBIND_ID" ]] || die "rebind monitor was not saved, HTTP $HTTP_CODE"

# a-c: targets refused at save time (c is refused in both modes).
reject_case a http '{"url":"http://169.254.169.254/"}' /config/url "$(pick yes no)" \
  "a: cloud metadata IP literal"
reject_case b http '{"url":"http://10.99.0.10/hit"}' /config/url "$(pick yes no)" \
  "b: private IP literal"
reject_case c http '{"url":"http://0x7f.1/"}' /config/url yes \
  "c: non-canonical IP notation"

# d: name that resolves to an internal address.
before="$(canary_hits)"
result_case d "$(pick "$EXPECTED_SSRF_BLOCKED" 'UP -')" http '{"url":"http://internal.ssrf.test/hit"}' \
  "d: name resolves to internal IP"
after="$(canary_hits)"
canary_case d/canary "d: canary during case d" "$before" "$after"

# e: name that resolves to loopback.
result_case e "$(pick "$EXPECTED_SSRF_BLOCKED" 'not:ssrf_blocked')" http \
  '{"url":"http://loop.ssrf.test:8080/"}' "e: name resolves to 127.0.0.1"

# f: redirects from the public server to internal targets.
before="$(canary_hits)"
result_case f1 "$(pick "$EXPECTED_SSRF_BLOCKED" 'UP -')" http '{"url":"http://public.ssrf.test/to-internal"}' \
  "f1: redirect to 10.99.0.10 (IP)"
after="$(canary_hits)"
canary_case f1/canary "f1: canary during case f1" "$before" "$after"

before="$(canary_hits)"
result_case f2 "$(pick "$EXPECTED_SSRF_BLOCKED" 'UP -')" http '{"url":"http://public.ssrf.test/to-internal-name"}' \
  "f2: redirect to internal name"
after="$(canary_hits)"
canary_case f2/canary "f2: canary during case f2" "$before" "$after"

result_case f3 "$(pick "$EXPECTED_SSRF_BLOCKED" 'not:ssrf_blocked')" http \
  '{"url":"http://public.ssrf.test/to-metadata"}' "f3: redirect to cloud metadata"
result_case f4 "$(pick "$EXPECTED_SSRF_BLOCKED" 'not:ssrf_blocked')" http \
  '{"url":"http://public.ssrf.test/to-loop"}' "f4: redirect to 127.0.0.1:8080"

# g: positive control, the "public" network works.
result_case g "UP -" http '{"url":"http://public.ssrf.test/ok"}' "g: public target (control)"

# i: DNS check on an internal name, system resolver.
say "case i: DNS check on an internal name"
create_monitor "ssrf-i-dns" dns '{"hostname":"internal.ssrf.test"}'
if [[ -n "$LAST_ID" ]]; then
  i_id="$LAST_ID"
  if hb="$(latest_heartbeat "$i_id" "$RESULT_TIMEOUT_S")"; then
    record i "i: DNS internal name, scheduled result" "$(pick "$EXPECTED_SSRF_BLOCKED" 'UP -')" "$(summary_of "$hb")"
    i_hb_msg="$(jq -r '.message // ""' <<<"$hb")"
  else
    record i "i: DNS internal name, scheduled result" "$(pick "$EXPECTED_SSRF_BLOCKED" 'UP -')" "no result within ${RESULT_TIMEOUT_S}s"
    i_hb_msg=""
  fi
  api POST "/monitors/$i_id/test"
  i_test_body="$BODY"
  i_test_code="$HTTP_CODE"
  if [[ "$MODE" == "on" ]]; then
    leak=0
    if grep -q '10\.99' <<<"$i_test_body$i_hb_msg"; then leak=1; fi
    record_bool i/leak "i: no internal address in message or details" "no 10.99 in response" \
      "$([[ $leak == 1 ]] && echo 'internal address leaked' || echo 'no internal address')" \
      "$([[ $leak == 0 ]] && echo 1 || echo 0)"
    resolver="$(jq -r '.details.resolver // "-"' <<<"$i_test_body" 2>/dev/null || echo '-')"
    record i/resolver "i: resolver detail" "system" "$resolver"
  fi
  delete_monitor "$i_id"
else
  record i "i: DNS internal name, scheduled result" "$(pick "$EXPECTED_SSRF_BLOCKED" 'UP -')" "monitor not saved, HTTP $HTTP_CODE"
fi

# j: DNS check on a public name.
say "case j: DNS check on a public name"
create_monitor "ssrf-j-dns" dns '{"hostname":"public.ssrf.test"}'
if [[ -n "$LAST_ID" ]]; then
  if hb="$(latest_heartbeat "$LAST_ID" "$RESULT_TIMEOUT_S")"; then
    record j "j: DNS public name" "UP -" "$(summary_of "$hb")"
  else
    record j "j: DNS public name" "UP -" "no result within ${RESULT_TIMEOUT_S}s"
  fi
  delete_monitor "$LAST_ID"
else
  record j "j: DNS public name" "UP -" "monitor not saved, HTTP $HTTP_CODE"
fi

# k: TCP check on an internal name.
result_case k "$(pick "$EXPECTED_SSRF_BLOCKED" 'UP -')" tcp '{"host":"internal.ssrf.test","port":80}' \
  "k: TCP to internal name, port 80"

# l: webhook channels.
say "case l: webhook channels"
ch_payload="$(jq -n '{name: "ssrf-l-internal-ip", kind: "webhook", webhook: {url: "http://10.99.0.10/hook"}}')"
api POST /notifications "$ch_payload"
ch_fields="$(problem_fields)"
l1_ok=0
if [[ "$(pick yes no)" == "yes" ]]; then
  ch_detail="$(jq -r '.detail // ""' <<<"$BODY" 2>/dev/null || true)"
  if [[ "$HTTP_CODE" == "422" && ( "$ch_fields" == *url* || "$ch_detail" == *"SSRF protection"* ) ]]; then l1_ok=1; fi
  record_bool l1 "l1: webhook to internal IP is refused" "HTTP 422, SSRF protection" "HTTP $HTTP_CODE, $ch_detail" "$l1_ok"
else
  if [[ "$HTTP_CODE" == "201" ]]; then l1_ok=1; fi
  record_bool l1 "l1: webhook to internal IP is saved" "HTTP 201" "HTTP $HTTP_CODE" "$l1_ok"
  if [[ "$HTTP_CODE" == "201" ]]; then CH_IDS="$CH_IDS $(jq -r '.id' <<<"$BODY")"; fi
fi

ch_payload="$(jq -n '{name: "ssrf-l-internal-name", kind: "webhook", webhook: {url: "http://internal.ssrf.test/hook"}}')"
api POST /notifications "$ch_payload"
if [[ "$HTTP_CODE" == "201" ]]; then
  ch_id="$(jq -r '.id' <<<"$BODY")"
  CH_IDS="$CH_IDS $ch_id"
  record_bool l2 "l2: webhook to internal name is saved" "HTTP 201" "HTTP $HTTP_CODE" 1
  before="$(canary_hits)"
  api POST "/notifications/$ch_id/test"
  delivered="$(jq -r '.delivered' <<<"$BODY" 2>/dev/null || echo '?')"
  after="$(canary_hits)"
  record l2/test "l2: test send to internal name, delivered" "$(pick false true)" "$delivered"
  canary_case l2/canary "l2: canary during test send" "$before" "$after"
  api DELETE "/notifications/$ch_id"
else
  record_bool l2 "l2: webhook to internal name is saved" "HTTP 201" "HTTP $HTTP_CODE" 0
fi

# h: DNS rebinding. The results have been collecting since the start.
say "case h: waiting for $REBIND_CHECKS scheduled results of the rebind monitor"
deadline=$(($(date +%s) + REBIND_TIMEOUT_S))
while :; do
  api GET "/monitors/$REBIND_ID/heartbeats?limit=1000"
  count="$(jq '.items | length' <<<"$BODY" 2>/dev/null || echo 0)"
  if ((count >= REBIND_CHECKS)) || (($(date +%s) >= deadline)); then
    break
  fi
  sleep 5
done
n_up="$(jq '[.items[] | select(.status == "UP")] | length' <<<"$BODY" 2>/dev/null || echo 0)"
n_blocked="$(jq '[.items[] | select(.errorClass == "ssrf_blocked")] | length' <<<"$BODY" 2>/dev/null || echo 0)"
h_summary="$count results: $n_up UP, $n_blocked ssrf_blocked"
h_ok=0
if [[ "$MODE" == "on" ]]; then
  if ((count >= REBIND_CHECKS && n_up > 0 && n_blocked > 0)); then h_ok=1; fi
  record_bool h "h: DNS rebinding, both UP and ssrf_blocked" "UP and DOWN ssrf_blocked" "$h_summary" "$h_ok"
else
  if ((count >= REBIND_CHECKS && n_up > 0 && n_blocked == 0)); then h_ok=1; fi
  record_bool h "h: DNS rebinding, reachable both ways" "UP only, no ssrf_blocked" "$h_summary" "$h_ok"
fi
delete_monitor "$REBIND_ID"

# m: the canary in total.
total_hits="$(canary_hits)"
if [[ "$MODE" == "on" ]]; then
  m_ok=0
  if ((total_hits == 0)); then m_ok=1; fi
  record_bool m "m: canary log at the end" "0 lines" "$total_hits lines" "$m_ok"
else
  m_ok=0
  if ((total_hits >= 1)); then m_ok=1; fi
  record_bool m "m: canary log at the end" ">= 1 line" "$total_hits lines" "$m_ok"
fi

# ---------------------------------------------------------------------------
# Table.
echo
printf '%-7s  %-52s  %-30s  %-42s  %s\n' CASE CHECK EXPECTED ACTUAL RESULT
printf '%-7s  %-52s  %-30s  %-42s  %s\n' ------- ---------------------------------------------------- ------------------------------ ------------------------------------------ ------
while IFS=$'\t' read -r c k e a v; do
  printf '%-7s  %-52s  %-30s  %-42s  %s\n' "$c" "$k" "$e" "$a" "$v"
done <"$RESULTS_FILE"
total="$(wc -l <"$RESULTS_FILE" | tr -d ' ')"
echo
echo "mode=$MODE: $total checks, $FAILS failed"

if ((FAILS > 0)); then
  exit 1
fi
