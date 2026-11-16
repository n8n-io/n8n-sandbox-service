package volumestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/volumestore/storepb"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// remoteIdleTimeout fails a call once nothing has moved for this long, so a
// stuck helper cannot hold it.
var remoteIdleTimeout = time.Minute

const (
	putPieceSize         = 1 << 20
	remoteMaxMessageSize = 4 << 20
	// Flow-control windows large enough for multi-GB streams.
	remoteWindowSize = 8 << 20
)

var (
	errStalled       = errors.New("volumestore: call made no progress")
	errHelperEndedIt = errors.New("volumestore: helper ended the upload early")
	errCallDone      = errors.New("volumestore: call done")
)

// Remote keeps objects through a store helper, a process next to the API
// that holds the storage credentials and serves the VolumeStore service in
// proto/volumestore/v1 on a Unix socket.
type Remote struct {
	conn   *grpc.ClientConn
	client storepb.VolumeStoreClient
	idle   time.Duration
}

// NewRemote talks to the helper listening on socket. It does not connect yet.
func NewRemote(socket string) (*Remote, error) {
	// No TLS: only the API and the helper can reach the socket.
	conn, err := grpc.NewClient("unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithNoProxy(),
		grpc.WithInitialWindowSize(remoteWindowSize),
		grpc.WithInitialConnWindowSize(remoteWindowSize),
		grpc.WithMaxHeaderListSize(64<<10),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(remoteMaxMessageSize),
			grpc.MaxCallSendMsgSize(remoteMaxMessageSize),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("volumestore: %w", err)
	}
	return &Remote{conn: conn, client: storepb.NewVolumeStoreClient(conn), idle: remoteIdleTimeout}, nil
}

// Close releases the connection to the helper.
func (rs *Remote) Close() error {
	return rs.conn.Close()
}

// watchdog cancels its call once it has been armed for idle without a kick.
type watchdog struct {
	timer *time.Timer
	idle  time.Duration
}

func (w *watchdog) kick()  { w.timer.Reset(w.idle) }
func (w *watchdog) pause() { w.timer.Stop() }

// watch returns a context for one call, cancelled with errStalled by an armed
// watchdog, and a function that ends both.
func (rs *Remote) watch(ctx context.Context) (context.Context, *watchdog, func()) {
	callCtx, cancel := context.WithCancelCause(ctx)
	dog := &watchdog{idle: rs.idle, timer: time.AfterFunc(rs.idle, func() { cancel(errStalled) })}
	return callCtx, dog, func() {
		dog.timer.Stop()
		cancel(errCallDone)
	}
}

// callError turns a failed call into a Store error. A call ended on this
// side says so rather than blaming the helper.
func (rs *Remote) callError(callCtx context.Context, err error) error {
	switch cause := context.Cause(callCtx); {
	case errors.Is(cause, errStalled):
		return fmt.Errorf("%w for %s", errStalled, rs.idle)
	case cause != nil && !errors.Is(cause, errCallDone):
		return fmt.Errorf("volumestore: %w", cause)
	}
	return statusError(err)
}

func statusError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("volumestore: helper: %w", err)
	}
	switch st.Code() {
	case codes.NotFound:
		return ErrNotFound
	case codes.AlreadyExists:
		return ErrExists
	case codes.OutOfRange:
		return ErrTooLarge
	case codes.DataLoss:
		return ErrDigestMismatch
	case codes.InvalidArgument:
		return fmt.Errorf("%w: helper: %.512s", ErrInvalidKey, st.Message())
	}
	return fmt.Errorf("volumestore: helper: %s: %.512s", st.Code(), st.Message())
}

// definitive reports whether a Put status says the helper stored nothing.
func definitive(err error) bool {
	switch status.Code(err) {
	case codes.AlreadyExists, codes.OutOfRange, codes.DataLoss, codes.InvalidArgument:
		return true
	}
	return false
}

func objectKey(key Key) *storepb.ObjectKey {
	kind := storepb.Kind_KIND_UNSPECIFIED
	switch key.Kind {
	case volume.KindExt4:
		kind = storepb.Kind_KIND_EXT4
	case volume.KindTar:
		kind = storepb.Kind_KIND_TAR
	}
	return &storepb.ObjectKey{Scope: key.Scope, Id: key.ID, Kind: kind}
}

func checkObject(object *storepb.Object) (Object, error) {
	if object == nil || object.GetSize() < 0 || !volume.ValidDigest(object.GetSha256()) {
		return Object{}, errors.New("volumestore: helper sent no valid object size and sha256")
	}
	return Object{Size: object.GetSize(), SHA256: object.GetSha256()}, nil
}

