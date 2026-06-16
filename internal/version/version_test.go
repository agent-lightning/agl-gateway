package version

import "testing"

// Version defaults to "dev" for un-stamped builds. Releases override it via the
// linker (-X), which this test cannot exercise; it only guards the default so an
// accidental hardcoded release number never lands in source.
func TestDefault(t *testing.T) {
	if Version != "dev" {
		t.Errorf("Version = %q, want %q (release numbers are stamped via -ldflags, not committed)", Version, "dev")
	}
}
