package volumes

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

var (
	errValidated = errors.New("validation finished")
	errAborted   = errors.New("upload ended early")
)

// tarGate hands an upload to the volume store while a validator reads the
// same bytes, and ends it with io.EOF only if the tar was valid. Anything else
// ends it with an error, before the store's Put sees the end, so the store
// never commits an invalid tar.
type tarGate struct {
	body       io.Reader
	pipeWriter *io.PipeWriter
	results    chan validation
	result     *validation
	// bodyEnded is set once body returned io.EOF; from then on a refused tar
	// leaves body alone.
	bodyEnded atomic.Bool
	// failure is the error Read returned once, and returns from then on.
	failure  error
	finished bool
}

type validation struct {
	stats safetar.Stats
	err   error
}

// newTarGate reads body. interrupt, if set, ends a read of body that waits
// for the client: the gate calls it when the validator refuses the tar before
// body has ended.
func newTarGate(body io.Reader, policy safetar.Policy, interrupt func()) *tarGate {
	pipeReader, pipeWriter := io.Pipe()
	gate := &tarGate{body: body, pipeWriter: pipeWriter, results: make(chan validation, 1)}
	go func() {
		stats, err := safetar.Validate(pipeReader, policy)
		if err == nil && stats.Entries == stats.GlobalHeaders {
			err = errTarEmpty
		}
		if err != nil && !errors.Is(err, errAborted) && interrupt != nil && !gate.bodyEnded.Load() {
			interrupt()
		}
		// Ends a write waiting on a validator that stopped early.
		pipeReader.CloseWithError(errValidated)
		gate.results <- validation{stats: stats, err: err}
	}()
	return gate
}

func (gate *tarGate) Read(buf []byte) (int, error) {
	if gate.failure != nil {
		return 0, gate.failure
	}
	if gate.finished {
		return 0, io.EOF
	}
	read, err := gate.body.Read(buf)
	if errors.Is(err, io.EOF) {
		gate.bodyEnded.Store(true)
	}
	if read > 0 {
		if _, writeErr := gate.pipeWriter.Write(buf[:read]); writeErr != nil {
			// The validator stopped before the end, which only a verdict does.
			failed := verdict(gate.wait())
			if failed == nil {
				failed = errTarRules
			}
			return 0, gate.fail(failed)
		}
	}
	switch {
	case errors.Is(err, io.EOF):
		_ = gate.pipeWriter.Close()
		if failed := verdict(gate.wait()); failed != nil {
			return 0, gate.fail(failed)
		}
		gate.finished = true
		return read, io.EOF
	case err != nil:
		gate.pipeWriter.CloseWithError(errAborted)
		// A validator that refused the tar first is what interrupted the read.
		if result := gate.wait(); result.err != nil && !errors.Is(result.err, errAborted) {
			return 0, gate.fail(verdict(result))
		}
		return 0, gate.fail(fmt.Errorf("%w: %w", ErrUpload, err))
	}
	return read, nil
}

func (gate *tarGate) fail(err error) error {
	gate.failure = err
	return err
}

func (gate *tarGate) wait() validation {
	if gate.result == nil {
		result := <-gate.results
		gate.result = &result
	}
	return *gate.result
}

// close ends the validator if the store stopped reading early.
func (gate *tarGate) close() {
	gate.pipeWriter.CloseWithError(errAborted)
	gate.wait()
}

// err returns what ended the upload, nil if it ended well or never failed.
func (gate *tarGate) err() error {
	return gate.failure
}

// counts returns what the validator counted; only after a valid tar.
func (gate *tarGate) counts() volume.TarCounts {
	stats := gate.wait().stats
	return volume.TarCounts{
		Entries:           int64(stats.Entries),
		ContentBytes:      stats.Bytes,
		DecompressedBytes: stats.DecompressedBytes,
	}
}

// verdict turns the validator's result into an error for a caller. It never
// passes on the validator's own error, which can name an entry of the tar.
func verdict(result validation) error {
	switch {
	case result.err == nil:
		return nil
	case errors.Is(result.err, errTarEmpty):
		return errTarEmpty
	case errors.Is(result.err, safetar.ErrTooLarge):
		return errTarTooLarge
	}
	return errTarRules
}
