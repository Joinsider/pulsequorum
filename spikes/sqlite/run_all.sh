#!/bin/sh
# Reproduziert die Messmatrix aus README.md. Laeuft seriell, damit sich die
# Messungen nicht gegenseitig beeinflussen. Gesamtdauer ca. 25 Minuten.
set -eu
cd "$(dirname "$0")"
export CGO_ENABLED=0 GOWORK=off
mkdir -p results

run() { # driver variant rows think_ms conc_seconds
	local driver="$1" variant="$2" rows="$3" think_ms="$4" conc_seconds="$5"
	local log="results/$driver-$variant-$rows-think$think_ms.log"
	go run . -driver="$driver" -variant="$variant" -rows="$rows" -think-ms="$think_ms" -conc-seconds="$conc_seconds" >"$log" 2>&1
	echo "done $log"
}

# 1) 2 Stunden (~1,08 Mio. Zeilen), Leser ohne Pause
for d in modernc ncruces; do for v in rowid without-rowid; do run $d $v 1080000 0 20; done; done
# 2) 12 Stunden (~6,48 Mio. Zeilen), Leser ohne Pause
for d in modernc ncruces; do for v in rowid without-rowid; do run $d $v 6480000 0 20; done; done
# 3) 2 Stunden, Leser mit 100 ms Pause zwischen Queries (nur rowid, Sensitivitaet)
for d in modernc ncruces; do run $d rowid 1080000 100 20; done

go run . -summarize
