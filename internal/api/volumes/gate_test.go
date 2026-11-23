package volumes

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

// readGate reads the gate the way the volume store's Put does, and returns
// what it read and the error that ended the read.
func readGate(gate *tarGate) ([]byte, error) {
	var out bytes.Buffer
	buf := make([]byte, 512)
	for {
		read, err := gate.Read(buf)
		out.Write(buf[:read])
		if err != nil {
			return out.Bytes(), err
		}
	}
}

func TestTarGatePassesAValidTar(t *testing.T) {
	plain := validTar(t)
	for name, data := range map[string][]byte{"plain": plain, "gzip": gzipped(t, plain)} {
		t.Run(name, func(t *testing.T) {
			gate := newTarGate(bytes.NewReader(data), testPolicy)
			defer gate.close()
			read, err := readGate(gate)
			if err != io.EOF || gate.err() != nil {
				t.Fatalf("the read ended with %v, err() = %v; want io.EOF and nil", err, gate.err())
			}
			if !bytes.Equal(read, data) {
				t.Fatal("the gate changed the bytes it passed on")
			}
			// docs and docs/guide are left out of the tar but counted.
			want := volume.TarCounts{Entries: 6, ContentBytes: 18, DecompressedBytes: int64(len(plain))}
			if got := gate.counts(); got != want {
				t.Fatalf("counts() = %+v, want %+v", got, want)
			}
		})
	}
}

// A tar the validator refuses ends with an error instead of io.EOF, so the
// store never commits it, and the error names no entry of the tar.
func TestTarGateRefusesATarBeforeItsEnd(t *testing.T) {
	var tooMany []tarEntry
	for index := range 17 {
		tooMany = append(tooMany, file(fmt.Sprintf("secret-name-%d", index), "x"))
	}
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"hard link":        {makeTar(t, file("a", "x"), tarEntry{name: "secret-name", typeflag: tar.TypeLink, linkname: "a"}), ErrInvalidTar},
		"absolute path":    {makeTar(t, file("/secret-name", "x")), ErrInvalidTar},
		"path out of tree": {makeTar(t, file("../secret-name", "x")), ErrInvalidTar},
		"through a link":   {makeTar(t, link("dir", "elsewhere"), file("dir/secret-name", "x")), ErrInvalidTar},
		"no entries":       {makeTar(t), ErrInvalidTar},
		"not a tar":        {[]byte(strings.Repeat("secret-name ", 200)), ErrInvalidTar},
		"too many entries": {makeTar(t, tooMany...), ErrTooLarge},
		"too much content": {makeTar(t, file("secret-name", strings.Repeat("x", 1<<14+1))), ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			gate := newTarGate(bytes.NewReader(tc.data), testPolicy)
			defer gate.close()
			_, err := readGate(gate)
			if err == io.EOF || !errors.Is(err, tc.want) || !errors.Is(gate.err(), tc.want) {
				t.Fatalf("the read ended with %v, err() = %v; want %v", err, gate.err(), tc.want)
			}
			if strings.Contains(err.Error(), "secret-name") {
				t.Fatalf("the error %q names an entry of the tar", err)
			}
		})
	}
}

func TestTarGateReportsAFailedUpload(t *testing.T) {
	gate := newTarGate(&chunkedReader{data: validTar(t), piece: 512, failAt: 1024}, testPolicy)
	defer gate.close()
	if _, err := readGate(gate); !errors.Is(err, ErrUpload) {
		t.Fatalf("the read ended with %v, want ErrUpload", err)
	}
}

// A store that stops reading early must not leave the validator running.
func TestTarGateCloseEndsTheValidator(t *testing.T) {
	gate := newTarGate(bytes.NewReader(validTar(t)), testPolicy)
	if _, err := gate.Read(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		gate.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close() kept waiting for the validator")
	}
}
