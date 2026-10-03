FROM golang:1.27 AS build

WORKDIR /src

# Modules first, so editing source doesn't download them again.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binaries, as the final image has no C library. cmd/loadbalancer's
# default.pgo is picked up automatically.
RUN --mount=type=cache,target=/root/.cache/go-build \
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/loadbalancer /out/fakebackend /

EXPOSE 8000 9000

# Exec form, so the program is PID 1 and gets SIGTERM from docker stop.
# Compose runs the fake backends by replacing it.
CMD ["/loadbalancer", "-config", "/etc/loadbalancer/config.yaml"]
