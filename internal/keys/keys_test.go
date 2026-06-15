package keys

import "testing"

func TestGenerateAndHash(t *testing.T) {
	k1, disp1, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	k2, _, _ := Generate()
	if k1 == k2 {
		t.Error("Generate produced identical keys")
	}
	if len(k1) <= len(Prefix) || k1[:len(Prefix)] != Prefix {
		t.Errorf("key %q missing prefix %q", k1, Prefix)
	}
	if disp1 != k1[:len(Prefix)+8] {
		t.Errorf("display = %q, want prefix of key", disp1)
	}
	if Hash(k1) == Hash(k2) {
		t.Error("distinct keys hashed equal")
	}
	if Hash(k1) != Hash(k1) {
		t.Error("Hash not deterministic")
	}
	if len(Hash(k1)) != 64 {
		t.Errorf("hash len = %d, want 64 hex chars", len(Hash(k1)))
	}
}
