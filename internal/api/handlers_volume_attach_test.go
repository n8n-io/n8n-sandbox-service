package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumes"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// failableReadURLs is a volume store whose read URLs fail while fail is set.
type failableReadURLs struct {
	volumestore.Store
	fail bool
}

func (objects *failableReadURLs) ReadURL(ctx context.Context, key volumestore.Key, ttl time.Duration) (string, error) {
	if objects.fail {
		return "", errors.New("store helper down")
	}
	return objects.Store.ReadURL(ctx, key, ttl)
}

type volumeCreateEnv struct {
	router  http.Handler
	store   store.SandboxStore
	runner  *fakeSandboxControl
	objects *failableReadURLs
	metrics *metrics.APIRecorder
}

// newVolumeCreateEnv serves the API with a local volume store and one runner
// that attaches every volume it is sent.
func newVolumeCreateEnv(t *testing.T) *volumeCreateEnv {
	t.Helper()
	sqlite, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	local, err := volumestore.NewLocal(t.TempDir(), "https://api.test:8443", volumestore.LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	env := &volumeCreateEnv{store: sqlite, objects: &failableReadURLs{Store: local}, metrics: metrics.NewAPIRecorder( /* enabled= */ true)}
	env.runner = &fakeSandboxControl{applied: func(createOptionsJSON string) string {
		options, err := runnerruntime.ParseCreateOptions(createOptionsJSON)
		if err != nil {
			return "invalid: " + err.Error()
		}
		echo, _ := json.Marshal(options.Applied(nil))
		return string(echo)
	}}
	cfg := idleSweepConfig()
	cfg.APIKeys = map[string]struct{}{"admin-key": {}}
	cfg.DefaultMaxSandboxes = 50
	runners := registry.New(45 * time.Second)
	runners.Upsert("runner-1", "https://127.0.0.1:9", startFakeRunnerControl(t, env.runner), true, false, 10, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := volumes.NewService(ctx, sqlite, env.objects, metrics.NewSharedVolumeRecorder(env.metrics))
	env.router, err = NewGatewayRouter(sqlite, cfg, runners, env.metrics, NewActivityRecorder(sqlite, nil), WithSharedVolumes(service))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// publish publishes a shared volume under name and returns its digest.
func (env *volumeCreateEnv) publish(t *testing.T, name string) string {
	t.Helper()
	data := sharedTar(t, name)
	req := httptest.NewRequest(http.MethodPut, sharedPath(data)+"?name="+name, strings.NewReader(string(data)))
	req.Header.Set("X-Api-Key", "admin-key")
	recorder := httptest.NewRecorder()
	env.router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("publish %s = %d %s", name, recorder.Code, recorder.Body.String())
	}
	return "sha256:" + sharedDigest(data)
}

func (env *volumeCreateEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Api-Key", "admin-key")
	recorder := httptest.NewRecorder()
	env.router.ServeHTTP(recorder, req)
	return recorder
}

func TestCreateSandboxAttachesVolumes(t *testing.T) {
	env := newVolumeCreateEnv(t)
	digest := env.publish(t, "tools")

	created := postCreateSandbox(t, env.router, "admin-key",
		`{"volumes":[{"name":"tools","mount_path":"/mnt/tools"},{"name":"missing","mount_path":"/mnt/missing","optional":true}]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	want := `[{"name":"tools","digest":"` + digest + `","scope":"shared","mount_path":"/mnt/tools","optional":false,"attached":true},` +
		`{"name":"missing","mount_path":"/mnt/missing","optional":true,"attached":false,"reason":"not_found"}]`
	var sandbox SandboxResponse
	if err := json.Unmarshal(created.Body.Bytes(), &sandbox); err != nil || string(sandbox.Volumes) != want {
		t.Fatalf("created volumes = %s, %v; want %s", sandbox.Volumes, err, want)
	}
	env.runner.mu.Lock()
	sent, err := runnerruntime.ParseCreateOptions(env.runner.createOptionsJSON[0])
	env.runner.mu.Unlock()
	if err != nil || len(sent.Volumes) != 1 || "sha256:"+sent.Volumes[0].SHA256 != digest || sent.Volumes[0].MountPath != "/mnt/tools" {
		t.Fatalf("runner got %+v, %v; want the tools volume", sent, err)
	}

	var listed []SandboxResponse
	if err := json.Unmarshal(env.get(t, "/sandboxes").Body.Bytes(), &listed); err != nil || len(listed) != 1 || string(listed[0].Volumes) != want {
		t.Errorf("listed = %+v, %v; want the sandbox with its volumes", listed, err)
	}
	var got SandboxResponse
	if err := json.Unmarshal(env.get(t, "/sandboxes/"+sandbox.ID).Body.Bytes(), &got); err != nil || string(got.Volumes) != want {
		t.Errorf("GET volumes = %s, %v; want %s", got.Volumes, err, want)
	}
	if attached, notFound := env.metrics.CreateVolumeCount(metrics.VolumeAttached), env.metrics.CreateVolumeCount(volumes.LeftOutNotFound); attached != 1 || notFound != 1 {
		t.Errorf("volumes counted attached %v, not_found %v; want 1 each", attached, notFound)
	}

	plain := postCreateSandbox(t, env.router, "admin-key", `{}`)
	if plain.Code != http.StatusCreated || strings.Contains(plain.Body.String(), "volumes") {
		t.Errorf("create without volumes = %d %s, want 201 without volumes", plain.Code, plain.Body.String())
	}
}

func TestCreateSandboxVolumeErrors(t *testing.T) {
	env := newVolumeCreateEnv(t)
	env.publish(t, "tools")
	for _, test := range []struct {
		name        string
		body        string
		failReadURL bool
		code        int
		reason      string
	}{
		{"mount path outside /mnt", `{"volumes":[{"name":"tools","mount_path":"/home/user"}]}`, false, http.StatusBadRequest, ""},
		{"unknown field", `{"volumes":[{"name":"tools","mount_path":"/mnt/a","size":1}]}`, false, http.StatusBadRequest, ""},
		{"required volume not found", `{"volumes":[{"name":"missing","mount_path":"/mnt/a"}]}`, false, http.StatusUnprocessableEntity, "volume_not_found"},
		{"no read URL for a required volume", `{"volumes":[{"name":"tools","mount_path":"/mnt/a"}]}`, true, http.StatusServiceUnavailable, "volume_store_unavailable"},
	} {
		env.objects.fail = test.failReadURL
		recorder := postCreateSandbox(t, env.router, "admin-key", test.body)
		if recorder.Code != test.code || reasonOf(t, recorder) != test.reason {
			t.Errorf("%s: create = %d %s, want %d with reason %q", test.name, recorder.Code, recorder.Body.String(), test.code, test.reason)
		}
	}
	env.runner.mu.Lock()
	defer env.runner.mu.Unlock()
	if len(env.runner.createOptionsJSON) != 0 {
		t.Errorf("the runner got %d creates, want none", len(env.runner.createOptionsJSON))
	}
}

func TestCreateSandboxRollsBackWhenTheRunnerIgnoresVolumes(t *testing.T) {
	env := newVolumeCreateEnv(t)
	env.publish(t, "tools")
	env.runner.applied = func(string) string { return `{"egress":"public"}` }

	recorder := postCreateSandbox(t, env.router, "admin-key", `{"volumes":[{"name":"tools","mount_path":"/mnt/tools","optional":true}]}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("create = %d %s, want 502", recorder.Code, recorder.Body.String())
	}
	if _, deleted := env.runner.calls(); len(deleted) != 1 {
		t.Errorf("runner deletes = %v, want the rollback", deleted)
	}
	if listed := env.get(t, "/sandboxes").Body.String(); listed != "[]\n" {
		t.Errorf("sandboxes = %s, want none", listed)
	}
}

func TestCreateSandboxWithAnExistingIDComparesVolumes(t *testing.T) {
	env := newVolumeCreateEnv(t)
	digest := env.publish(t, "tools")
	id := uuid.New().String()
	body := func(volumesJSON string) string { return `{"id":"` + id + `"` + volumesJSON + `}` }

	if recorder := postCreateSandbox(t, env.router, "admin-key", body(`,"volumes":[{"name":"tools","mount_path":"/mnt/tools"}]`)); recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", recorder.Code, recorder.Body.String())
	}
	for _, test := range []struct {
		name, volumesJSON string
		code              int
	}{
		{"the same volumes", `,"volumes":[{"name":"tools","mount_path":"/mnt/tools"}]`, http.StatusOK},
		{"the volume by its digest", `,"volumes":[{"digest":"` + digest + `","mount_path":"/mnt/tools"}]`, http.StatusOK},
		{"no volumes field", ``, http.StatusOK},
		{"another mount path", `,"volumes":[{"name":"tools","mount_path":"/mnt/other"}]`, http.StatusConflict},
		{"no volumes", `,"volumes":[]`, http.StatusConflict},
	} {
		recorder := postCreateSandbox(t, env.router, "admin-key", body(test.volumesJSON))
		if recorder.Code != test.code {
			t.Errorf("%s: create = %d %s, want %d", test.name, recorder.Code, recorder.Body.String(), test.code)
		}
		if recorder.Code == http.StatusConflict && reasonOf(t, recorder) != reasonSettingsMismatch {
			t.Errorf("%s: reason = %q, want %s", test.name, reasonOf(t, recorder), reasonSettingsMismatch)
		}
		if recorder.Code == http.StatusOK && !strings.Contains(recorder.Body.String(), `"mount_path":"/mnt/tools"`) {
			t.Errorf("%s: body %s, want the sandbox's volumes", test.name, recorder.Body.String())
		}
	}
}
