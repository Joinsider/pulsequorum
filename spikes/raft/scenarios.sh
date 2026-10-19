#!/usr/bin/env bash
# AP-0.14 raft spike: builds the image, starts 3 voters with podman/docker
# compose and runs all scenarios. Output goes to results/run-<ts>.txt.
#
#   ./scenarios.sh                 # defaults (RAFT_TIMEOUT_MS=1000)
#   RAFT_TIMEOUT_MS=500 ./scenarios.sh
#   KEEP=1 ./scenarios.sh          # leave the cluster running afterwards
set -uo pipefail
cd "$(dirname "$0")"

COMPOSE=${COMPOSE:-podman-compose}
CTR=${CTR:-podman}
export RAFT_TIMEOUT_MS=${RAFT_TIMEOUT_MS:-1000}
export RAFT_TRAILING_LOGS=${RAFT_TRAILING_LOGS:-5} # tiny on purpose: rejoining nodes must use InstallSnapshot
export JOIN_HOLD_MS=${JOIN_HOLD_MS:-1500}           # forces the two joins to overlap
KILL_ROUNDS=${KILL_ROUNDS:-5}
NET=raftspike-cluster

mkdir -p results/logs
OUT=results/run-$(date +%Y%m%d-%H%M%S)-t${RAFT_TIMEOUT_MS}.txt
exec > >(tee "$OUT") 2>&1
TMP=results/logs/.resp

