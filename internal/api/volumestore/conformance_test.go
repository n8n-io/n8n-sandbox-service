package volumestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/volumestore/storepb"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

func TestConformanceFakeHelper(t *testing.T) {
	helper, _ := newFakeHelper(t)
	runConformance(t, dialHelper(t, helper.socket))
}

// TestConformanceExternalHelper checks the helper listening on
// SANDBOX_TEST_VOLUME_STORE_SOCKET. It stores and deletes objects with random
// IDs, so the helper should use a throwaway container.
func TestConformanceExternalHelper(t *testing.T) {
	socket := os.Getenv("SANDBOX_TEST_VOLUME_STORE_SOCKET")
	if socket == "" {
		t.Skip("SANDBOX_TEST_VOLUME_STORE_SOCKET is not set; see docs/volume-store-helper.md")
	}
	runConformance(t, dialHelper(t, socket))
}

func dialHelper(t *testing.T, socket string) storepb.VolumeStoreClient {
	t.Helper()
	conn, err := grpc.NewClient("unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithNoProxy(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(remoteMaxMessageSize), grpc.MaxCallSendMsgSize(remoteMaxMessageSize)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return storepb.NewVolumeStoreClient(conn)
}

// runConformance checks a helper against every rule in the proto comments
// that can be checked without access to its storage.
func runConformance(t *testing.T, client storepb.VolumeStoreClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run := randomHex(t, 6)
	key := func(name string, kind storepb.Kind) *storepb.ObjectKey {
		return &storepb.ObjectKey{Scope: volume.SharedScope, Id: "conformance-" + run + "-" + name, Kind: kind}
	}
	content := make([]byte, 5*putPieceSize/2) // Get needs several data messages
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	sum := digest(content)
	cleanup := func(keys ...*storepb.ObjectKey) {
		t.Cleanup(func() {
			for _, objectKey := range keys {
				_, _ = client.Delete(context.Background(), &storepb.DeleteRequest{Key: objectKey})
			}
		})
	}

	t.Run("put, stat, get and delete", func(t *testing.T) {
		for _, objectKey := range []*storepb.ObjectKey{
			key("shared", storepb.Kind_KIND_EXT4),
			{Scope: testTenant, Id: "conformance-" + run + "-tenant", Kind: storepb.Kind_KIND_TAR},
		} {
			cleanup(objectKey)
			object, err := putAll(ctx, client, &storepb.PutHeader{Key: objectKey, MaxBytes: int64(len(content)), Sha256: sum}, content)
			if err != nil || object.GetSize() != int64(len(content)) || object.GetSha256() != sum {
				t.Fatalf("Put = %v, %v; want %d bytes with sha256 %s", object, err, len(content), sum)
			}
			stat, err := client.Stat(ctx, &storepb.StatRequest{Key: objectKey})
			if err != nil || stat.GetSize() != int64(len(content)) || stat.GetSha256() != sum {
				t.Fatalf("Stat = %v, %v", stat, err)
			}
			first, data, largest, err := getAll(ctx, client, objectKey)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if first.GetSize() != int64(len(content)) || first.GetSha256() != sum {
				t.Errorf("Get's first message = %v, want the object's size and sha256", first)
			}
			if !bytes.Equal(data, content) {
				t.Errorf("Get returned %d bytes that differ from what was stored", len(data))
			}
			if largest > putPieceSize {
				t.Errorf("Get sent a data message of %d bytes, more than %d", largest, putPieceSize)
			}
			if _, err := client.Delete(ctx, &storepb.DeleteRequest{Key: objectKey}); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := client.Delete(ctx, &storepb.DeleteRequest{Key: objectKey}); err != nil {
				t.Errorf("Delete of a missing object = %v, want OK", err)
			}
			_, err = client.Stat(ctx, &storepb.StatRequest{Key: objectKey})
			expectCode(t, "Stat after Delete", err, codes.NotFound)
			_, _, _, err = getAll(ctx, client, objectKey)
			expectCode(t, "Get after Delete", err, codes.NotFound)
		}
	})

	t.Run("never replaces", func(t *testing.T) {
		objectKey := key("replace", storepb.Kind_KIND_TAR)
		cleanup(objectKey)
		object, err := putAll(ctx, client, &storepb.PutHeader{Key: objectKey, MaxBytes: 10}, []byte("first"))
		if err != nil || object.GetSize() != 5 || object.GetSha256() != digest([]byte("first")) {
			t.Fatalf("Put without a sha256 = %v, %v; want the size and sha256 it recorded", object, err)
		}
		_, err = putAll(ctx, client, &storepb.PutHeader{Key: objectKey, MaxBytes: 10}, []byte("second"))
		expectCode(t, "second Put", err, codes.AlreadyExists)
		if _, data, _, err := getAll(ctx, client, objectKey); err != nil || string(data) != "first" {
			t.Errorf("after a second Put the object holds %q, %v; want \"first\"", data, err)
		}
	})

	t.Run("concurrent Puts create once", func(t *testing.T) {
		objectKey := key("concurrent", storepb.Kind_KIND_TAR)
		cleanup(objectKey)
		const writers = 4
		payload := func(index int) []byte { return []byte(fmt.Sprintf("writer-%d", index)) }
		streams := make([]storepb.VolumeStore_PutClient, writers)
		for index := range streams {
			stream, err := client.Put(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// A few bytes each, so no send waits for the helper to read.
			if err := sendPut(stream, &storepb.PutHeader{Key: objectKey, MaxBytes: 64}, payload(index)); err != nil && !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			streams[index] = stream
		}
		type answer struct {
			index int
			err   error
		}
		answers := make(chan answer, writers)
		for index, stream := range streams {
			go func() {
				_, err := stream.CloseAndRecv()
				answers <- answer{index: index, err: err}
			}()
		}
		winner := -1
		for range writers {
			got := <-answers
			switch {
			case got.err == nil && winner >= 0:
				t.Errorf("writers %d and %d both answered OK", winner, got.index)
			case got.err == nil:
				winner = got.index
			default:
				expectCode(t, "concurrent Put", got.err, codes.AlreadyExists)
			}
		}
		if winner < 0 {
			t.Fatal("no concurrent Put answered OK")
		}
		if _, data, _, err := getAll(ctx, client, objectKey); err != nil || !bytes.Equal(data, payload(winner)) {
			t.Errorf("stored %q, %v; want %q from the Put that answered OK", data, err, payload(winner))
		}
	})

	t.Run("refused uploads store nothing", func(t *testing.T) {
		cases := []struct {
			name   string
			header *storepb.PutHeader
			want   codes.Code
		}{
			{"more than max_bytes", &storepb.PutHeader{Key: key("large", storepb.Kind_KIND_TAR), MaxBytes: int64(len(content)) - 1}, codes.OutOfRange},
			{"not the given sha256", &storepb.PutHeader{Key: key("digest", storepb.Kind_KIND_TAR), MaxBytes: int64(len(content)), Sha256: digest([]byte("other"))}, codes.DataLoss},
		}
		for _, testCase := range cases {
			cleanup(testCase.header.GetKey())
			_, err := putAll(ctx, client, testCase.header, content)
			expectCode(t, testCase.name, err, testCase.want)
			_, err = client.Stat(ctx, &storepb.StatRequest{Key: testCase.header.GetKey()})
			expectCode(t, testCase.name+": Stat", err, codes.NotFound)
		}
	})

	t.Run("invalid keys and headers", func(t *testing.T) {
		valid := key("valid", storepb.Kind_KIND_TAR)
		cleanup(valid)
		badKeys := map[string]*storepb.ObjectKey{
			"no key":           nil,
			"empty scope":      {Scope: "", Id: valid.GetId(), Kind: valid.GetKind()},
			"uppercase scope":  {Scope: "SHARED", Id: valid.GetId(), Kind: valid.GetKind()},
			"uppercase tenant": {Scope: strings.ToUpper(testTenant), Id: valid.GetId(), Kind: valid.GetKind()},
			"traversal scope":  {Scope: "../shared", Id: valid.GetId(), Kind: valid.GetKind()},
			"empty id":         {Scope: volume.SharedScope, Id: "", Kind: valid.GetKind()},
			"slash in id":      {Scope: volume.SharedScope, Id: "a/b", Kind: valid.GetKind()},
			"underscore in id": {Scope: volume.SharedScope, Id: "a_b", Kind: valid.GetKind()},
			"dot in id":        {Scope: volume.SharedScope, Id: "a.b", Kind: valid.GetKind()},
			"dots id":          {Scope: volume.SharedScope, Id: "..", Kind: valid.GetKind()},
			"uppercase id":     {Scope: volume.SharedScope, Id: "A", Kind: valid.GetKind()},
			"65 character id":  {Scope: volume.SharedScope, Id: strings.Repeat("a", 65), Kind: valid.GetKind()},
			"unspecified kind": {Scope: volume.SharedScope, Id: valid.GetId(), Kind: storepb.Kind_KIND_UNSPECIFIED},
			"unknown kind":     {Scope: volume.SharedScope, Id: valid.GetId(), Kind: storepb.Kind(7)},
		}
		for name, badKey := range badKeys {
			_, err := putAll(ctx, client, &storepb.PutHeader{Key: badKey, MaxBytes: 10}, []byte("x"))
			expectCode(t, "Put, "+name, err, codes.InvalidArgument)
			_, err = client.Stat(ctx, &storepb.StatRequest{Key: badKey})
			expectCode(t, "Stat, "+name, err, codes.InvalidArgument)
			_, _, _, err = getAll(ctx, client, badKey)
			expectCode(t, "Get, "+name, err, codes.InvalidArgument)
			_, err = client.Delete(ctx, &storepb.DeleteRequest{Key: badKey})
			expectCode(t, "Delete, "+name, err, codes.InvalidArgument)
			_, err = client.ReadURL(ctx, &storepb.ReadURLRequest{Key: badKey, TtlSeconds: 60})
			expectCode(t, "ReadURL, "+name, err, codes.InvalidArgument)
		}
		for name, header := range map[string]*storepb.PutHeader{
			"zero max_bytes":     {Key: valid, MaxBytes: 0},
			"negative max_bytes": {Key: valid, MaxBytes: -1},
			"uppercase sha256":   {Key: valid, MaxBytes: 10, Sha256: strings.ToUpper(digest([]byte("x")))},
			"short sha256":       {Key: valid, MaxBytes: 10, Sha256: "abc"},
		} {
			_, err := putAll(ctx, client, header, []byte("x"))
			expectCode(t, "Put, "+name, err, codes.InvalidArgument)
		}
		_, err := putMessages(ctx, client, &storepb.PutRequest{Part: &storepb.PutRequest_Data{Data: []byte("x")}})
		expectCode(t, "Put, data before the header", err, codes.InvalidArgument)
		_, err = client.Stat(ctx, &storepb.StatRequest{Key: valid})
		expectCode(t, "Stat after refused Puts", err, codes.NotFound)
	})

	t.Run("a cancelled upload stores nothing", func(t *testing.T) {
		objectKey := key("cancelled", storepb.Kind_KIND_TAR)
		cleanup(objectKey)
		callCtx, cancelCall := context.WithCancel(ctx)
		stream, err := client.Put(callCtx)
		if err != nil {
			t.Fatal(err)
		}
		if err := sendPut(stream, &storepb.PutHeader{Key: objectKey, MaxBytes: int64(len(content))}, content[:putPieceSize]); err != nil {
			t.Fatal(err)
		}
		cancelCall()
		// Never half-close a cancelled stream: the half-close could reach the
		// helper before the cancel.
		if err := stream.RecvMsg(new(storepb.Object)); err == nil {
			t.Fatal("a cancelled Put answered OK")
		}
		time.Sleep(200 * time.Millisecond)
		_, err = client.Stat(ctx, &storepb.StatRequest{Key: objectKey})
		expectCode(t, "Stat after a cancelled Put", err, codes.NotFound)
	})

	t.Run("stores and answers only after the half-close", func(t *testing.T) {
		objectKey := key("open", storepb.Kind_KIND_TAR)
		cleanup(objectKey)
		stream, err := client.Put(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := sendPut(stream, &storepb.PutHeader{Key: objectKey, MaxBytes: int64(len(content)), Sha256: sum}, content); err != nil {
			t.Fatal(err)
		}
		answers := make(chan error, 1)
		object := new(storepb.Object)
		go func() { answers <- stream.RecvMsg(object) }()
		select {
		case err := <-answers:
			t.Fatalf("answered before the half-close: %v, %v", object, err)
		case <-time.After(300 * time.Millisecond):
		}
		_, err = client.Stat(ctx, &storepb.StatRequest{Key: objectKey})
		expectCode(t, "Stat before the half-close", err, codes.NotFound)
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if err := <-answers; err != nil || object.GetSize() != int64(len(content)) || object.GetSha256() != sum {
			t.Fatalf("answer after the half-close = %v, %v", object, err)
		}
	})

	t.Run("read URL", func(t *testing.T) {
		objectKey := key("never-stored", storepb.Kind_KIND_EXT4)
		for _, ttl := range []int64{1, 3600} {
			response, err := client.ReadURL(ctx, &storepb.ReadURLRequest{Key: objectKey, TtlSeconds: ttl})
			if err != nil {
				t.Fatalf("ReadURL with ttl %d for an object that need not exist: %v", ttl, err)
			}
			parsed, err := url.Parse(response.GetUrl())
			if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
				t.Errorf("ReadURL returned %q, want a plain https URL", response.GetUrl())
			}
		}
		for _, ttl := range []int64{0, -1, 3601} {
			_, err := client.ReadURL(ctx, &storepb.ReadURLRequest{Key: objectKey, TtlSeconds: ttl})
			if err == nil {
				t.Errorf("ReadURL with ttl %d succeeded", ttl)
			}
			expectNoURL(t, "ReadURL", err)
		}
	})
}

func randomHex(t *testing.T, size int) string {
	t.Helper()
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

// sendPut sends a header and data in pieces of putPieceSize, without
// half-closing.
func sendPut(stream storepb.VolumeStore_PutClient, header *storepb.PutHeader, data []byte) error {
	if err := stream.Send(&storepb.PutRequest{Part: &storepb.PutRequest_Header{Header: header}}); err != nil {
		return err
	}
	for len(data) > 0 {
		piece := data[:min(len(data), putPieceSize)]
		data = data[len(piece):]
		if err := stream.Send(&storepb.PutRequest{Part: &storepb.PutRequest_Data{Data: piece}}); err != nil {
			return err
		}
	}
	return nil
}

// putAll uploads data completely and returns the helper's answer.
func putAll(ctx context.Context, client storepb.VolumeStoreClient, header *storepb.PutHeader, data []byte) (*storepb.Object, error) {
	stream, err := client.Put(ctx)
	if err != nil {
		return nil, err
	}
	// io.EOF means the helper already answered; CloseAndRecv returns it.
	if err := sendPut(stream, header, data); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return stream.CloseAndRecv()
}

func putMessages(ctx context.Context, client storepb.VolumeStoreClient, messages ...*storepb.PutRequest) (*storepb.Object, error) {
	stream, err := client.Put(ctx)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if err := stream.Send(message); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

// getAll reads an object and returns Get's first message, the data, and the
// largest data message.
func getAll(ctx context.Context, client storepb.VolumeStoreClient, key *storepb.ObjectKey) (*storepb.Object, []byte, int, error) {
	stream, err := client.Get(ctx, &storepb.GetRequest{Key: key})
	if err != nil {
		return nil, nil, 0, err
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, nil, 0, err
	}
	header := first.GetObject()
	if header == nil {
		return nil, nil, 0, errors.New("Get's first message carries no object")
	}
	var data []byte
	largest := 0
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return header, data, largest, nil
		}
		if err != nil {
			return nil, nil, 0, err
		}
		piece, ok := message.GetPart().(*storepb.GetResponse_Data)
		if !ok {
			return nil, nil, 0, errors.New("Get sent a second object message")
		}
		largest = max(largest, len(piece.Data))
		data = append(data, piece.Data...)
	}
}

func expectCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("%s: %v, want %s", what, err, want)
	}
	expectNoURL(t, what, err)
}

// expectNoURL holds the helper to status messages without URLs, which the
// API logs.
func expectNoURL(t *testing.T, what string, err error) {
	t.Helper()
	if message := status.Convert(err).Message(); strings.Contains(message, "://") {
		t.Errorf("%s: status message %q contains a URL", what, message)
	}
}
