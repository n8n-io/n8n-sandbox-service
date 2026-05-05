package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

func runnerProxyForPick(w http.ResponseWriter, r *http.Request, limitBody bool, cfg *config.APIConfig, pick func() (*registry.Runner, error)) bool {
	run, err := pick()
	if err != nil {
		if errors.Is(err, registry.ErrNoRunners) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return false
	}
	u, _ := url.Parse(strings.TrimRight(run.HTTPBaseURL, "/"))
	proxy := newRunnerReverseProxy(u, cfg.RunnerAPIKey, cfg)
	if limitBody {
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
	}
	proxy.ServeHTTP(w, r)
	return true
}

func sandboxProxyHandler(s *store.Store, cfg *config.APIConfig) func(bool) http.HandlerFunc {
	return func(limitBody bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if !isValidUUID(id) {
				writeError(w, http.StatusBadRequest, "invalid sandbox id")
				return
			}

			rec, err := s.Get(id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if rec == nil {
				writeError(w, http.StatusNotFound, "sandbox not found")
				return
			}

			if rec.RunnerHTTPBase == "" {
				writeError(w, http.StatusBadGateway, "sandbox has no runner routing information")
				return
			}

			u, _ := url.Parse(strings.TrimRight(rec.RunnerHTTPBase, "/"))
			proxy := newRunnerReverseProxy(u, cfg.RunnerAPIKey, cfg)
			if limitBody {
				r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
			}
			proxy.ServeHTTP(w, r)
		}
	}
}

// State-managed handlers that coordinate with runner service

type SandboxResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Provider     string `json:"provider"`
	CreatedAt    int64  `json:"created_at"`
	LastActiveAt int64  `json:"last_active_at"`
}

func handleListSandboxes(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		records, err := s.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := make([]*SandboxResponse, len(records))
		for i, rec := range records {
			resp[i] = &SandboxResponse{
				ID:           rec.ID,
				Status:       rec.Status,
				Provider:     "delhi",
				CreatedAt:    rec.CreatedAt,
				LastActiveAt: rec.LastActiveAt,
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleGetSandbox(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}
		rec, err := s.Get(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rec == nil {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if err := s.UpdateLastActive(id); err != nil {
			// non-fatal
		}
		resp := &SandboxResponse{
			ID:           rec.ID,
			Status:       rec.Status,
			Provider:     "delhi",
			CreatedAt:    rec.CreatedAt,
			LastActiveAt: time.Now().Unix(),
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleCreateSandbox(s *store.Store, reg *registry.Registry, runnerAPIKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, err := reg.PickRoundRobin()
		if err != nil {
			if errors.Is(err, registry.ErrNoRunners) {
				writeError(w, http.StatusServiceUnavailable, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		runnerURL, _ := url.Parse(strings.TrimRight(run.HTTPBaseURL, "/"))

		sandboxID := generateUUID()
		now := time.Now().Unix()

		containerInfo, err := callRunnerCreateContainer(runnerURL, runnerAPIKey, sandboxID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create container: "+err.Error())
			return
		}

		record := &store.SandboxRecord{
			ID:             sandboxID,
			Status:         "running",
			CreatedAt:      now,
			LastActiveAt:   now,
			ContainerIP:    containerInfo.IP,
			DaemonPort:     8081,
			RunnerID:       run.ID,
			RunnerHTTPBase: strings.TrimRight(run.HTTPBaseURL, "/"),
		}
		if err := s.Create(record); err != nil {
			if cleanupErr := callRunnerDeleteContainer(runnerURL, runnerAPIKey, sandboxID, containerInfo.IP); cleanupErr != nil {
				// best effort
			}
			writeError(w, http.StatusInternalServerError, "failed to store sandbox: "+err.Error())
			return
		}

		resp := &SandboxResponse{
			ID:           sandboxID,
			Status:       "running",
			Provider:     "delhi",
			CreatedAt:    now,
			LastActiveAt: now,
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

func handleDeleteSandbox(s *store.Store, runnerAPIKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		rec, err := s.Get(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rec == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if rec.RunnerHTTPBase != "" {
			runnerURL, _ := url.Parse(strings.TrimRight(rec.RunnerHTTPBase, "/"))
			_ = callRunnerDeleteContainer(runnerURL, runnerAPIKey, id, rec.ContainerIP)
		}

		if err := s.Delete(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// Helper functions for calling runner service

type ContainerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IP   string `json:"ip"`
}

func callRunnerCreateContainer(runnerURL *url.URL, apiKey, sandboxID string) (*ContainerInfo, error) {
	urlStr := fmt.Sprintf("%s/sandboxes", strings.TrimRight(runnerURL.String(), "/"))
	httpReq, err := http.NewRequest("POST", urlStr, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Sandbox-Id", sandboxID)
	if apiKey != "" {
		httpReq.Header.Set("X-Api-Key", apiKey)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("runner returned status %d: %s", resp.StatusCode, string(body))
	}

	var containerInfo ContainerInfo
	if err := json.NewDecoder(resp.Body).Decode(&containerInfo); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &containerInfo, nil
}

func callRunnerDeleteContainer(runnerURL *url.URL, apiKey, sandboxID, containerIP string) error {
	urlStr := fmt.Sprintf("%s/sandboxes/%s", strings.TrimRight(runnerURL.String(), "/"), sandboxID)
	httpReq, err := http.NewRequest("DELETE", urlStr, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	if apiKey != "" {
		httpReq.Header.Set("X-Api-Key", apiKey)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runner returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func generateUUID() string {
	return uuid.New().String()
}

func isValidUUID(id string) bool {
	return id != "" && uuidRegex.MatchString(id)
}

func newRunnerReverseProxy(runnerURL *url.URL, runnerAPIKey string, cfg *config.APIConfig) *httputil.ReverseProxy {
	target := *runnerURL
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&target)
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = target.Host
			if runnerAPIKey != "" {
				pr.Out.Header.Set("X-Api-Key", runnerAPIKey)
			} else {
				pr.Out.Header.Del("X-Api-Key")
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusBadRequest, "failed to read request body: "+maxBytesErr.Error())
				return
			}
			if strings.Contains(err.Error(), "request body too large") {
				writeError(w, http.StatusBadRequest, "failed to read request body: http: request body too large")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		},
	}
}
