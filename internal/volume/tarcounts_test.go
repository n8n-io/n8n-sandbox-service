package volume

import (
	"archive/tar"
	"bytes"
	"errors"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/safetar"
)

func TestTarCountsValidate(t *testing.T) {
	for name, tt := range map[string]struct {
		counts TarCounts
		valid  bool
	}{
		"one directory":                  {TarCounts{Entries: 1, DecompressedBytes: 1024}, true},
		"at the bounds":                  {TarCounts{Entries: maxTarEntries, ContentBytes: maxTarContentBytes, DecompressedBytes: maxTarDecompressedBytes}, true},
		"no entries":                     {TarCounts{DecompressedBytes: 1024}, false},
		"too many entries":               {TarCounts{Entries: maxTarEntries + 1, DecompressedBytes: 1024}, false},
		"negative content":               {TarCounts{Entries: 1, ContentBytes: -1, DecompressedBytes: 1024}, false},
		"too much content":               {TarCounts{Entries: 1, ContentBytes: maxTarContentBytes + 1, DecompressedBytes: maxTarDecompressedBytes}, false},
		"nothing decompressed":           {TarCounts{Entries: 1}, false},
		"too much decompressed":          {TarCounts{Entries: 1, DecompressedBytes: maxTarDecompressedBytes + 1}, false},
		"content above the decompressed": {TarCounts{Entries: 1, ContentBytes: 2048, DecompressedBytes: 1024}, false},
	} {
		if err := tt.counts.Validate(); (err == nil) != tt.valid {
			t.Errorf("%s: Validate() = %v, want valid %t", name, err, tt.valid)
		}
	}
}

func TestTarCountsPolicyHoldsTheTarToItsCounts(t *testing.T) {
	body := testTar(t,
		tar.Header{Name: "dir/a", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "../elsewhere"},
	)
	exact := TarCounts{Entries: 3, ContentBytes: 5, DecompressedBytes: int64(len(body))}
	if _, err := safetar.Validate(bytes.NewReader(body), exact.Policy()); err != nil {
		t.Fatalf("at its counts: %v", err)
	}
	for name, counts := range map[string]TarCounts{
		"entries":            {Entries: 2, ContentBytes: 5, DecompressedBytes: int64(len(body))},
		"content":            {Entries: 3, ContentBytes: 4, DecompressedBytes: int64(len(body))},
		"decompressed bytes": {Entries: 3, ContentBytes: 5, DecompressedBytes: int64(len(body)) - 1},
	} {
		if _, err := safetar.Validate(bytes.NewReader(body), counts.Policy()); !errors.Is(err, safetar.ErrTooLarge) {
			t.Errorf("one %s too few: got %v, want %v", name, err, safetar.ErrTooLarge)
		}
	}
}
