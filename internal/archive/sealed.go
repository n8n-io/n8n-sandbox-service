// Package archive seals sandbox archives: a stream encrypted and
// authenticated with a key used for that one archive, so the object store
// alone reveals no files and an altered, reordered or truncated object fails
// to open.
package archive

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
)

// KeySize is the size of an archive key (AES-256).
const KeySize = 32

// chunkSize is the plaintext of one sealed chunk.
const chunkSize = 64 << 10

var magic = []byte("n8n-sandbox-archive-v1\n")

// ErrCorrupt is any archive that does not open: altered, truncated, reordered
// or sealed with another key.
var ErrCorrupt = errors.New("archive is corrupt or not sealed with this key")

// NewKey returns a fresh random archive key.
func NewKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, errors.New("archive key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// nonce is the chunk counter: the key is used for one archive only, so a
// counter never repeats under it.
func nonce(counter uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], counter)
	return n
}

// aad binds a chunk to its place and to whether it is the last one.
func aad(counter uint64, final bool) []byte {
	b := make([]byte, len(magic)+9)
	copy(b, magic)
	binary.BigEndian.PutUint64(b[len(magic):], counter)
	if final {
		b[len(b)-1] = 1
	}
	return b
}

// Writer seals what is written to it. Close writes the final chunk; without
// it the archive does not open.
type Writer struct {
	w       io.Writer
	aead    cipher.AEAD
	buf     []byte
	counter uint64
	started bool
	closed  bool
}

// NewWriter seals to w with key.
func NewWriter(w io.Writer, key []byte) (*Writer, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Writer{w: w, aead: aead, buf: make([]byte, 0, chunkSize)}, nil
}

func (s *Writer) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("archive writer closed")
	}
	n := len(p)
	for len(p) > 0 {
		k := min(chunkSize-len(s.buf), len(p))
		s.buf = append(s.buf, p[:k]...)
		p = p[k:]
		// A full chunk waits until more arrives, so the last one written is
		// always the one Close marks final.
		if len(s.buf) == chunkSize && len(p) > 0 {
			if err := s.flush(false); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

func (s *Writer) flush(final bool) error {
	if !s.started {
		if _, err := s.w.Write(magic); err != nil {
			return err
		}
		s.started = true
	}
	sealed := s.aead.Seal(nil, nonce(s.counter), s.buf, aad(s.counter, final))
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(sealed)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := s.w.Write(sealed); err != nil {
		return err
	}
	s.counter++
	s.buf = s.buf[:0]
	return nil
}

// Close seals the last chunk.
func (s *Writer) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

// Reader opens a sealed archive. It returns ErrCorrupt for anything that does
// not authenticate, an archive that ends before its final chunk included.
type Reader struct {
	r       io.Reader
	aead    cipher.AEAD
	plain   []byte
	counter uint64
	started bool
	done    bool
}

// NewReader opens r with key.
func NewReader(r io.Reader, key []byte) (*Reader, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Reader{r: r, aead: aead}, nil
}

func (o *Reader) Read(p []byte) (int, error) {
	for len(o.plain) == 0 {
		if o.done {
			return 0, io.EOF
		}
		if err := o.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, o.plain)
	o.plain = o.plain[n:]
	return n, nil
}

func (o *Reader) next() error {
	if !o.started {
		got := make([]byte, len(magic))
		if _, err := io.ReadFull(o.r, got); err != nil {
			return readErr(err)
		}
		if !bytes.Equal(got, magic) {
			return ErrCorrupt
		}
		o.started = true
	}
	var hdr [4]byte
	if _, err := io.ReadFull(o.r, hdr[:]); err != nil {
		return readErr(err) // at EOF: ended before the final chunk
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < uint32(o.aead.Overhead()) || n > chunkSize+uint32(o.aead.Overhead()) {
		return ErrCorrupt
	}
	sealed := make([]byte, n)
	if _, err := io.ReadFull(o.r, sealed); err != nil {
		return readErr(err)
	}
	// Try the chunk as an inner one first, then as the final one.
	if plain, err := o.aead.Open(nil, nonce(o.counter), sealed, aad(o.counter, false)); err == nil {
		o.plain, o.counter = plain, o.counter+1
		return nil
	}
	plain, err := o.aead.Open(nil, nonce(o.counter), sealed, aad(o.counter, true))
	if err != nil {
		return ErrCorrupt
	}
	// Nothing may follow the final chunk.
	var extra [1]byte
	switch _, err := io.ReadFull(o.r, extra[:]); {
	case err == nil:
		return ErrCorrupt
	case !errors.Is(err, io.EOF):
		return err
	}
	o.plain, o.counter, o.done = plain, o.counter+1, true
	return nil
}

// WrapKey seals an archive key under the API's master key, for storage.
func WrapKey(master, key []byte) ([]byte, error) {
	aead, err := newAEAD(master)
	if err != nil {
		return nil, err
	}
	n := make([]byte, aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return append(n, aead.Seal(nil, n, key, []byte("archive-key"))...), nil
}

// UnwrapKey opens a key WrapKey sealed.
func UnwrapKey(master, wrapped []byte) ([]byte, error) {
	aead, err := newAEAD(master)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < aead.NonceSize() {
		return nil, ErrCorrupt
	}
	key, err := aead.Open(nil, wrapped[:aead.NonceSize()], wrapped[aead.NonceSize():], []byte("archive-key"))
	if err != nil {
		return nil, ErrCorrupt
	}
	return key, nil
}

// readErr is ErrCorrupt for a stream that ended early, and the source's own
// error otherwise: a failed read says nothing about the archive.
func readErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrCorrupt
	}
	return err
}
