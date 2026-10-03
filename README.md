# loadbalancer

[![CI](https://github.com/francisco3ferraz/loadbalancer/actions/workflows/ci.yml/badge.svg)](https://github.com/francisco3ferraz/loadbalancer/actions/workflows/ci.yml)

An HTTP (layer 7) load balancer written in Go, using only the standard library
plus a YAML parser. Built as a learning project: each feature exists to handle
a real failure mode, and each one is covered by tests.

## Features

- **Four algorithms:** round robin, least connections, weighted round robin
  (nginx's smooth variant), and weighted least connections.
- **Health checks:** active checks on a configurable path, run on every
  backend at once so hanging backends don't slow down the rest, plus passive
  detection when real requests fail.
- **Failure handling:** a backend that refuses connections is marked down
  immediately; one that times out is marked down after several failures in a
  row, so one slow URL can't take healthy servers out of rotation.
- **Retries:** failed `GET`, `HEAD` and `OPTIONS` requests without a body are
  retried on another backend. Other requests are never retried, since repeating
  them could have side effects.
- **Timeouts:** per attempt, per request across all retries (giving a `504`),
  and on the server itself against slow clients.
- **Long-lived connections:** WebSockets (and any other `Upgrade`) and
  server-sent events stay open for as long as both ends want, exempt from the
  request deadline and the server's write timeout.
- **Request size limit:** bodies over `max_body_size` get a `413`, whether or
  not the client declares the size up front, and backends aren't blamed.
- **Graceful shutdown:** on Ctrl+C or `SIGTERM`, requests in progress finish
  before the process exits. A second Ctrl+C exits immediately.
- **Stats:** an optional admin server with a JSON `/stats` endpoint.

## Quick start

Requires Go 1.27.1 or later (the version in `go.mod`).

```sh
# Terminal 1: start three fake backends on ports 8080-8082
scripts/run-backends.sh

# Terminal 2: start the load balancer with ./config.yaml
go run ./cmd/loadbalancer

# Terminal 3: send some requests
for i in 1 2 3 4; do curl localhost:8000; done   # :8080 :8081 :8082 :8080
curl 127.0.0.1:9000/stats
```

Use a different config file with `-config path/to/file.yaml`.

## Configuration

Settings come from a YAML file. Only `backends` is required. The shipped
[`config.yaml`](config.yaml) documents every setting and its default.

| Setting | Default | Meaning |
|---|---|---|
| `listen` | `:8000` | Address the load balancer listens on |
| `algorithm` | `round-robin` | See [Algorithms](#algorithms) |
| `backends` | (required) | Backend URLs, or objects with `url` and `weight` |
| `health_path` | `/health` | Path requested by health checks; `200` means healthy |
| `health_check_interval` | `5s` | How often each backend is checked |
| `attempt_timeout` | `5s` | How long one backend has to start answering |
| `request_timeout` | `10s` | Total time for a request, across all retries; not applied to upgrades or event streams |
| `max_failures` | `3` | Failures in a row that mark a backend down |
| `max_body_size` | `1MB` | Largest request body accepted; bytes, or with `KB`, `MB` or `GB` |
| `admin_listen` | off | Address of the admin server, e.g. `127.0.0.1:9000` |

Backends can be plain URLs or carry a weight, and the two forms can be mixed:

```yaml
backends:
  - http://127.0.0.1:8080          # weight 1
  - url: http://127.0.0.1:8081
    weight: 3
```

The config is checked at startup: unknown keys (such as a typo like
`algoritm`), invalid durations, negative values and unknown algorithms are
reported as errors, and the load balancer doesn't start.

## Algorithms

| `algorithm` | Picks | Good for |
|---|---|---|
| `round-robin` | Each backend in turn | Similar backends and similar requests |
| `least-connections` | The backend with the fewest requests in progress | Requests with very different durations |
| `weighted-round-robin` | In proportion to `weight`, interleaved (`A A B A C A A` for weights 5, 1, 1) | Backends of different sizes |
| `weighted-least-connections` | The fewest requests in progress per unit of weight | Different sizes *and* uneven request durations |

Backends that are down, or already tried for the current request, are left out
before an algorithm chooses, so traffic is shared only among healthy backends.

## How failures are handled

For each request, the load balancer tries backends one at a time:

1. **Success:** the response is passed to the client, and the backend's
   failure count resets.
2. **Connection refused:** the backend is marked down immediately.
3. **Timeout or other error:** the backend's failure count goes up. It's marked
   down when the count reaches `max_failures`.
4. **Retry:** for `GET`, `HEAD` and `OPTIONS` requests without a body, the next
   backend is tried. Otherwise the client gets a `502`.

The client's response depends on why the request couldn't be served:

| Status | Meaning |
|---|---|
| `413 Content Too Large` | The request body was over `max_body_size` |
| `502 Bad Gateway` | A backend was tried and failed |
| `503 Service Unavailable` | No backend was up, so none was tried |
| `504 Gateway Timeout` | `request_timeout` ran out |

If the client disconnects, sends a body over `max_body_size`, or
`request_timeout` runs out, the backend isn't blamed: none of these counts as
a backend failure. Backends marked down come back when their health check
succeeds again.

## Admin server

With `admin_listen` set, `GET /stats` returns a snapshot of every backend:

```json
{
  "backends": [
    {"url": "http://127.0.0.1:8080", "alive": true, "active": 2, "failures": 0, "requests": 1532},
    {"url": "http://127.0.0.1:8081", "alive": false, "active": 0, "failures": 3, "requests": 610}
  ]
}
```

`active` is requests in progress, `failures` is failures in a row, and
`requests` is attempts served in total. The endpoint shows internal addresses,
so it runs on a separate port, which should stay on a loopback address.

## Testing

```sh
go test -race ./...
```

The suite runs in a few seconds. CI runs it on every push, along with
`gofmt`, `go vet` and `go mod tidy` checks.

For manual testing, `cmd/fakebackend` is a backend whose failures you control:

```sh
go run ./cmd/fakebackend -port 8081 -delay 30s        # hangs: tests timeouts
go run ./cmd/fakebackend -port 8081 -error-rate 0.2   # 20% of requests return 500
curl -X POST localhost:8081/admin/health/down          # fail health checks, keep serving
curl 'localhost:8000/slow?d=3s'                         # a slow request through the balancer
curl -N localhost:8000/stream                           # server-sent events, one a second
```

`scripts/run-backends.sh` starts several at once (`BACKEND_FLAGS` passes flags
to all of them), and prints each PID so you can kill one to simulate a crash.

## Project layout

```
cmd/loadbalancer     the program: loads config, starts the servers, shuts down
cmd/fakebackend      a controllable backend for testing
internal/balancer    proxying, retries, timeouts, health checks, algorithms, stats
internal/config      the YAML file format
internal/admin       the /stats endpoint
scripts/             run-backends.sh
```

## Design decisions

- **Only safe requests are retried.** Repeating a `POST` could charge someone
  twice. Requests with a body aren't retried either, because the body is used
  up by the first attempt.
- **A timeout isn't proof a backend is dead.** Refused connections are, so they
  mark a backend down at once; timeouts need several in a row.
- **One total deadline per request.** Without it, retries would multiply the
  waiting time: 3 backends × a 5s timeout would be 15s.
- **Long-lived connections are recognised, not configured.** An upgrade is
  known from its request (`Connection: Upgrade` plus an `Upgrade` header), so
  it never gets a deadline. An event stream is only known from its response
  (`Content-Type: text/event-stream`), so the deadline is a timer that's
  stopped when such a response arrives, rather than a fixed `WithTimeout`.
- **Load is counted locally.** Least connections uses the load balancer's own
  count of requests in progress, as nginx, HAProxy and Envoy do. Each instance
  therefore sees only its own traffic.
- **The file format is separate from the internal config,** so either can
  change without breaking the other. Older files listing backends as plain URLs
  keep working.
- **Bad configuration fails at startup,** never while serving traffic.

## Not included

These are real load balancer features, deliberately left out of scope:
TLS termination, HTTP/2 to backends, Prometheus metrics, structured logging,
reloading the config without a restart, sticky sessions (consistent hashing),
latency-based algorithms, and an idle timeout for long-lived connections: a
WebSocket or stream whose backend goes silent stays open until one end closes
it.