type putAnswer struct {
	object *storepb.Object
	err    error
}

// Put counts and hashes the upload itself and cancels the call when it grows
// past MaxBytes or ends with the wrong digest, so the helper, which stores
// only a stream the client half-closed, never stores it.
func (rs *Remote) Put(ctx context.Context, key Key, content io.Reader, opts PutOptions) (Object, error) {
	want, err := checkPut(key, opts)
	if err != nil {
		return Object{}, err
	}
	callCtx, dog, stop := rs.watch(ctx)
	defer stop()
	stream, err := rs.client.Put(callCtx)
	if err != nil {
		return Object{}, rs.callError(callCtx, err)
	}
	// The answer may come before the client half-closes; only a misbehaving
	// helper sends one then.
	answers := make(chan putAnswer, 1)
	go func() {
		object := new(storepb.Object)
		err := stream.RecvMsg(object)
		answers <- putAnswer{object: object, err: err}
	}()

	sent, err := rs.sendPut(callCtx, dog, stream, key, content, opts, want)
	if err != nil {
		// A stream the helper ended already carries its status, which a
		// cancel could hide. Anything else is cut off by cancelling.
		if !errors.Is(err, errHelperEndedIt) {
			stop()
		}
		answer := <-answers
		switch {
		case answer.err == nil:
			return Object{}, rs.discard(ctx, key, errors.New("volumestore: helper stored the object before the upload ended"))
		case errors.Is(err, errHelperEndedIt):
			return Object{}, rs.callError(callCtx, answer.err)
		}
		return Object{}, err
	}
	select {
	case answer := <-answers:
		if answer.err == nil {
			return Object{}, rs.discard(ctx, key, errors.New("volumestore: helper stored the object before the upload ended"))
		}
		return Object{}, rs.callError(callCtx, answer.err)
	default:
	}
	if err := stream.CloseSend(); err != nil {
		return rs.settle(ctx, key, sent, rs.callError(callCtx, err))
	}
	dog.kick()
	answer := <-answers
	switch {
	case answer.err != nil && definitive(answer.err):
		return Object{}, statusError(answer.err)
	case answer.err != nil:
		return rs.settle(ctx, key, sent, rs.callError(callCtx, answer.err))
	}
	if answer.object.GetSize() != sent.Size || answer.object.GetSha256() != sent.SHA256 {
		return Object{}, rs.discard(ctx, key, fmt.Errorf("volumestore: helper stored %d bytes with sha256 %.64q, not what was sent", answer.object.GetSize(), answer.object.GetSha256()))
	}
	return sent, nil
}

