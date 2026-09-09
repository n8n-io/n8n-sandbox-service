package runner

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

const minForwardablePort = 1024

// PortProxyHandler serves /sandboxes/{id}/ports/{port}/{path...}: it forwards the
// request to a process listening on port inside the sandbox, with the route prefix
// stripped. It wakes a stopped sandbox like the daemon routes do.
func PortProxyHandler(rt runnerruntime.Runtime, rec *metrics.RunnerRecorder) http.HandlerFunc {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pt, ok := pr.In.Context().Value(proxyContextKey{}).(*proxyTarget)
			if !ok || pt == nil {
				return
			}
			// SetURL also sets the outbound Host to ip:port; dev servers with a
			// host allowlist accept an IP literal where they would refuse a name.
			pr.SetURL(pt.url)
			pr.Out.URL.Path = pt.path
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// The runner's own credential, set by the API hop; the process in the
			// sandbox is user code and must not see it.
			pr.Out.Header.Del("X-Api-Key")
		},
		// The API acts on these headers (drops the store row, tells the client
		// its sandbox restarted). Only the runner may set them; the process on
		// the port is user code, and its own 404/409 are written before ServeHTTP.
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del(sandboxproxy.SandboxGoneHeader)
			resp.Header.Del(sandboxproxy.SandboxRestartedHeader)
			return nil
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if pt, ok := ctx.Value(proxyContextKey{}).(*proxyTarget); ok && pt != nil && pt.dial != nil {
					return pt.dial(ctx, network, addr)
				}
				return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, addr)
			},
			// Idle connections are pooled by address, and a runtime dialer may reach a
			// different sandbox at the same address (Firecracker guests share one IP),
			// so a pooled connection could serve the wrong sandbox. Upgrades are exempt
			// from the Connection: close this adds, so WebSockets still work.
			DisableKeepAlives: true,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("port proxy: dial failed", "sandbox_id", r.PathValue("id"), "port", r.PathValue("port"), "err", err)
			writeError(w, http.StatusBadGateway, "sandbox port unreachable")
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		port, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
		if err != nil || port < minForwardablePort {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid port: must be %d-65535", minForwardablePort))
			return
		}

		var dial runnerruntime.DialFunc
		baseURL, ok := resolveSandboxURL(w, r, rt, rec, true, func(ctx context.Context, id string) (string, error) {
			addr, d, err := rt.SandboxAddr(ctx, id, int(port))
			dial = d
			return addr, err
		})
		if !ok {
			return
		}
		target, err := url.Parse(baseURL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("invalid sandbox address: %v", err))
			return
		}

		prefix := "/sandboxes/" + r.PathValue("id") + "/ports/" + r.PathValue("port")
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if path == "" {
			path = "/"
		}

		ctx := context.WithValue(r.Context(), proxyContextKey{}, &proxyTarget{url: target, path: path, dial: dial})
		proxy.ServeHTTP(w, r.WithContext(ctx))
	}
}
