// Package portal serves the master-key-protected inspection and management SPA. The UI is a
// Vite/React app whose source lives at the repo root (/ui) and whose production build is
// emitted into ./dist, embedded here. Everything under /portal is served from that build;
// unknown sub-paths fall back to index.html so the client-side app owns routing.
package portal

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var assets embed.FS

// placeholder is served when the UI build is absent (only the dist anchor is present). The
// production build is emitted into ./dist by `npm --prefix ui run build` and is produced by
// CI and the Docker image rather than committed.
const placeholder = `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
	`<title>agl-gateway · portal</title><style>body{font:15px/1.6 system-ui,sans-serif;` +
	`background:#17151f;color:#e7e3f0;display:grid;place-items:center;min-height:100vh;margin:0}` +
	`code{background:#27232f;padding:.15em .4em;border-radius:4px}div{max-width:34rem;padding:2rem}` +
	`</style></head><body><div><h1>Portal not built</h1><p>The management UI has not been ` +
	`compiled. Build it with:</p><p><code>npm --prefix ui ci &amp;&amp; npm --prefix ui run build</code></p>` +
	`<p>CI and the Docker image do this automatically.</p></div></body></html>`

// Handler serves the embedded SPA under /portal and /portal/. Hashed asset files are served
// directly (and are safely long-cacheable); any other path returns index.html. When the UI
// has not been built, a placeholder page is served instead of failing.
func Handler() http.Handler {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		// The dist directory is embedded at build time; its absence is a build error.
		panic("portal: " + err.Error())
	}
	index, err := fs.ReadFile(dist, "index.html")
	built := err == nil
	if !built {
		index = []byte(placeholder)
	}
	fileServer := http.StripPrefix("/portal/", http.FileServerFS(dist))

	serveIndex := func(w http.ResponseWriter) {
		// index.html names content-hashed assets, so it must not be cached itself.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/portal")
		rel = strings.TrimPrefix(rel, "/")
		if built && rel != "" && rel != "index.html" {
			// Serve a real embedded file when it exists; otherwise fall through to the SPA.
			if f, err := dist.Open(rel); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w)
	})
}
