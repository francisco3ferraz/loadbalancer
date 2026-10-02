#!/usr/bin/env bash
# Starts fake backends for the load balancer. Ctrl+C stops all of them.
#
#   scripts/run-backends.sh              # ports 8080 8081 8082
#   scripts/run-backends.sh 9000 9001    # custom ports
#   BACKEND_FLAGS="-delay 100ms" scripts/run-backends.sh
#
# To simulate one backend crashing, kill it by its PID (printed at startup).
# To bring it back: bin/fakebackend -port <port>
set -euo pipefail
cd "$(dirname "$0")/.."

ports=("$@")
if [ ${#ports[@]} -eq 0 ]; then
	ports=(8080 8081 8082)
fi

go build -o bin/fakebackend ./cmd/fakebackend

pids=()
cleanup() {
	kill "${pids[@]}" 2>/dev/null || true
	wait
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for port in "${ports[@]}"; do
	# shellcheck disable=SC2086 # BACKEND_FLAGS is meant to split into words
	bin/fakebackend -port "$port" ${BACKEND_FLAGS:-} &
	pids+=($!)
	echo "backend :$port pid $!"
done

wait
