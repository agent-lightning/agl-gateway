# syntax=docker/dockerfile:1

# ---- build ----
# golang:1.26-trixie is the latest Go on the latest Debian (13 "trixie"). The only
# third-party deps are pure Go (modernc.org/sqlite, gopkg.in/yaml.v3), so CGO stays
# off and the result is a fully static binary.
FROM golang:1.26-trixie AS build
WORKDIR /src

# Download modules first so they cache across source-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static, stripped binary. The portal SPA is embedded via go:embed, so no extra assets
# need to be copied into the runtime image. VERSION is stamped into the binary so it is
# reported by GET /healthz and `gateway -version`; it defaults to "dev" for local builds.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/agent-lightning/agl-gateway/internal/version.Version=${VERSION}" \
    -o /out/gateway ./cmd/gateway

# ---- run ----
# Latest Debian stable (slim). ca-certificates is required for outbound HTTPS to the
# upstream providers. Runs as a non-root user whose home (/data) holds the SQLite DB.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --create-home --home-dir /data agl

COPY --from=build /out/gateway /usr/local/bin/gateway

USER agl
WORKDIR /data
EXPOSE 8080

# Mount a config at /data/config.yaml and persist /data for the SQLite database, e.g.:
#   docker run -p 8080:8080 -v "$PWD/config.yaml:/data/config.yaml:ro" -v agl-data:/data agl-gateway
ENTRYPOINT ["/usr/local/bin/gateway"]
CMD ["-config", "/data/config.yaml"]
