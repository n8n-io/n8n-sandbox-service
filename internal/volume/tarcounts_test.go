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
		"at the bounds":                  {TarCounts{Entries: MaxTarEntries, ContentBytes: MaxTarBytes, DecompressedBytes: MaxTarDecompressedBytes}, true},
		"no entries":                     {TarCounts{DecompressedBytes: 1024}, false},
		"too many entries":               {TarCounts{Entries: MaxTarEntries + 1, DecompressedBytes: 1024}, false},
		"negative content":               {TarCounts{Entries: 1, ContentBytes: -1, DecompressedBytes: 1024}, false},
		"too much content":               {TarCounts{Entries: 1, ContentBytes: MaxTarBytes + 1, DecompressedBytes: MaxTarDecompressedBytes}, false},
		"nothing decompressed":           {TarCounts{Entries: 1}, false},
		"too much decompressed":          {TarCounts{Entries: 1, DecompressedBytes: MaxTarDecompressedBytes + 1}, false},
		"content above the decompressed": {TarCounts{Entries: 1, ContentBytes: 2048, DecompressedBytes: 1024}, false},
	} {
		if err := tt.counts.Validate(); (err == nil) != tt.valid {
			t.Errorf("%s: Validate() = %v, want valid %t", name, err, tt.valid)
		}
	}
}

// The numbers are the runner cache's from before the formula moved here.
func TestRunnerBytes(t *testing.T) {
	small := TarCounts{Entries: 6, ContentBytes: 30, DecompressedBytes: 10240}
	if treeBytes, treeInodes := small.TreeRoom(); treeBytes != 44736 || treeInodes != 7 {
		t.Errorf("TreeRoom() = %d, %d; want 44736, 7", treeBytes, treeInodes)
	}
	if got := RunnerBytes(KindTar, 10240, small); got != 10240+44736 {
		t.Errorf("RunnerBytes(tar) = %d, want the tar and its tree, %d", got, 10240+44736)
	}
	const imageBytes = 1 << 20 // 1 MiB
	if got := RunnerBytes(KindExt4, imageBytes, small); got != imageBytes {
		t.Errorf("RunnerBytes(ext4) = %d, want the image's size", got)
	}

	// A tar at the API's publish limits would take about 16 GiB on a Docker
	// runner.
	const tarBytes = 4 << 30 // 4 GiB, as much as content
	atThePublishLimits := TarCounts{Entries: 1_000_000, ContentBytes: tarBytes, DecompressedBytes: 2 * tarBytes}
	if got := RunnerBytes(KindTar, tarBytes, atThePublishLimits); got != 17627124224 || got <= MaxSandboxVolumeBytes {
		t.Errorf("RunnerBytes() at the publish limits = %d, want 17627124224, over the sandbox cap", got)
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
