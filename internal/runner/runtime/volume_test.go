package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

const (
	testToken      = "sig=secret-token"
	testDigest     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testURL        = "https://objects.example/shared/image.ext4?" + testToken
	testVolumeJSON = `{"scope":"shared","sha256":"` + testDigest + `","size":4096,"kind":"ext4","url":"` + testURL + `","mount_path":"/mnt/a"}`
)

func testVolume() Volume {
	return Volume{Scope: volume.SharedScope, SHA256: testDigest, Size: 4096, Kind: volume.KindExt4, URL: testURL, MountPath: "/mnt/a"}
}

func optionsJSON(t *testing.T, volumes ...Volume) string {
	t.Helper()
	raw, err := json.Marshal(CreateOptions{Volumes: volumes})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseCreateOptionsVolumes(t *testing.T) {
	second := testVolume()
	second.SHA256 = strings.Repeat("f", 64)
	second.MountPath = "/mnt/b"
	second.Optional = true
	second.Kind = volume.KindTar
	second.Size = 1
	second.Tar = volume.TarCounts{Entries: 3, ContentBytes: 10, DecompressedBytes: 2048}

	opts, err := ParseCreateOptions(optionsJSON(t, testVolume(), second))
	if err != nil {
		t.Fatalf("ParseCreateOptions() error = %v", err)
	}
	if len(opts.Volumes) != 2 || opts.Volumes[0] != testVolume() || opts.Volumes[1] != second {
		t.Fatalf("volumes = %v, want both as sent", opts.Volumes)
	}

	opts, err = ParseCreateOptions(`{"volumes":[]}`)
	if err != nil || opts.Volumes != nil {
		t.Fatalf("ParseCreateOptions(empty list) = %+v, %v; want no volumes", opts, err)
	}
}

func TestParseCreateOptionsRefusesInvalidVolumes(t *testing.T) {
	with := func(change func(*Volume)) Volume {
		vol := testVolume()
		change(&vol)
		return vol
	}
	sibling := with(func(v *Volume) { v.SHA256 = strings.Repeat("e", 64); v.MountPath = "/mnt/b" })
	tooMany := make([]Volume, volume.MaxDrives+1)
	for index := range tooMany {
		tooMany[index] = with(func(v *Volume) {
			v.SHA256 = fmt.Sprintf("%064x", index)
			v.MountPath = fmt.Sprintf("/mnt/v%d", index)
		})
	}

	tests := []struct {
		name    string
		volumes []Volume
		wantErr string
	}{
		{"too many", tooMany, "at most 15 volumes, got 16"},
		{"bad scope", []Volume{with(func(v *Volume) { v.Scope = "../shared" })}, `volume 0: invalid scope "../shared"`},
		{"bad digest", []Volume{with(func(v *Volume) { v.SHA256 = "sha256:" + testDigest })}, "volume 0: sha256 must be"},
		{"zero size", []Volume{with(func(v *Volume) { v.Size = 0 })}, "volume 0: size must be positive"},
		{"negative size", []Volume{with(func(v *Volume) { v.Size = -1 })}, "volume 0: size must be positive"},
		{"unknown kind", []Volume{with(func(v *Volume) { v.Kind = "squashfs" })}, `volume 0: unknown kind "squashfs"`},
		{"no kind", []Volume{with(func(v *Volume) { v.Kind = "" })}, `volume 0: unknown kind ""`},
		{"ext4 not whole sectors", []Volume{with(func(v *Volume) { v.Size = 4097 })}, "whole 512-byte sectors"},
		{"ext4 with tar counts", []Volume{with(func(v *Volume) { v.Tar = volume.TarCounts{Entries: 1, DecompressedBytes: 1024} })}, "volume 0: only a tar volume has tar counts"},
		{"tar without counts", []Volume{with(func(v *Volume) { v.Kind = volume.KindTar })}, "volume 0: tar counts: entries must be"},
		{"tar with invalid counts", []Volume{with(func(v *Volume) {
			v.Kind = volume.KindTar
			v.Tar = volume.TarCounts{Entries: 1, ContentBytes: 4096, DecompressedBytes: 1024}
		})}, "volume 0: tar counts: content bytes 4096 exceed"},
		{"no url", []Volume{with(func(v *Volume) { v.URL = "" })}, "volume 0: url must be an https URL"},
		{"http url", []Volume{with(func(v *Volume) { v.URL = "http://objects.example/x?" + testToken })}, "url must be an https URL"},
		{"url without host", []Volume{with(func(v *Volume) { v.URL = "https:///x?" + testToken })}, "url must be an https URL"},
		{"unparsable url", []Volume{with(func(v *Volume) { v.URL = "https://objects.example/%zz?" + testToken })}, "url must be an https URL"},
		{"mount path outside mnt", []Volume{with(func(v *Volume) { v.MountPath = "/etc" })}, "volume 0: mount path: path \"/etc\" is not below /mnt/"},
		{"mount path not clean", []Volume{with(func(v *Volume) { v.MountPath = "/mnt/a/../b" })}, "not absolute and clean"},
		{"same object twice", []Volume{testVolume(), with(func(v *Volume) { v.MountPath = "/mnt/b" })}, "volume 1: same object as volume 0"},
		{"same path", []Volume{testVolume(), with(func(v *Volume) { v.SHA256 = sibling.SHA256 })}, `volume 1: mount path "/mnt/a" overlaps "/mnt/a"`},
		{"nested path", []Volume{sibling, with(func(v *Volume) { v.MountPath = "/mnt/b/c" })}, `volume 1: mount path "/mnt/b/c" overlaps "/mnt/b"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCreateOptions(optionsJSON(t, tt.volumes...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseCreateOptions() error = %v, want one containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error %q contains the URL's token", err)
			}
		})
	}
}

func TestParseCreateOptionsErrorsLeaveOutTheURL(t *testing.T) {
	for _, input := range []string{
		`{"volumes":[{"url":"` + testURL + `","scope":1}]}`,
		`{"volumes":[{"url":"` + testURL + `"}]}`,
		`{"volumes":[{"url":"` + testURL + `"},`,
		`{"volumes":{"url":"` + testURL + `"}}`,
	} {
		if _, err := ParseCreateOptions(input); err == nil || strings.Contains(err.Error(), testToken) {
			t.Errorf("ParseCreateOptions(%s) error = %v, want one without the URL", input, err)
		}
	}
}

func TestAppliedCreateOptions(t *testing.T) {
	optional := testVolume()
	optional.SHA256 = strings.Repeat("f", 64)
	optional.MountPath = "/mnt/b"
	optional.Optional = true
	opts := &CreateOptions{Egress: EgressNone, Volumes: []Volume{testVolume(), optional}}

	raw, err := json.Marshal(opts.Applied(map[int]string{1: VolumeLeftOutDownload}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"egress":"none","volumes":[` +
		`{"scope":"shared","sha256":"` + testDigest + `","size":4096,"kind":"ext4","mount_path":"/mnt/a","attached":true},` +
		`{"scope":"shared","sha256":"` + optional.SHA256 + `","size":4096,"kind":"ext4","mount_path":"/mnt/b","optional":true,"attached":false,"reason":"download"}]}`
	if string(raw) != want {
		t.Fatalf("echo =\n%s\nwant\n%s", raw, want)
	}

	raw, err = json.Marshal((&CreateOptions{Egress: EgressPublic}).Applied(nil))
	if err != nil || string(raw) != `{"egress":"public"}` {
		t.Fatalf("echo without volumes = %s, %v; want the echo from before volumes", raw, err)
	}
}

func TestVolumesLeaveTheURLOutOfLogsAndFormatting(t *testing.T) {
	opts, err := ParseCreateOptions(`{"volumes":[` + testVolumeJSON + `]}`)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("create", "opts", opts, "options", *opts, "volume", opts.Volumes[0], "volumes", opts.Volumes)
	formatted := fmt.Sprintf("%v %+v %#v %v %+v %#v %s", opts, *opts, *opts, opts.Volumes, opts.Volumes[0], opts.Volumes[0], opts.Volumes[0])

	for _, out := range []string{logs.String(), formatted} {
		if strings.Contains(out, testToken) || strings.Contains(out, "objects.example") {
			t.Errorf("output names the URL:\n%s", out)
		}
		if !strings.Contains(out, testDigest) {
			t.Errorf("output does not name the volume:\n%s", out)
		}
	}
}
