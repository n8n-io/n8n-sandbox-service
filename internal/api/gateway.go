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

// NewGatewayRouter creates the public API gateway that manages state and
// coordinates with the runner service.
func NewGatewayRouter(s *store.Store, cfg *config.APIConfig) (http.Handler, error) {
	runnerURL, err := url.Parse(cfg.RunnerURL)
	if err != nil {
		return nil, err
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(runnerURL)
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = runnerURL.Host
			if cfg.RunnerAPIKey != "" {
				pr.Out.Header.Set("X-Api-Key", cfg.RunnerAPIKey)
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
			writeError(w, http.StatusBadGateway, "runner unreachable")
		},
	}

	proxyHandler := func(limitBody bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if limitBody {
				r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
			}
			proxy.ServeHTTP(w, r)
		}
	}

	sandboxProxyHandler := func(s *store.Store, proxy *httputil.ReverseProxy, limitBody bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if !isValidUUID(id) {
				writeError(w, http.StatusBadRequest, "invalid sandbox id")
				return
			}

			// Check if sandbox exists
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Sandbox CRUD - managed by API with state
	mux.HandleFunc("GET /sandboxes", handleListSandboxes(s))
	mux.HandleFunc("POST /sandboxes", handleCreateSandbox(s, runnerURL, cfg.RunnerAPIKey))
	mux.HandleFunc("GET /sandboxes/{id}", handleGetSandbox(s))
	mux.HandleFunc("DELETE /sandboxes/{id}", handleDeleteSandbox(s, runnerURL, cfg.RunnerAPIKey))

	// Image CRUD - proxied to runner for now
	mux.HandleFunc("GET /images", proxyHandler(false))
	mux.HandleFunc("GET /images/{id}", handleGetImage(proxy))
	mux.HandleFunc("DELETE /images/{id}", handleDeleteImage(proxy))

	// Sandbox command/file endpoints proxied to runner.
	mux.HandleFunc("POST /sandboxes/{id}/exec", sandboxProxyHandler(s, proxy, false))
	mux.HandleFunc("POST /sandboxes/{id}/files/copy", sandboxProxyHandler(s, proxy, false))
	mux.HandleFunc("POST /sandboxes/{id}/files/move", sandboxProxyHandler(s, proxy, false))
	mux.HandleFunc("GET /sandboxes/{id}/files", sandboxProxyHandler(s, proxy, false))
	mux.HandleFunc("GET /sandboxes/{id}/files/content", sandboxProxyHandler(s, proxy, false))
	mux.HandleFunc("PUT /sandboxes/{id}/files", sandboxProxyHandler(s, proxy, true))
	mux.HandleFunc("POST /sandboxes/{id}/files", sandboxProxyHandler(s, proxy, true))
	mux.HandleFunc("DELETE /sandboxes/{id}/files", sandboxProxyHandler(s, proxy, false))
	mux.HandleFunc("POST /sandboxes/{id}/mkdir", sandboxProxyHandler(s, proxy, false))
	mux.HandleFunc("GET /sandboxes/{id}/stat", sandboxProxyHandler(s, proxy, false))

	var handler http.Handler = mux
	handler = AuthMiddleware(cfg.APIKeys)(handler)
	handler = LoggingMiddleware(handler)
	handler = CORSMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	return handler, nil
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
		// Update last active
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

func handleCreateSandbox(s *store.Store, runnerURL *url.URL, runnerAPIKey string) http.HandlerFunc {
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

		// Generate sandbox ID
		sandboxID := generateUUID()
		now := time.Now().Unix()

		// Call runner to create container
		containerInfo, err := callRunnerCreateContainer(runnerURL, runnerAPIKey, sandboxID, &req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create container: "+err.Error())
			return
		}

		// Store sandbox state in database
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
			// Try to cleanup container on database failure
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

		// Get sandbox record
		rec, err := s.Get(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rec == nil {
			// Delete is idempotent - return 204 even if sandbox doesn't exist
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Call runner to delete container
		if err := callRunnerDeleteContainer(runnerURL, runnerAPIKey, id, rec.ContainerIP); err != nil {
			// Log but continue - container might already be gone
		}

		// Delete from database
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
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid image id")
			return
		}
		proxy.ServeHTTP(w, r)
	}
}

func handleDeleteImage(proxy *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid image id")
			return
		}
		proxy.ServeHTTP(w, r)
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
	// Prepare request body
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Create HTTP request
	url := fmt.Sprintf("%s/sandboxes", runnerURL.String())
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Sandbox-Id", sandboxID)
	if apiKey != "" {
		httpReq.Header.Set("X-Api-Key", apiKey)
	}

	// Make HTTP request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("runner returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var containerInfo ContainerInfo
	if err := json.NewDecoder(resp.Body).Decode(&containerInfo); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &containerInfo, nil
}

func callRunnerDeleteContainer(runnerURL *url.URL, apiKey, sandboxID, containerIP string) error {
	// Create HTTP request
	url := fmt.Sprintf("%s/sandboxes/%s", runnerURL.String(), sandboxID)
	httpReq, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	// Set headers
	if apiKey != "" {
		httpReq.Header.Set("X-Api-Key", apiKey)
	}

	// Make HTTP request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
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
