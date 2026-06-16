# syntax=docker/dockerfile:1

# ---- build ----
# golang:1.26-trixie is the latest Go on the latest Debian (13 "trixie"). The only
# third-party deps are pure Go (modernc.org/sqlite, gopkg.in/yaml.v3), so CGO stays
# off and the result is a fully static binary.
FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS build
WORKDIR /src

# Download modules first so they cache across source-only changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# Static, stripped binary. The portal SPA is embedded via go:embed, so no extra assets
# need to be copied into the runtime image. VERSION is stamped into the binary so it is
# reported by GET /healthz and `gateway -version`; it defaults to "dev" for local builds.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X github.com/agent-lightning/agl-gateway/internal/version.Version=${VERSION}" \
    -o /out/gateway ./cmd/gateway \
 && mkdir -p /out/data /out/tmp \
 && chmod 1777 /out/tmp

# ---- certs ----
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS certs
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# ---- run ----
# Static scratch image. CA certificates are required for outbound HTTPS to upstream
# providers, and /data holds the SQLite DB.
FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/gateway /usr/local/bin/gateway
COPY --from=build --chown=10001:10001 /out/data /data
COPY --from=build --chown=10001:10001 /out/tmp /tmp

USER 10001:10001
WORKDIR /data
EXPOSE 8080

# Mount a config at /data/config.yaml and persist /data for the SQLite database, e.g.:
#   docker run -p 8080:8080 -v "$PWD/config.yaml:/data/config.yaml:ro" -v agl-data:/data agl-gateway
ENTRYPOINT ["/usr/local/bin/gateway"]
CMD ["-config", "/data/config.yaml"]
