package volumestore

import (
	"errors"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

const testTenant = "0b6f3a52-7c1e-4d2a-9f3b-5e8d1c2a4b6f"

func TestKeyValidate(t *testing.T) {
	valid := []Key{
		{Scope: volume.SharedScope, ID: "python-3-12", Kind: volume.KindExt4},
		{Scope: testTenant, ID: strings.Repeat("ab", 32), Kind: volume.KindTar},
		{Scope: volume.SharedScope, ID: "0", Kind: volume.KindTar},
	}
	for _, key := range valid {
		if err := key.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", key, err)
		}
	}

	invalid := []Key{
		{Scope: "", ID: "a", Kind: volume.KindExt4},
		{Scope: "tenants", ID: "a", Kind: volume.KindExt4},
		{Scope: "__admin__", ID: "a", Kind: volume.KindExt4},
		{Scope: strings.ToUpper(testTenant), ID: "a", Kind: volume.KindExt4},
		{Scope: "{" + testTenant + "}", ID: "a", Kind: volume.KindExt4},
		{Scope: "../" + testTenant, ID: "a", Kind: volume.KindExt4},
		{Scope: volume.SharedScope + "/..", ID: "a", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "..", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "../x", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "a/b", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: `a\b`, Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "a.b", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "A", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "a b", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "a\x00", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "%2e%2e", Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: strings.Repeat("a", 65), Kind: volume.KindExt4},
		{Scope: volume.SharedScope, ID: "a", Kind: ""},
		{Scope: volume.SharedScope, ID: "a", Kind: "ext4/../x"},
		{Scope: volume.SharedScope, ID: "a", Kind: "squashfs"},
	}
	for _, key := range invalid {
		if err := key.Validate(); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidKey", key, err)
		}
	}
}

func TestObjectName(t *testing.T) {
	cases := map[Key]string{
		{Scope: volume.SharedScope, ID: "abc", Kind: volume.KindExt4}: "shared/abc.ext4",
		{Scope: testTenant, ID: "abc", Kind: volume.KindTar}:          "tenants/" + testTenant + "/abc.tar",
	}
	for key, want := range cases {
		if got := key.objectName(); got != want {
			t.Errorf("objectName(%+v) = %q, want %q", key, got, want)
		}
		got, err := parseObjectName(want)
		if err != nil || got != key {
			t.Errorf("parseObjectName(%q) = %+v, %v; want %+v", want, got, err, key)
		}
	}
}

func TestParseObjectNameRejectsAnythingElse(t *testing.T) {
	for _, name := range []string{
		"",
		"shared",
		"shared/",
		"shared/abc",
		"shared/abc.",
		"shared/.ext4",
		"shared/abc.ext4/",
		"/shared/abc.ext4",
		"shared//abc.ext4",
		"shared/../shared/abc.ext4",
		"shared/abc.tar.ext4",
		"tenants/abc.ext4",
		"tenants/shared/abc.ext4",
		"tenants/" + testTenant + "/../abc.ext4",
		"tenants/" + testTenant + "/x/abc.ext4",
		testTenant + "/abc.ext4",
		"other/abc.ext4",
	} {
		if key, err := parseObjectName(name); err == nil {
			t.Errorf("parseObjectName(%q) = %+v, want an error", name, key)
		}
	}
}