// sendPut sends the header and the content, each read in a message of its own
// of at most putPieceSize, and returns what it sent. It never half-closes.
func (rs *Remote) sendPut(callCtx context.Context, dog *watchdog, stream storepb.VolumeStore_PutClient, key Key, content io.Reader, opts PutOptions, want []byte) (Object, error) {
	send := func(request *storepb.PutRequest) error {
		if err := stream.Send(request); errors.Is(err, io.EOF) {
			return errHelperEndedIt
		} else if err != nil {
			return rs.callError(callCtx, err)
		}
		dog.kick()
		return nil
	}
	err := send(&storepb.PutRequest{Part: &storepb.PutRequest_Header{Header: &storepb.PutHeader{
		Key:      objectKey(key),
		MaxBytes: opts.MaxBytes,
		Sha256:   opts.ExpectedSHA256,
	}}})
	if err != nil {
		return Object{}, err
	}
	source := readUpTo(ctxReader{callCtx, content}, opts.MaxBytes)
	hasher := sha256.New()
	buf := make([]byte, putPieceSize)
	var size int64
	for {
		read, readErr := source.Read(buf)
		size += int64(read)
		if size > opts.MaxBytes {
			return Object{}, ErrTooLarge
		}
		if read > 0 {
			hasher.Write(buf[:read])
			// A fresh slice: gRPC may use a sent message after Send returns.
			if err := send(&storepb.PutRequest{Part: &storepb.PutRequest_Data{Data: bytes.Clone(buf[:read])}}); err != nil {
				return Object{}, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			if errors.Is(context.Cause(callCtx), errStalled) {
				return Object{}, rs.callError(callCtx, readErr)
			}
			return Object{}, readErr
		}
	}
	sum := hasher.Sum(nil)
	if want != nil && !bytes.Equal(sum, want) {
		return Object{}, ErrDigestMismatch
	}
	return Object{Size: size, SHA256: hex.EncodeToString(sum)}, nil
}

// settle decides a Put whose whole upload went out but whose answer says
// nothing about whether it was stored. An object with exactly the sent
// content counts as stored.
func (rs *Remote) settle(ctx context.Context, key Key, sent Object, cause error) (Object, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	got, err := rs.Stat(ctx, key)
	switch {
	case err == nil && got == sent:
		return sent, nil
	case err == nil:
		return Object{}, ErrExists
	case errors.Is(err, ErrNotFound):
		return Object{}, cause
	}
	return Object{}, fmt.Errorf("%w; whether it was stored is unknown: %v", cause, err)
}

// discard deletes an object the helper stored although it must not have.
func (rs *Remote) discard(ctx context.Context, key Key, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := rs.Delete(ctx, key); err != nil {
		return fmt.Errorf("%w; deleting what it stored failed: %v", cause, err)
	}
	return cause
}

func (rs *Remote) Stat(ctx context.Context, key Key) (Object, error) {
	if err := key.Validate(); err != nil {
		return Object{}, err
	}
	callCtx, _, stop := rs.watch(ctx)
	defer stop()
	object, err := rs.client.Stat(callCtx, &storepb.StatRequest{Key: objectKey(key)})
	if err != nil {
		return Object{}, rs.callError(callCtx, err)
	}
	return checkObject(object)
}

func (rs *Remote) Open(ctx context.Context, key Key) (io.ReadCloser, Object, error) {
	if err := key.Validate(); err != nil {
		return nil, Object{}, err
	}
	callCtx, dog, stop := rs.watch(ctx)
	stream, err := rs.client.Get(callCtx, &storepb.GetRequest{Key: objectKey(key)})
	if err != nil {
		stop()
		return nil, Object{}, rs.callError(callCtx, err)
	}
	first, err := stream.Recv()
	if err != nil {
		stop()
		return nil, Object{}, rs.callError(callCtx, err)
	}
	obj, err := checkObject(first.GetObject())
	if err != nil {
		stop()
		return nil, Object{}, err
	}
	// Armed only while waiting for the helper, so a slow reader can pause.
	dog.pause()
	return &objectReader{callCtx: callCtx, rs: rs, stream: stream, dog: dog, stop: stop, remaining: obj.Size}, obj, nil
}

// objectReader streams Get's data messages and holds the helper to the size
// it announced.
type objectReader struct {
	callCtx   context.Context
	rs        *Remote
	stream    storepb.VolumeStore_GetClient
	dog       *watchdog
	stop      func()
	pending   []byte
	remaining int64
}

func (o *objectReader) Read(buf []byte) (int, error) {
	if len(o.pending) == 0 {
		// Armed once per Read, so empty messages cannot keep a stall alive.
		o.dog.kick()
		defer o.dog.pause()
	}
	for len(o.pending) == 0 {
		message, err := o.stream.Recv()
		switch {
		case errors.Is(err, io.EOF) && o.remaining == 0:
			return 0, io.EOF
		case errors.Is(err, io.EOF):
			return 0, io.ErrUnexpectedEOF
		case err != nil:
			return 0, o.rs.callError(o.callCtx, err)
		}
		data, ok := message.GetPart().(*storepb.GetResponse_Data)
		if !ok || int64(len(data.Data)) > o.remaining {
			return 0, errors.New("volumestore: helper sent more than the object or a second header")
		}
		o.remaining -= int64(len(data.Data))
		o.pending = data.Data
	}
	copied := copy(buf, o.pending)
	o.pending = o.pending[copied:]
	return copied, nil
}

func (o *objectReader) Close() error {
	o.stop()
	return nil
}

func (rs *Remote) Delete(ctx context.Context, key Key) error {
	if err := key.Validate(); err != nil {
		return err
	}
	callCtx, _, stop := rs.watch(ctx)
	defer stop()
	_, err := rs.client.Delete(callCtx, &storepb.DeleteRequest{Key: objectKey(key)})
	if err != nil && status.Code(err) != codes.NotFound {
		return rs.callError(callCtx, err)
	}
	return nil
}

func (rs *Remote) ReadURL(ctx context.Context, key Key, ttl time.Duration) (string, error) {
	if err := key.Validate(); err != nil {
		return "", err
	}
	ttl, err := checkTTL(ttl)
	if err != nil {
		return "", err
	}
	callCtx, _, stop := rs.watch(ctx)
	defer stop()
	response, err := rs.client.ReadURL(callCtx, &storepb.ReadURLRequest{Key: objectKey(key), TtlSeconds: ceilSeconds(ttl)})
	if err != nil {
		return "", rs.callError(callCtx, err)
	}
	readURL := response.GetUrl()
	if parsed, err := url.Parse(readURL); err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("volumestore: helper returned a read URL that is not a plain https URL")
	}
	return readURL, nil
}
