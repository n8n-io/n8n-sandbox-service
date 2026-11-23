package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVolumePublisherMayCall(t *testing.T) {
	for urlPath, want := range map[string]bool{
		"/shared-volumes":                  true,
		"/shared-volumes/sha256:abc":       true,
		"/shared-volumes/":                 false,
		"/shared-volumesX":                 false,
		"/shared-volumes/../sandboxes":     false,
		"/shared-volumes/./sha256:abc":     false,
		"//shared-volumes":                 false,
		"/sandboxes":                       false,
		"/admin/tenants/../shared-volumes": false,
	} {
		if got := volumePublisherMayCall(urlPath); got != want {
			t.Errorf("volumePublisherMayCall(%q) = %v, want %v", urlPath, got, want)
		}
	}
}

func TestVolumePublisherKeyOnlyReachesSharedVolumes(t *testing.T) {
	var reached []authRole
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, _ := authFromContext(r.Context())
		reached = append(reached, identity.Role)
		w.WriteHeader(http.StatusNoContent)
	})
	handler := AuthMiddleware(
		map[string]struct{}{"admin-key": {}}, nil, nil,
		map[string]struct{}{"volume-publisher-key": {}}, nil, newAPIKeyCache(time.Minute),
	)(next)
	serve := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("X-Api-Key", "volume-publisher-key")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	for _, target := range []string{"/shared-volumes", "/shared-volumes/sha256:abc"} {
		if got := serve(http.MethodGet, target); got.Code != http.StatusNoContent {
			t.Errorf("GET %s = %d %s, want it passed on", target, got.Code, got.Body.String())
		}
	}
	if len(reached) != 2 {
		t.Fatalf("the handler was reached %d times, want 2", len(reached))
	}
	for _, role := range reached {
		if role != roleVolumePublisher {
			t.Errorf("the handler saw role %q, want %q", role, roleVolumePublisher)
		}
	}
	for _, request := range []struct{ method, target string }{
		{http.MethodGet, "/sandboxes"},
		{http.MethodPost, "/sandboxes"},
		{http.MethodGet, "/admin/tenants"},
		{http.MethodGet, "/admin/runners"},
		{http.MethodGet, "/shared-volumesX"},
		{http.MethodGet, "/shared-volumes/../sandboxes"},
	} {
		got := serve(request.method, request.target)
		if got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "volume publisher API key may only manage shared volumes") {
			t.Errorf("%s %s = %d %s, want 403", request.method, request.target, got.Code, got.Body.String())
		}
	}
}
