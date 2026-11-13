package volume

import (
	"strings"
	"testing"
)

func TestValidScope(t *testing.T) {
	valid := []string{
		SharedScope,
		"0b6f8f2e-6a3c-4c1d-9f5e-2a7b8c9d0e1f",
	}
	for _, scope := range valid {
		if !ValidScope(scope) {
			t.Errorf("ValidScope(%q) = false, want true", scope)
		}
	}

	invalid := []string{
		"",
		"Shared",
		"shared/",
		"tenants",
		"..",
		"../shared",
		"0B6F8F2E-6A3C-4C1D-9F5E-2A7B8C9D0E1F",
		"0b6f8f2e6a3c4c1d9f5e2a7b8c9d0e1f",
		"0b6f8f2e-6a3c-4c1d-9f5e-2a7b8c9d0e1f/..",
		" 0b6f8f2e-6a3c-4c1d-9f5e-2a7b8c9d0e1f",
	}
	for _, scope := range invalid {
		if ValidScope(scope) {
			t.Errorf("ValidScope(%q) = true, want false", scope)
		}
	}
}

func TestValidDigest(t *testing.T) {
	digest := strings.Repeat("0123456789abcdef", 4)
	if !ValidDigest(digest) {
		t.Errorf("ValidDigest(%q) = false, want true", digest)
	}

	invalid := []string{
		"",
		digest[:63],
		digest + "0",
		strings.ToUpper(digest),
		"sha256:" + digest,
		strings.Repeat("g", 64),
	}
	for _, candidate := range invalid {
		if ValidDigest(candidate) {
			t.Errorf("ValidDigest(%q) = true, want false", candidate)
		}
	}
}
