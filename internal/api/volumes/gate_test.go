package volumes

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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
			gate := newTarGate(bytes.NewReader(data), testPolicy, nil)
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
		"only metadata":    {makeTar(t, tarEntry{typeflag: tar.TypeXGlobalHeader, body: "secret-name"}), ErrInvalidTar},
		"not a tar":        {[]byte(strings.Repeat("secret-name ", 200)), ErrInvalidTar},
		"too many entries": {makeTar(t, tooMany...), ErrTooLarge},
		"too much content": {makeTar(t, file("secret-name", strings.Repeat("x", 1<<14+1))), ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			gate := newTarGate(bytes.NewReader(tc.data), testPolicy, nil)
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
	for name, data := range map[string][]byte{"plain": validTar(t), "gzip": gzipped(t, validTar(t))} {
		t.Run(name, func(t *testing.T) {
			gate := newTarGate(&chunkedReader{data: data, piece: 64, failAt: len(data) / 2}, testPolicy, nil)
			defer gate.close()
			if _, err := readGate(gate); !errors.Is(err, ErrUpload) {
				t.Fatalf("the read ended with %v, want ErrUpload", err)
			}
		})
	}
}

// A client that stalls after sending an entry the validator refuses gets the
// tar's error at once, not when its read times out.
func TestTarGateInterruptsAReadOnceTheTarIsRefused(t *testing.T) {
	reader := &chunkedReader{data: makeTar(t, file("../outside", "x")), piece: 512, next: make(chan struct{}, 1), stop: make(chan struct{})}
	reader.next <- struct{}{}
	var stopOnce sync.Once
	stopReading := func() { stopOnce.Do(func() { close(reader.stop) }) }
	var interrupts atomic.Int32
	gate := newTarGate(reader, testPolicy, func() {
		interrupts.Add(1)
		stopReading()
	})
	defer gate.close()
	ended := make(chan error, 1)
	go func() {
		_, err := readGate(gate)
		ended <- err
	}()
	select {
	case err := <-ended:
		if !errors.Is(err, ErrInvalidTar) || errors.Is(err, ErrUpload) || interrupts.Load() != 1 {
			t.Fatalf("the read ended with %v after %d interrupts, want ErrInvalidTar after 1", err, interrupts.Load())
		}
	case <-time.After(5 * time.Second):
		stopReading()
		<-ended
		t.Fatal("the read kept waiting for the client after the tar was refused")
	}
}

// Once the body has ended, a refused tar does not interrupt it.
func TestTarGateLeavesAnEndedBodyAlone(t *testing.T) {
	var interrupts atomic.Int32
	gate := newTarGate(bytes.NewReader(makeTar(t)), testPolicy, func() { interrupts.Add(1) })
	defer gate.close()
	if _, err := readGate(gate); !errors.Is(err, ErrInvalidTar) || interrupts.Load() != 0 {
		t.Fatalf("the read ended with %v after %d interrupts, want ErrInvalidTar after none", err, interrupts.Load())
	}
}

// A store that stops reading early must not leave the validator running.
func TestTarGateCloseEndsTheValidator(t *testing.T) {
	gate := newTarGate(bytes.NewReader(validTar(t)), testPolicy, nil)
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
