// This is the Vite/React portal UI, not a Go package. The go.mod exists only so the parent
// module's `./...` (and gofmt/vet/test) never descend into ui/node_modules, where npm deps
// can vendor stray .go files. It is intentionally minimal and builds nothing.
module github.com/agent-lightning/agl-gateway/ui

go 1.26
