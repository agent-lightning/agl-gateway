package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesHTML(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/portal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "agl-gateway") {
		t.Error("portal HTML missing expected content")
	}
}
