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
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func newRunnerProxyHandler(cfg *config.APIConfig, proxy *httputil.ReverseProxy, limitBody bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if limitBody {
			r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
		}
		proxy.ServeHTTP(w, r)
	}
}

func newSandboxProxyHandler(s *store.Store, cfg *config.APIConfig, proxy *httputil.ReverseProxy, limitBody bool) http.HandlerFunc {
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

		if limitBody {
			r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
		}
		proxy.ServeHTTP(w, r)
	}
}

// CreateSandboxRequest is the JSON body for POST /sandboxes.
type CreateSandboxRequest struct {
	NetworkPolicy   interface{} `json:"network_policy,omitempty"`
	ResourceLimits  interface{} `json:"resource_limits,omitempty"`
	DockerfileSteps []string    `json:"dockerfile_steps,omitempty"`
}

// SandboxResponse is the JSON shape for sandbox resources.
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
			// Log but don't fail
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

func handleCreateSandbox(s *store.Store, cfg *config.APIConfig, runnerURL *url.URL, runnerAPIKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateSandboxRequest
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					writeError(w, http.StatusBadRequest, "failed to read request body: "+maxBytesErr.Error())
					return
				}
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

		sandboxID := generateUUID()
		now := time.Now().Unix()

		containerInfo, err := callRunnerCreateContainer(runnerURL, runnerAPIKey, sandboxID, &req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create container: "+err.Error())
			return
		}

		record := &store.SandboxRecord{
			ID:           sandboxID,
			Status:       "running",
			CreatedAt:    now,
			LastActiveAt: now,
			ContainerIP:  containerInfo.IP,
			DaemonPort:   8081,
			ImageID:      containerInfo.ImageTag,
		}
		if err := s.Create(record); err != nil {
			if cleanupErr := callRunnerDeleteContainer(runnerURL, runnerAPIKey, sandboxID, containerInfo.IP); cleanupErr != nil {
				// Log but continue with error response
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

func handleDeleteSandbox(s *store.Store, runnerURL *url.URL, runnerAPIKey string) http.HandlerFunc {
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

		if err := callRunnerDeleteContainer(runnerURL, runnerAPIKey, id, rec.ContainerIP); err != nil {
			// Log but continue - container might already be gone
		}

		if err := s.Delete(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func handleGetImage(proxy *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidImageRouteID(id) {
			writeError(w, http.StatusBadRequest, "invalid image id")
			return
		}
		proxy.ServeHTTP(w, r)
	}
}

func handleDeleteImage(proxy *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidImageRouteID(id) {
			writeError(w, http.StatusBadRequest, "invalid image id")
			return
		}
		proxy.ServeHTTP(w, r)
	}
}

// isValidImageRouteID checks the /images/{id} path segment. Image IDs are not
// sandbox UUIDs — they may be logical ids like "img-"+hex, Docker tags, or
// registry references (see API.md). We only reject empty, oversized, or
// traversal-like values; existence is decided by the runner.
func isValidImageRouteID(id string) bool {
	if id == "" || len(id) > 512 {
		return false
	}
	if strings.Contains(id, "..") {
		return false
	}
	return true
}

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

	urlStr := fmt.Sprintf("%s/sandboxes", runnerURL.String())
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
	urlStr := fmt.Sprintf("%s/sandboxes/%s", runnerURL.String(), sandboxID)
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
