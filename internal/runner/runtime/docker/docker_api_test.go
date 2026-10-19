package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newTestDockerAPI serves handler on a unix socket and returns a client for it.
func newTestDockerAPI(t *testing.T, handler http.HandlerFunc) *dockerAPI {
	t.Helper()
	// t.TempDir can exceed the unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "dapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")

	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	api, err := newDockerAPI("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func TestDockerAPIInspectContainer(t *testing.T) {
	api := newTestDockerAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/containers/abc%2F..%2Fx/json" {
			t.Errorf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{
			"Id": "abc",
			"Name": "/sandbox-abc",
			"State": {"Status": "running", "Running": true},
			"Config": {"Labels": {"sandbox-service.id": "sb1"}},
			"NetworkSettings": {"Networks": {"runner-bridge": {"IPAddress": "172.18.0.5"}}}
		}`))
	})

	inspect, err := api.inspectContainer(context.Background(), "abc/../x")
	if err != nil {
		t.Fatalf("inspectContainer() error = %v", err)
	}
	if inspect.ID != "abc" || !inspect.State.Running || inspect.Config.Labels[containerLabelSandboxID] != "sb1" {
		t.Fatalf("inspectContainer() = %+v", inspect)
	}
	if network, ip := sandboxAttachment(inspect); network != runnerBridgeNetwork || ip != "172.18.0.5" {
		t.Fatalf("sandboxAttachment() = %q, %q", network, ip)
	}
}

func TestDockerAPIListContainerIDsEncodesFilters(t *testing.T) {
	full := strings.Repeat("a", 64)
	api := newTestDockerAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("all"); got != "true" {
			t.Errorf("all = %q", got)
		}
		var filters map[string][]string
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
			t.Errorf("decode filters: %v", err)
		}
		want := map[string][]string{"label": {"sandbox-service.managed=true", "sandbox-service.id=a&b=\"c\""}}
		if !reflect.DeepEqual(filters, want) {
			t.Errorf("filters = %#v, want %#v", filters, want)
		}
		_, _ = w.Write([]byte(`[{"Id":"` + full + `"},{"Id":"` + strings.Repeat("b", 64) + `"}]`))
	})

	ids, err := api.listContainerIDs(context.Background(),
		managedLabelFilter, "label="+containerLabelSandboxID+"=a&b=\"c\"")
	if err != nil {
		t.Fatalf("listContainerIDs() error = %v", err)
	}
	if len(ids) != 2 || ids[0] != full {
		t.Fatalf("listContainerIDs() = %v", ids)
	}
}

func TestDockerAPIListContainerIDsEmpty(t *testing.T) {
	api := newTestDockerAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	ids, err := api.listContainerIDs(context.Background(), managedLabelFilter)
	if err != nil || len(ids) != 0 {
		t.Fatalf("listContainerIDs() = %v, %v", ids, err)
	}
}

func TestDockerAPIFiltersRejectsMalformed(t *testing.T) {
	if _, err := dockerAPIFilters([]string{"label"}); err == nil {
		t.Fatal("dockerAPIFilters() error = nil, want error")
	}
}

func TestDockerAPINotFound(t *testing.T) {
	api := newTestDockerAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: abc"}`))
	})

	_, err := api.inspectContainer(context.Background(), "abc")
	if !errors.Is(err, errDockerNotFound) {
		t.Fatalf("error = %v, want errDockerNotFound", err)
	}
	if !isDockerNotFound(err) || !strings.Contains(err.Error(), "No such container: abc") {
		t.Fatalf("error = %v, want the daemon message", err)
	}
}

func TestDockerAPIErrorReportsOnlyTheMessage(t *testing.T) {
	api := newTestDockerAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"daemon broke","Env":["TOKEN=secret"]}`))
	})

	_, err := api.inspectContainer(context.Background(), "abc")
	if err == nil {
		t.Fatal("error = nil, want error")
	}
	if !strings.Contains(err.Error(), "daemon broke") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %q", err)
	}
	if isDockerNotFound(err) {
		t.Fatalf("isDockerNotFound(%q) = true", err)
	}
}

func TestDockerAPIErrorWithoutMessageUsesStatusText(t *testing.T) {
	api := newTestDockerAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`TOKEN=secret`))
	})

	_, err := api.inspectContainer(context.Background(), "abc")
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "Internal Server Error") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewDockerAPIRejectsNonUnixHosts(t *testing.T) {
	for _, host := range []string{"tcp://127.0.0.1:2375", "ssh://user@host", "unix://", "/var/run/docker.sock", ""} {
		if _, err := newDockerAPI(host); err == nil {
			t.Errorf("newDockerAPI(%q) error = nil, want error", host)
		}
	}
}
