// Package version exposes the gateway's build version. It is a single string,
// stamped at release time by the linker, so nothing else in the tree needs to
// know how the value was produced.
package version

// Version is the gateway's semantic version. It defaults to "dev" for un-stamped
// local/source builds and is overridden at build time via the linker:
//
//	go build -ldflags "-X github.com/agent-lightning/agl-gateway/internal/version.Version=v1.2.3" ./cmd/gateway
//
// The Dockerfile and the publish-on-tag workflow pass the git tag here, so a
// released image reports its tag from GET /healthz.
var Version = "dev"
