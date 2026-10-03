#!/usr/bin/env bash
# Benchmarks the load balancer with wrk: one backend hit directly as a
# baseline, then the load balancer in front of three backends with each
# algorithm. Prints a Markdown table with the median run of each.
#
#   scripts/bench.sh
#   RUNS=5 DURATION=60s CONNECTIONS=100 scripts/bench.sh
#
# Everything runs on this machine, so the numbers compare setups on the same
# hardware rather than measure the load balancer alone.
set -euo pipefail
cd "$(dirname "$0")/.."

RUNS=${RUNS:-3}
DURATION=${DURATION:-30s}
CONNECTIONS=${CONNECTIONS:-50}
THREADS=${THREADS:-4}
WARMUP=${WARMUP:-5s}

backend_ports=(18091 18092 18093)
lb_port=18100

if ! command -v wrk >/dev/null; then
	echo "wrk not found: install it first (on Arch, paru -S wrk)" >&2
	exit 1
fi

go build -o bin/fakebackend ./cmd/fakebackend
go build -o bin/loadbalancer ./cmd/loadbalancer

tmp=$(mktemp -d)
pids=()
cleanup() {
	kill "${pids[@]}" 2>/dev/null || true
	wait 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

wait_for_port() {
	for _ in $(seq 50); do
		if (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; then
			return
		fi
		sleep 0.1
	done
	echo "nothing listening on port $1" >&2
	exit 1
}

for port in "${backend_ports[@]}"; do
	bin/fakebackend -port "$port" -quiet 2>/dev/null &
	pids+=($!)
	wait_for_port "$port"
done

# bench prints one table row for url: the run with the median requests/sec,
# with that run's latency percentiles.
bench() {
	local name=$1 url=$2
	wrk -t"$THREADS" -c"$CONNECTIONS" -d"$WARMUP" "$url" >/dev/null
	for _ in $(seq "$RUNS"); do
		wrk -t"$THREADS" -c"$CONNECTIONS" -d"$DURATION" --latency "$url" | awk '
			$1 == "50%" { p50 = $2 }
			$1 == "99%" { p99 = $2 }
			$1 == "Requests/sec:" { rps = $2 }
			/Non-2xx|Socket errors/ { errors = errors " " $0 }
			END { printf "%.0f %s %s%s\n", rps, p50, p99, errors }'
	done | sort -n | sed -n "$(((RUNS + 1) / 2))p" | {
		read -r rps p50 p99 errors
		printf "| %s | %s | %s | %s |%s\n" "$name" "$rps" "$p50" "$p99" "${errors:+ $errors}"
	}
}

echo "wrk -t$THREADS -c$CONNECTIONS -d$DURATION, median of $RUNS runs"
echo
echo "| Setup | Requests/sec | p50 | p99 |"
echo "|---|---|---|---|"

bench "Direct to one backend" "http://127.0.0.1:${backend_ports[0]}/"

for algorithm in round-robin least-connections weighted-round-robin weighted-least-connections; do
	cat >"$tmp/config.yaml" <<EOF
listen: "127.0.0.1:$lb_port"
algorithm: $algorithm
access_log: false
backends:
$(printf '  - http://127.0.0.1:%s\n' "${backend_ports[@]}")
EOF
	bin/loadbalancer -config "$tmp/config.yaml" 2>"$tmp/lb.log" &
	lb=$!
	wait_for_port "$lb_port"

	bench "Load balancer, $algorithm" "http://127.0.0.1:$lb_port/"

	kill "$lb"
	wait "$lb" 2>/dev/null || true
done