# ---------- helpers ----------
now() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
port() { echo $((18000 + ${1#n})); }
st() { curl -s -m 2 "localhost:$(port "$1")/status"; }
field() { st "$1" | jq -r "$2" 2>/dev/null; }
leader_of() { local a; a=$(field "$1" .leader_addr); a=${a#raft-}; echo "${a%%:*}"; }
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
hdr() { printf '\n==================== %s ====================\n' "$*"; }

wait_until() { # timeout_s cmd...  (polls every 50 ms)
	local deadline=$(($(now) + $1 * 1000))
	shift
	while (($(now) < deadline)); do
		"$@" && return 0
		sleep 0.05
	done
	return 1
}

# put node key value [request-id]  -> sets CODE and BODY
put() {
	local rid=${4:-}
	CODE=$(curl -s -m 8 -o "$TMP" -w '%{http_code}' -X PUT ${rid:+-H "X-Request-ID: $rid"} \
		"localhost:$(port "$1")/kv/$2?value=$3")
	BODY=$(cat "$TMP" 2>/dev/null)
}
put_ok() { put "$1" "$2" "$3" && [[ $CODE == 200 ]]; }

all_agree_leader() { # nodes... -> all report the same non-empty leader
	local first="" l
	for n in "$@"; do
		l=$(leader_of "$n")
		[[ -z $l ]] && return 1
		[[ -z $first ]] && first=$l
		[[ $l == "$first" ]] || return 1
	done
	LEADER=$first
}
new_leader_among() { # old nodes...
	local old=$1
	shift
	all_agree_leader "$@" && [[ $LEADER != "$old" ]]
}
current_leader() { for n in n1 n2 n3; do l=$(leader_of $n) && [[ -n $l ]] && echo "$l" && return; done; }
voters() { field "$1" '[.servers[] | select(.suffrage=="Voter")] | length'; }
is_follower_caught_up() { # node leader
	[[ $(field "$1" .state) == Follower ]] && [[ $(field "$1" .fsm.fingerprint) == $(field "$2" .fsm.fingerprint) ]]
}
fingerprints() { for n in "$@"; do printf '%s=%s(rows=%s,applied=%s) ' "$n" "$(field $n .fsm.fingerprint)" "$(field $n .fsm.rows)" "$(field $n .fsm.applied_index)"; done; echo; }
same_fp() { local a; a=$(field "$1" .fsm.fingerprint); shift; for n in "$@"; do [[ $(field $n .fsm.fingerprint) == "$a" ]] || return 1; done; }
terms() { for n in "$@"; do printf '%s:term=%s/%s ' "$n" "$(field $n .term)" "$(field $n .state)"; done; echo; }

# hb_table nodes... : per stream (node_id/incarnation) contig and max as seen by each node
hb_table() {
	local f
	for n in "$@"; do curl -s -m 2 "localhost:$(port $n)/hb/summary" >"results/logs/.hb_$n"; done
	jq -rn --args '
	  [ $ARGS.positional[] as $n | (input | to_entries[] | {node:$n, k:.key, c:.value.contig, m:.value.max}) ]
	  | group_by(.k)[] | "  stream \(.[0].k[0:8])…/\(.[0].k | split("/")[1]): " + ([.[] | "\(.node) contig=\(.c) max=\(.m)"] | join("  "))
	' "$@" < <(for n in "$@"; do cat "results/logs/.hb_$n"; done)
}
# hb_contig node stream-prefix -> contig of that stream on node
hb_contig() { curl -s -m 2 "localhost:$(port $1)/hb/summary" | jq -r --arg k "$2" '[to_entries[] | select(.key | startswith($k)) | .value.contig] | max // 0'; }
hb_ge() { [[ $(hb_contig "$1" "$2") -ge $3 ]]; }

recreate() { # node [ENV=VAL...]  (keeps the volume)
	local n=$1
	shift
	$CTR rm -f "spike-$n" >/dev/null 2>&1
	env "$@" $COMPOSE up -d --no-deps "$n" >/dev/null 2>&1
}

# ---------- build & setup ----------
hdr "S0 setup (RAFT_TIMEOUT_MS=$RAFT_TIMEOUT_MS, TRAILING_LOGS=$RAFT_TRAILING_LOGS)"
mkdir -p build
GOOS=linux GOARCH=$(go env GOARCH) CGO_ENABLED=0 go build -trimpath -o build/spike . || exit 1
[[ -f certs/ca.pem ]] || go run . pki certs n1,n2,n3
$CTR build -q -t localhost/uc-raft-spike:dev . >/dev/null || exit 1
$COMPOSE down -v >/dev/null 2>&1
$CTR rm -f spike-n1 spike-n2 spike-n3 >/dev/null 2>&1
$CTR volume rm raftspike-n1 raftspike-n2 raftspike-n3 >/dev/null 2>&1

N1_JOIN= $COMPOSE up -d n1 >/dev/null 2>&1
wait_until 20 all_agree_leader n1 || { log "n1 did not become leader"; exit 1; }
log "n1 boot: $(field n1 '.boot.decision') state=$(field n1 .state)"
put n1 hello world
log "write on n1: $CODE"
curl -s -X POST "localhost:18001/admin/bulk?n=10000&prefix=mon" | jq -c '{status: (.error // "ok"), ms}'
log "bulk 10k rows applied: rows=$(field n1 .fsm.rows)"
log "user snapshot n1: $(curl -s -X POST localhost:18001/admin/snapshot | jq -c .)"
$CTR logs spike-n1 2>&1 | grep "snapshot persisted" | tail -1 | sed 's/^/    /'

hdr "S0b concurrent join of n2 + n3 (nonvoter -> catch-up -> voter, serialised)"
t0=$(now)
$COMPOSE up -d n2 n3 >/dev/null 2>&1
wait_until 60 bash -c "[[ \$(curl -s localhost:18001/status | jq '[.servers[] | select(.suffrage==\"Voter\")] | length') == 3 ]]" &&
	log "3 voters after $(($(now) - t0)) ms (incl. container start)"
$CTR logs spike-n1 2>&1 | grep -E 'join: ' | sed -E 's/^time=[^ ]+ //; s/^/    /'
for n in n2 n3; do $CTR logs spike-$n 2>&1 | grep -E 'snapshot restored|join attempt' | sed -E "s/^time=[^ ]+ /    $n: /"; done
wait_until 20 same_fp n1 n2 n3 && log "FSM identical on all nodes: $(fingerprints n1 n2 n3)"

# ---------- S1 ----------
hdr "S1 bootstrap detection from persisted state (changed env)"
recreate n1 N1_JOIN=raft-n2:9443
wait_until 20 all_agree_leader n1 n2 n3
log "n1 recreated with CLUSTER_JOIN=raft-n2:9443 -> $(field n1 .boot.decision); has_existing_state=$(field n1 .boot.has_existing_state)"
recreate n2 N2_JOIN=
wait_until 20 all_agree_leader n1 n2 n3
log "n2 recreated WITHOUT CLUSTER_JOIN    -> $(field n2 .boot.decision); has_existing_state=$(field n2 .boot.has_existing_state)"
log "voters seen by n1/n2/n3: $(voters n1)/$(voters n2)/$(voters n3); leader=$LEADER (single cluster)"
log "$(terms n1 n2 n3)"
recreate n1
recreate n2
wait_until 20 all_agree_leader n1 n2 n3

# ---------- S2 ----------
hdr "S2 write forwarding + request-id dedup"
L=$(current_leader)
FOL=($(for n in n1 n2 n3; do [[ $n != "$L" ]] && echo $n; done))
put "${FOL[0]}" fwd 1 rid-fwd-1
log "PUT on follower ${FOL[0]}: $CODE $(echo "$BODY" | jq -c '{forwarded, applied_by, ms, v: .result.version_id, deduped: .result.deduped}')"
put "${FOL[1]}" fwd 2 rid-fwd-1
log "same request id on ${FOL[1]}: $CODE $(echo "$BODY" | jq -c '{forwarded, applied_by, ms, v: .result.version_id, deduped: .result.deduped}')"
sleep 0.3
for n in n1 n2 n3; do log "  GET fwd on $n: $(curl -s localhost:$(port $n)/kv/fwd | jq -c .)"; done
# latency of forwarded vs local writes
lat_local=0 lat_fwd=0
for i in $(seq 1 20); do
	a=$(now); put "$L" lat$i x; lat_local=$((lat_local + $(now) - a))
	a=$(now); put "${FOL[0]}" latf$i x; lat_fwd=$((lat_fwd + $(now) - a))
done
log "avg write latency (curl, 20x): leader $((lat_local / 20)) ms, via follower $((lat_fwd / 20)) ms"

# ---------- S3 ----------
hdr "S3 kill -9 leader ($KILL_ROUNDS rounds)"
printf '  %-5s %-6s %-8s %-12s %-14s %-10s %s\n' round killed new_lead elect_ms first_write_ms dedup_ok rejoin_ms
for i in $(seq 1 "$KILL_ROUNDS"); do
	L=$(current_leader)
	S=($(for n in n1 n2 n3; do [[ $n != "$L" ]] && echo $n; done))
	put "$L" before$i v rid-kill-$i
	v1=$(echo "$BODY" | jq -r .result.version_id)
	t0=$(now)
	$CTR kill "spike-$L" >/dev/null
	wait_until 30 new_leader_among "$L" "${S[@]}"
	te=$(($(now) - t0))
	NL=$LEADER
	wait_until 30 put_ok "${S[0]}" after$i v
	tw=$(($(now) - t0))
	put "${S[1]}" before$i v rid-kill-$i # client retry with the same request id after leader change
	v2=$(echo "$BODY" | jq -r .result.version_id)
	dd=$([[ $(echo "$BODY" | jq -r .result.deduped) == true && $v1 == "$v2" ]] && echo yes || echo NO)
	t1=$(now)
	$CTR start "spike-$L" >/dev/null
	wait_until 60 is_follower_caught_up "$L" "$NL"
	tr=$(($(now) - t1))
	printf '  %-5s %-6s %-8s %-12s %-14s %-10s %s\n' "$i" "$L" "$NL" "$te" "$tw" "$dd" "$tr"
	sleep 2
done

hdr "S3b graceful stop of leader (SIGTERM -> leadership transfer)"
L=$(current_leader)
S=($(for n in n1 n2 n3; do [[ $n != "$L" ]] && echo $n; done))
t0=$(now)
$CTR stop -t 10 "spike-$L" >/dev/null &
wait_until 30 new_leader_among "$L" "${S[@]}"
log "new leader $LEADER after $(($(now) - t0)) ms"
wait
$CTR logs spike-$L 2>&1 | grep -E 'leadership transfer|shutdown complete' | tail -2 | sed -E 's/^time=[^ ]+ /    /'
$CTR start "spike-$L" >/dev/null
wait_until 60 is_follower_caught_up "$L" "$LEADER"

# ---------- S4 ----------
hdr "S4 partition 2|1 with the LEADER in the minority"
L=$(current_leader)
M=($(for n in n1 n2 n3; do [[ $n != "$L" ]] && echo $n; done))
log "before: $(terms n1 n2 n3)"
LID=$(field "$L" .node_id)
t0=$(now)
$CTR network disconnect $NET "spike-$L" >/dev/null
wait_until 30 new_leader_among "$L" "${M[@]}"
log "majority ${M[*]} elected $LEADER after $(($(now) - t0)) ms"
NL=$LEADER
wait_until 30 bash -c "[[ \$(curl -s localhost:$(port $L)/status | jq -r .state) != Leader ]]"
log "old leader $L stepped down after $(($(now) - t0)) ms (state=$(field $L .state))"
put "${M[0]}" part-major ok
log "write on majority ${M[0]}: $CODE"
a=$(now); put "$L" part-minor x
log "write on minority $L: $CODE after $(($(now) - a)) ms: $(echo "$BODY" | jq -c '{error, result_unknown}')"
log "read on minority $L (stale ok): $(curl -s localhost:$(port $L)/kv/hello | jq -c .)"
sleep 15
log "after 15 s partition: $(terms n1 n2 n3)"
seqL=$(hb_contig "$L" "$LID")
seqM=$(hb_contig "${M[0]}" "$(field ${M[0]} .node_id)")
log "hb: $L own contig=$seqL, seen by ${M[0]}=$(hb_contig ${M[0]} $LID)"
th=$(now)
$CTR network connect --alias "raft-$L" $NET "spike-$L" >/dev/null
wait_until 60 bash -c "[[ \$(curl -s localhost:$(port $L)/status | jq -r .leader_addr) == raft-$NL:9443 ]]"
log "after heal: $L follows $NL after $(($(now) - th)) ms; $(terms n1 n2 n3)"
wait_until 60 same_fp n1 n2 n3 && log "FSM converged after $(($(now) - th)) ms: $(fingerprints n1 n2 n3)"
wait_until 60 hb_ge "${M[0]}" "$LID" "$seqL" && wait_until 60 hb_ge "${M[1]}" "$LID" "$seqL" &&
	wait_until 60 hb_ge "$L" "$(field ${M[0]} .node_id)" "$seqM" &&
	log "hb gap closed in both directions after $(($(now) - th)) ms"
hb_table n1 n2 n3
log "replication stats on ${M[0]}: $(field ${M[0]} '.hb_peers | map_values({pushed, dropped_buffer_full, push_errors, ae_fetched_new_rows})' | jq -c .)"

hdr "S4b partition 2|1 with a FOLLOWER in the minority"
L=$(current_leader)
F=$(for n in n1 n2 n3; do [[ $n != "$L" ]] && echo $n; done | head -1)
$CTR network disconnect $NET "spike-$F" >/dev/null
t0=$(now)
for i in $(seq 1 20); do put "$L" iso$i v; done
log "20 writes on leader during isolation: last=$CODE; leader unchanged: $(leader_of $L)"
put "$F" iso-minor x
log "write on isolated follower $F: $CODE $(echo "$BODY" | jq -c '{error, result_unknown}')"
sleep 5
log "$(terms n1 n2 n3)"
th=$(now)
$CTR network connect --alias "raft-$F" $NET "spike-$F" >/dev/null
wait_until 60 same_fp n1 n2 n3 && log "follower $F caught up $(($(now) - th)) ms after heal; $(terms n1 n2 n3)"

# ---------- S5 ----------
hdr "S5 wipe data dir of one node (new node id + incarnation)"
L=$(current_leader)
W=n3
[[ $L == n3 ]] && W=n2
O=($(for n in n1 n2 n3; do [[ $n != "$W" ]] && echo $n; done))
OLD_ID=$(field $W .node_id)
OLD_INC=$(field $W .incarnation)
$CTR rm -f "spike-$W" >/dev/null
$CTR volume rm "raftspike-$W" >/dev/null
OLD_MAX=$(hb_contig "${O[0]}" "$OLD_ID")
log "wiped $W (old id ${OLD_ID:0:8}, inc $OLD_INC); its stream on ${O[0]}: contig=$OLD_MAX, on ${O[1]}: $(hb_contig ${O[1]} $OLD_ID)"
for i in $(seq 1 10); do put "$L" wipe$i v; done
log "10 writes while $W is gone: $CODE; leader snapshot: $(curl -s -X POST localhost:$(port $L)/admin/snapshot | jq -c .)"
recreate "$W"
sleep 6
log "join attempts of the wiped node:"
$CTR logs "spike-$W" 2>&1 | grep -E 'boot decision|join attempt' | head -3 | sed -E 's/^time=[^ ]+ /    /' | cut -c1-260
log "cluster still writable with 2/3 voters: $(put_ok "$L" during-wipe v && echo yes || echo NO)"
t0=$(now)
log "operator removes old id: $(curl -s -X POST "localhost:$(port $L)/admin/remove?id=$OLD_ID" | jq -c .)"
wait_until 60 bash -c "[[ \$(curl -s localhost:$(port $L)/status | jq '[.servers[] | select(.suffrage==\"Voter\")] | length') == 3 ]]"
log "$W rejoined as voter $(($(now) - t0)) ms after removal; new id ${OLD_ID:0:0}$(field $W .node_id | cut -c1-8), new inc $(field $W .incarnation)"
$CTR logs "spike-$L" 2>&1 | grep -E "join: (nonvoter|promoted)" | tail -2 | sed -E 's/^time=[^ ]+ /    /'
$CTR logs "spike-$W" 2>&1 | grep -E 'snapshot restored' | sed -E 's/^time=[^ ]+ /    /'
wait_until 30 same_fp n1 n2 n3 && log "FSM identical: $(fingerprints n1 n2 n3)"
wait_until 30 hb_ge "$W" "$OLD_ID" "$OLD_MAX" && log "old incarnation stream restored onto new $W via anti-entropy: contig=$(hb_contig $W $OLD_ID) (expected $OLD_MAX)"
log "old stream still complete on ${O[0]}/${O[1]}: $(hb_contig ${O[0]} $OLD_ID)/$(hb_contig ${O[1]} $OLD_ID)"
hb_table n1 n2 n3

# ---------- S6 ----------
hdr "S6 restart all"
for mode in kill stop; do
	wait_until 30 same_fp n1 n2 n3
	FP=$(field n1 .fsm.fingerprint)
	if [[ $mode == kill ]]; then $CTR kill spike-n1 spike-n2 spike-n3 >/dev/null; else $CTR stop -t 10 spike-n1 spike-n2 spike-n3 >/dev/null; fi
	t0=$(now)
	$CTR start spike-n1 spike-n2 spike-n3 >/dev/null
	wait_until 60 all_agree_leader n1 n2 n3
	tl=$(($(now) - t0))
	wait_until 60 put_ok n1 restart-$mode v
	log "[$mode all] leader $LEADER after $tl ms, first write after $(($(now) - t0)) ms (incl. container start)"
	for n in n1 n2 n3; do log "  $n boot: $(field $n '.boot | {decision, fsm_applied_index_at_start, latest_snapshot_index, no_snapshot_restore_on_start}' | jq -c .) skipped_on_replay=$(field $n .fsm.skipped_on_replay)"; done
	wait_until 30 same_fp n1 n2 n3
	log "  fingerprint before=$FP after: $(fingerprints n1 n2 n3)"
done
hb_table n1 n2 n3

hdr "logs"
for n in n1 n2 n3; do $CTR logs spike-$n >"results/logs/$n.log" 2>&1; done
log "container logs in results/logs/, summary in $OUT"
[[ ${KEEP:-0} == 1 ]] || $COMPOSE down -v >/dev/null 2>&1
