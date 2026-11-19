package archive

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func seal(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func open(key, sealed []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(sealed), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestRoundTrip(t *testing.T) {
	key, _ := NewKey()
	for _, n := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 17} {
		plain := make([]byte, n)
		_, _ = rand.Read(plain)
		got, err := open(key, seal(t, key, plain))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%d bytes: err=%v equal=%v", n, err, bytes.Equal(got, plain))
		}
	}
}

// Every tampering is ErrCorrupt: another key, a flipped bit, a truncated or
// extended object, chunks reordered or dropped.
func TestTamperingIsRefused(t *testing.T) {
	key, _ := NewKey()
	other, _ := NewKey()
	plain := make([]byte, 3*chunkSize+5)
	_, _ = rand.Read(plain)
	sealed := seal(t, key, plain)
	chunk := 4 + chunkSize + 16
	flipped := append([]byte(nil), sealed...)
	flipped[len(magic)+100] ^= 1
	reordered := append([]byte(nil), sealed[:len(magic)]...)
	reordered = append(reordered, sealed[len(magic)+chunk:len(magic)+2*chunk]...)
	reordered = append(reordered, sealed[len(magic):len(magic)+chunk]...)
	reordered = append(reordered, sealed[len(magic)+2*chunk:]...)
	dropped := append(append([]byte(nil), sealed[:len(magic)]...), sealed[len(magic)+chunk:]...)
	for name, tc := range map[string]struct{ key, data []byte }{
		"other key":     {other, sealed},
		"flipped bit":   {key, flipped},
		"truncated":     {key, sealed[:len(sealed)-chunk/2]},
		"chunk aligned": {key, sealed[:len(magic)+2*chunk]},
		"extended":      {key, append(append([]byte(nil), sealed...), 0)},
		"reordered":     {key, reordered},
		"dropped chunk": {key, dropped},
		"empty":         {key, nil},
	} {
		if _, err := open(tc.key, tc.data); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestWrapKey(t *testing.T) {
	master, _ := NewKey()
	key, _ := NewKey()
	wrapped, err := WrapKey(master, key)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := UnwrapKey(master, wrapped); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("unwrap: %v", err)
	}
	other, _ := NewKey()
	if _, err := UnwrapKey(other, wrapped); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("other master: %v", err)
	}
}

func FuzzReader(f *testing.F) {
	key := bytes.Repeat([]byte{7}, KeySize)
	var out bytes.Buffer
	w, _ := NewWriter(&out, key)
	_, _ = w.Write([]byte("hello archive"))
	_ = w.Close()
	f.Add(out.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := open(key, data)
		if err == nil && !bytes.Equal(data, out.Bytes()) && len(got) > 0 {
			// Opening anything but a genuine archive would be a forgery.
			t.Fatalf("forged archive opened: %q", got)
		}
	})
}

// A failed read is the reader's error, not ErrCorrupt: it says nothing about
// the archive.
func TestReadErrorIsNotCorrupt(t *testing.T) {
	key, _ := NewKey()
	sealed := seal(t, key, make([]byte, 2*chunkSize))
	broken := errors.New("connection reset")
	for _, at := range []int{0, len(magic), len(magic) + 2, len(magic) + chunkSize, len(sealed)} {
		r, err := NewReader(io.MultiReader(bytes.NewReader(sealed[:at]), iotestErr{broken}), key)
		if err == nil {
			_, err = io.ReadAll(r)
		}
		if !errors.Is(err, broken) {
			t.Errorf("failing at %d: err = %v", at, err)
		}
	}
}

type iotestErr struct{ err error }

func (e iotestErr) Read([]byte) (int, error) { return 0, e.err }
