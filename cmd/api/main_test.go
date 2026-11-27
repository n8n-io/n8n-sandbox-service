package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// captureLogs sends the default logger's JSON lines to the returned buffer
// until the test ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	defaultLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(defaultLogger) })
	return &logs
}

func localVolumeConfig(t *testing.T) *config.APIConfig {
	t.Helper()
	return &config.APIConfig{
		VolumeStore:           config.VolumeStoreLocal,
		VolumeLocalDir:        filepath.Join(t.TempDir(), "volumes"),
		VolumeLocalURL:        "https://api.test:8443",
		VolumeLocalListenAddr: ":8443",
	}
}

// The store signs with the configured key, so another API instance with that
// key verifies its URLs, and the startup log says a key is set but never
// shows it.
func TestNewVolumeStoreUsesTheSigningKeyAndLogsNone(t *testing.T) {
	key := bytes.Repeat([]byte{0x7e}, volumestore.MinSigningKeyBytes)
	previous := bytes.Repeat([]byte{0x3c}, volumestore.MinSigningKeyBytes)
	logs := captureLogs(t)
	cfg := localVolumeConfig(t)
	cfg.VolumeLocalSigningKey = key
	cfg.VolumeLocalPreviousSigningKey = previous

	volumes, err := newVolumeStore(cfg)
	if err != nil {
		t.Fatalf("newVolumeStore: %v", err)
	}
	objectKey := volumestore.Key{Scope: volume.SharedScope, ID: "python", Kind: volume.KindTar}
	readURL, err := volumes.ReadURL(context.Background(), objectKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(readURL)
	if err != nil {
		t.Fatal(err)
	}
	other, err := volumestore.NewLocal(filepath.Join(t.TempDir(), "volumes"), cfg.VolumeLocalURL, volumestore.LocalOptions{SigningKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := other.VerifyRequest(httptest.NewRequest("GET", parsed.RequestURI(), nil)); err != nil || got != objectKey {
		t.Errorf("another instance with the key: %+v, %v; want %+v", got, err, objectKey)
	}

	logged := logs.String()
	for _, want := range []string{`"signing_key":"configured"`, `"previous_signing_key":true`, cfg.VolumeLocalDir} {
		if !strings.Contains(logged, want) {
			t.Errorf("the startup log lacks %s: %s", want, logged)
		}
	}
	for _, secret := range [][]byte{key, previous} {
		for _, form := range []string{
			string(secret),
			hex.EncodeToString(secret),
			base64.StdEncoding.EncodeToString(secret),
			base64.RawStdEncoding.EncodeToString(secret),
			strings.Trim(fmt.Sprint(secret), "[]"), //nolint:staticcheck // QF1010: The decimal byte form is the point.
		} {
			if strings.Contains(logged, form) {
				t.Fatalf("the startup log shows a signing key: %s", logged)
			}
		}
	}
}

func TestNewVolumeStoreLogsARandomKey(t *testing.T) {
	logs := captureLogs(t)
	if _, err := newVolumeStore(localVolumeConfig(t)); err != nil {
		t.Fatalf("newVolumeStore: %v", err)
	}
	for _, want := range []string{`"signing_key":"random per process"`, `"previous_signing_key":false`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the startup log lacks %s: %s", want, logs.String())
		}
	}
}
