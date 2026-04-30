package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	u, err := url.Parse(run.HTTPBaseURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid runner URL")
		return false
	}
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

			u, err := url.Parse(rec.RunnerHTTPBase)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "invalid stored runner URL")
				return
			}

			proxy := newRunnerReverseProxy(u, cfg.RunnerAPIKey, cfg)
			if limitBody {
				r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
			}
			proxy.ServeHTTP(w, r)
		}
	}
}

func imageProxyHandler(reg *registry.Registry, cfg *config.APIConfig) func(bool) http.HandlerFunc {
	return func(limitBody bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = runnerProxyForPick(w, r, limitBody, cfg, reg.PickRoundRobin)
		}
	}
}

// State-managed handlers that coordinate with runner service

type CreateSandboxRequest struct {
	NetworkPolicy   interface{} `json:"network_policy,omitempty"`
	ResourceLimits  interface{} `json:"resource_limits,omitempty"`
	DockerfileSteps []string    `json:"dockerfile_steps,omitempty"`
}

type SandboxResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Provider     string `json:"provider"`
	ImageID      string `json:"image_id"`
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
				ImageID:      rec.ImageID,
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
			ImageID:      rec.ImageID,
			CreatedAt:    rec.CreatedAt,
			LastActiveAt: time.Now().Unix(),
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleCreateSandbox(s *store.Store, reg *registry.Registry, runnerAPIKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "failed to read request body")
				return
			}
			if len(body) > 0 {
				if err := json.Unmarshal(body, &req); err != nil {
					writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
					return
				}
			}
		}

		run, err := reg.PickRoundRobin()
		if err != nil {
			if errors.Is(err, registry.ErrNoRunners) {
				writeError(w, http.StatusServiceUnavailable, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		runnerURL, err := url.Parse(run.HTTPBaseURL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid runner URL")
			return
		}

		sandboxID := generateUUID()
		now := time.Now().Unix()

		containerInfo, err := callRunnerCreateContainer(runnerURL, runnerAPIKey, sandboxID, &req)
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
			ImageID:        containerInfo.ImageTag,
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
			ImageID:      containerInfo.ImageTag,
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
			runnerURL, err := url.Parse(rec.RunnerHTTPBase)
			if err == nil {
				_ = callRunnerDeleteContainer(runnerURL, runnerAPIKey, id, rec.ContainerIP)
			}
		}

		if err := s.Delete(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func handleGetImage(reg *registry.Registry, cfg *config.APIConfig) http.HandlerFunc {
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
		u, err := url.Parse(run.HTTPBaseURL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid runner URL")
			return
		}
		newRunnerReverseProxy(u, cfg.RunnerAPIKey, cfg).ServeHTTP(w, r)
	}
}

func handleDeleteImage(reg *registry.Registry, cfg *config.APIConfig) http.HandlerFunc {
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
		u, err := url.Parse(run.HTTPBaseURL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid runner URL")
			return
		}
		newRunnerReverseProxy(u, cfg.RunnerAPIKey, cfg).ServeHTTP(w, r)
	}
}

// Helper functions for calling runner service

type ContainerInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IP       string `json:"ip"`
	ImageTag string `json:"image_tag"`
}

func callRunnerCreateContainer(runnerURL *url.URL, apiKey, sandboxID string, req *CreateSandboxRequest) (*ContainerInfo, error) {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	urlStr := fmt.Sprintf("%s/sandboxes", strings.TrimRight(runnerURL.String(), "/"))
	httpReq, err := http.NewRequest("POST", urlStr, bytes.NewReader(reqBody))
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
