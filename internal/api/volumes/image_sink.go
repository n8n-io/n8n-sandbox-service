// This file streams an ext4 image from a runner into the volume store, starting
// the store write on the first image byte.

package volumes

import (
	"errors"
	"fmt"
	"io"

	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
)

var errBuildEnded = errors.New("the build ended without its image")

// imageSink stores the image a runner sends. It starts the Put at the first
// image byte, so that a remote store's idle timeout does not run during the
// build. finishPut commits the image only when the build succeeded.
type imageSink struct {
	put        func(image io.Reader) (volumestore.Object, error)
	pipeWriter *io.PipeWriter
	stored     chan putResult
}

type putResult struct {
	object volumestore.Object
	err    error
}

func newImageSink(put func(image io.Reader) (volumestore.Object, error)) *imageSink {
	return &imageSink{put: put}
}

func (sink *imageSink) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if sink.pipeWriter == nil {
		pipeReader, pipeWriter := io.Pipe()
		sink.pipeWriter = pipeWriter
		sink.stored = make(chan putResult, 1)
		go func() {
			object, err := sink.put(pipeReader)
			// Fails the writes of an image the store stopped reading.
			pipeReader.CloseWithError(err)
			sink.stored <- putResult{object: object, err: err}
		}()
	}
	return sink.pipeWriter.Write(data)
}

// finishPut commits the image when buildError is nil, and cancels the Put
// otherwise. A failed Put is a store failure when the build succeeded or
// failed because of the Put. An image over the store's cap is the runner's
// failure.
func (sink *imageSink) finishPut(buildError error) (volumestore.Object, error) {
	if sink.pipeWriter == nil {
		return volumestore.Object{}, buildError
	}
	if buildError != nil {
		sink.pipeWriter.CloseWithError(errBuildEnded)
	} else {
		_ = sink.pipeWriter.Close()
	}
	result := <-sink.stored
	switch {
	case errors.Is(result.err, volumestore.ErrTooLarge):
		return volumestore.Object{}, fmt.Errorf("the runner sent an image over the cap: %w", result.err)
	case result.err != nil && (buildError == nil || errors.Is(buildError, result.err)):
		return volumestore.Object{}, &storeFailure{fmt.Errorf("store the image: %w", result.err)}
	case buildError != nil:
		return volumestore.Object{}, buildError
	}
	return result.object, nil
}

// abortPut cancels a Put that finishPut did not end, so that its goroutine
// stops.
func (sink *imageSink) abortPut() {
	if sink.pipeWriter != nil {
		sink.pipeWriter.CloseWithError(errBuildEnded)
	}
}
