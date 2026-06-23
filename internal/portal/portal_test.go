package portal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// get drives the portal handler for a path and returns the recorder.
func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

// requireBuilt skips a test when the UI has not been compiled into ./dist (only the embed
// anchor is present). CI and the Docker image build the UI first, so these run there; a
// bare `go test ./...` without a prior `npm run build` skips them instead of failing.
func requireBuilt(t *testing.T) {
	t.Helper()
	if _, err := assets.ReadFile("dist/index.html"); err != nil {
		t.Skip("portal UI not built (run `npm --prefix ui run build`)")
	}
}

func TestServesSPA(t *testing.T) {
	requireBuilt(t)
	rec := get(t, "/portal")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "agl-gateway") {
		t.Error("portal HTML missing expected title")
	}
	// The build emits hashed assets referenced under the /portal/ base.
	if !strings.Contains(body, "/portal/assets/") {
		t.Error("portal HTML does not reference hashed assets under /portal/")
	}
}

func TestServesHashedAssetAndWiring(t *testing.T) {
	requireBuilt(t)
	// Find the JS bundle the index references, fetch it, and confirm it is served as a real
	// file and carries the control-plane wiring (model test + logs endpoints).
	html := get(t, "/portal/").Body.String()
	m := regexp.MustCompile(`/portal/(assets/index-[^"']+\.js)`).FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("could not find JS bundle reference in index.html")
	}

	rec := get(t, "/portal/"+m[1])
	if rec.Code != http.StatusOK {
		t.Fatalf("asset status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("asset content-type = %q, want javascript", ct)
	}
	js := rec.Body.String()
	for _, want := range []string{"/admin/test", "/admin/logs", "/admin/keys"} {
		if !strings.Contains(js, want) {
			t.Errorf("bundle missing wiring for %q", want)
		}
	}
}

func TestUnknownRouteFallsBackToIndex(t *testing.T) {
	requireBuilt(t)
	// Client-side routes (no matching file) return index.html, not 404.
	rec := get(t, "/portal/does/not/exist")
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Error("fallback did not serve the SPA shell")
	}
}

func TestAlwaysServesHTML(t *testing.T) {
	// Whether or not the UI is built, /portal must return 200 HTML (the SPA, or a
	// build-instructions placeholder) — never a 404 or a panic.
	rec := get(t, "/portal")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
}

func TestServesFavicon(t *testing.T) {
	requireBuilt(t)
	rec := get(t, "/portal/favicon.svg")
	if rec.Code != http.StatusOK {
		t.Fatalf("favicon status = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if len(body) == 0 {
		t.Error("favicon body is empty")
	}
}
