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
  them could have side effects. Optionally, so are answers of `502` or `503`.
- **Timeouts:** per attempt, per request across all retries (giving a `504`),
  and on the server itself against slow clients.
- **Long-lived connections:** WebSockets (and any other `Upgrade`) and
  server-sent events stay open for as long as both ends want, exempt from the
  request deadline and the server's write timeout.
- **Request size limit:** bodies over `max_body_size` get a `413`, whether or
  not the client declares the size up front, and backends aren't blamed.
- **Graceful shutdown:** on Ctrl+C or `SIGTERM`, requests in progress finish
  before the process exits. A second Ctrl+C exits immediately. Event streams
  never finish on their own, so they hold the shutdown for its full limit
  (`request_timeout` + 5s) and are then cut; WebSockets are cut when the
  process exits.
- **Config reload:** `kill -HUP <pid>` applies an edited config without
  closing the port or failing a request; an invalid config is rejected and
  the old one keeps running.
- **Forwarding headers:** backends get `X-Forwarded-For`, `X-Forwarded-Host`
  and `X-Forwarded-Proto`, set by the load balancer and never copied from the
  client, so they can't be forged. The client's `Host` is kept.
- **Access log:** one structured line per request on stdout, with the backend
  that served it and how many were tried; errors stay on stderr.
- **Stats:** an optional admin server with a JSON `/stats` endpoint and
  `pprof` profiles.

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
| `access_log` | `true` | Log one line per request to stdout |
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
`algoritm`), invalid durations or sizes, negative values and unknown algorithms
are reported as errors, and the load balancer doesn't start.

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
5. **`502` or `503` answer:** with `retry_unavailable: true`, these requests
   are also retried on the next backend, and the backend isn't counted as
   failing; health checks decide whether it's down. The last backend's answer
   is passed to the client unchanged, `Retry-After` included. The response
   can't be retried once it has started reaching the client, so the decision
   is made as soon as the backend's headers arrive.

The client's response depends on why the request couldn't be served:

| Status | Meaning |
|---|---|
| `413 Content Too Large` | The request body was over `max_body_size` |
| `502 Bad Gateway` | A backend was tried and failed |
| `503 Service Unavailable` | No backend was up, so none was tried (or a backend's own `503`) |
| `504 Gateway Timeout` | `request_timeout` ran out |

If the client disconnects, sends a body over `max_body_size`, or
`request_timeout` runs out, the backend isn't blamed: none of these counts as
a backend failure. Backends marked down come back when their health check
succeeds again.

## Access log

Every request gets one line on stdout once it's finished, in `log/slog`'s
key=value format:

```
time=... level=INFO msg=request method=GET path=/status/404 status=404 bytes=14 duration=670µs client=[::1]:37894 backend=localhost:18081 attempts=2
```

`backend` is the last backend tried and `attempts` how many were, so retries
show up as `attempts=2` or more. The query string is left out, since it can
carry tokens. `status=0` means no response was sent because the client gave
up first. WebSockets are logged as `101` when they close, so their `duration`
is how long the connection was open. Set `access_log: false` to turn it off.

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
`requests` is attempts served in total.

The admin server also serves Go's runtime profiles under `/debug/pprof/` (see
[Performance](#performance)). Both show internal details, so it runs on a
separate port, which should stay on a loopback address.

## Reloading the config

Edit the config file, then send the process `SIGHUP`:

```sh
kill -HUP <pid>
```

The new config is checked first. If it's valid, new requests go to a balancer
built from it, while requests already in progress finish on the old one, so
none fail. If it isn't, the error is logged and the old config keeps running.

A reload starts afresh: every backend begins alive, with its failure count and
`/stats` counters at zero, and health checks restart on the new interval.

Some settings are fixed once the process has started, so a reload that
changes them is rejected as a whole: `listen`, `admin_listen`, `access_log`,
and raising `request_timeout` above its value at startup (the server's own
timeouts were sized from it). Restart to change these.

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

The fake backend's `/ws` accepts a plain `Connection: Upgrade` and echoes back
whatever it receives, which is all a proxy sees of a WebSocket.

`scripts/run-backends.sh` starts several at once (`BACKEND_FLAGS` passes flags
to all of them), and prints each PID so you can kill one to simulate a crash.

## Project layout

```
cmd/loadbalancer     the program: loads config, starts the servers, shuts down
cmd/fakebackend      a controllable backend for testing
internal/balancer    proxying, retries, timeouts, health checks, algorithms,
                     stats, access log
internal/config      the YAML file format
internal/admin       the /stats and /debug/pprof endpoints
scripts/             run-backends.sh, bench.sh
```

## Performance

Measured with [`wrk`](https://github.com/wg/wrk) on one laptop (AMD Ryzen 5
5600H, 6 cores / 12 threads, Linux 7.2, Go 1.27.1), with the client, the load
balancer and three fake backends all on the same machine. Backends answer at
once with a few bytes, so these numbers show the load balancer's own cost, not
a realistic workload.

`wrk -t4 -c50 -d30s`, median of 3 runs, access log off:

| Setup | Requests/sec | p50 | p99 |
|---|---|---|---|
| Direct to one backend | 240,563 | 136µs | 1.22ms |
| Load balancer, round-robin | 31,440 | 1.41ms | 4.14ms |
| Load balancer, least-connections | 29,707 | 1.47ms | 4.37ms |
| Load balancer, weighted-round-robin | 31,193 | 1.42ms | 4.15ms |
| Load balancer, weighted-least-connections | 29,569 | 1.48ms | 4.41ms |

To reproduce (requires `wrk`; `RUNS`, `DURATION`, `CONNECTIONS` and `THREADS`
can be overridden):

```sh
scripts/bench.sh
```

What the numbers show:

- **A second hop is expensive next to a trivial backend.** Each request is
  parsed twice and crosses four sockets instead of two. A CPU profile under
  load puts about a quarter of the load balancer's time in socket reads and
  writes, a fifth in memory allocation, and a tenth in garbage collection; the
  load balancer's own code is about 6%. With all processes sharing 12 threads,
  the machine is saturated, which widens the gap further.
- **The algorithms cost about the same.** The least-connections variants are
  about 5% slower, as each pick reads every backend's in-progress counter, and
  identical backends give them nothing to gain.
- **Connection reuse mattered most.** Go's default transport keeps only 2 idle
  connections per backend, so under load most requests opened a new one: about
  42,000 sockets piled up in `TIME_WAIT`, and round robin managed 21,000
  requests/sec at a p50 of 2.15ms. Keeping up to 100 idle connections per
  backend raised that by half.
- **wrk is closed-loop:** each connection waits for a response before sending
  the next request, so a stall delays the requests that would have measured
  it, and p99 can look better than it is (coordinated omission).

To profile, set `admin_listen` and, under load:

```sh
go tool pprof -http=: http://127.0.0.1:9000/debug/pprof/profile?seconds=20
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
- **Bad configuration fails at startup,** never while serving traffic; a bad
  config on reload is rejected and the old one keeps serving.

## Not included

These are real load balancer features, deliberately left out of scope:
TLS termination, HTTP/2 to backends, Prometheus metrics, structured error
logs (only the access log is structured), sticky sessions (consistent hashing), latency-based algorithms,
trusted proxies (behind a CDN or another proxy, forwarding headers describe
that proxy, not the real client), the standard `Forwarded` header, an
idle timeout for long-lived connections (a WebSocket or stream whose backend
goes silent stays open until one end closes it), and closing them cleanly on
shutdown.

## License

[MIT](LICENSE)
