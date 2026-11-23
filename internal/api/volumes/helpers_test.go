package volumes

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/volume"
)

// tarEntry is one entry of a test tar.
type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	// mode is 0o644 when 0.
	mode int64
}

func dir(name string) tarEntry        { return tarEntry{name: name, typeflag: tar.TypeDir} }
func file(name, body string) tarEntry { return tarEntry{name: name, typeflag: tar.TypeReg, body: body} }
func link(name, target string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeSymlink, linkname: target}
}

func makeTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: cmp.Or(entry.mode, 0o644), Size: int64(len(entry.body)), Linkname: entry.linkname}
		if entry.typeflag != tar.TypeReg {
			header.Size = 0
		}
		if entry.typeflag == tar.TypeXGlobalHeader {
			// Metadata for the whole archive, its body as a pax comment.
			header = &tar.Header{Typeflag: entry.typeflag, PAXRecords: map[string]string{"comment": entry.body}}
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// validTar has a folder the tar leaves out (docs), an executable and a link.
func validTar(t *testing.T) []byte {
	return makeTar(t,
		file("docs/guide/readme.md", "read me\n"),
		dir("bin"),
		tarEntry{name: "bin/run.sh", typeflag: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
		link("bin/readme", "../docs/guide/readme.md"),
	)
}

// testPolicy keeps the caps small enough to reach in a test.
var testPolicy = volume.TarPolicy(16, 1<<14, 1<<16)

// chunkedReader hands out data in pieces, waiting for each piece's turn on
// next, then fails with failAt bytes read if failAt is set. A wait ends like
// a read past its deadline once stop is closed.
type chunkedReader struct {
	data   []byte
	piece  int
	next   chan struct{}
	stop   chan struct{}
	failAt int
	read   int
}

func (reader *chunkedReader) Read(buf []byte) (int, error) {
	if reader.failAt > 0 && reader.read >= reader.failAt {
		return 0, errors.New("connection reset by peer")
	}
	if reader.read >= len(reader.data) {
		return 0, io.EOF
	}
	if reader.next != nil {
		select {
		case <-reader.next:
		case <-reader.stop:
			return 0, os.ErrDeadlineExceeded
		}
	}
	end := min(reader.read+reader.piece, len(reader.data), reader.read+len(buf))
	if reader.failAt > 0 {
		end = min(end, reader.failAt)
	}
	copied := copy(buf, reader.data[reader.read:end])
	reader.read += copied
	return copied, nil
}
