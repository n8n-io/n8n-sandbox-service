package runner

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/egressproxy"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/credentials"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/metering"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// Where a proxy sandbox's guest finds the egress CA: the certificate alone,
// and the guest's system bundle with it appended. /tmp because the daemon
// runs as uid 1000 and cannot write the system trust store.
const (
	guestCAPath     = "/tmp/sandbox-egress/ca.pem"
	guestBundlePath = "/tmp/sandbox-egress/ca-bundle.pem"
	guestSystemCAs  = "/etc/ssl/certs/ca-certificates.crt"
)

// LoadEgressProxy builds the egress proxy from SANDBOX_EGRESS_* settings.
func LoadEgressProxy(runnerID string) (*egressproxy.Server, error) {
	var ca *egressproxy.CA
	var err error
	certFile, keyFile := os.Getenv("SANDBOX_EGRESS_PROXY_CA_CERT_FILE"), os.Getenv("SANDBOX_EGRESS_PROXY_CA_KEY_FILE")
	switch {
	case certFile != "" && keyFile != "":
		ca, err = egressproxy.LoadCA(certFile, keyFile)
	case certFile != "" || keyFile != "":
		err = errors.New("SANDBOX_EGRESS_PROXY_CA_CERT_FILE and _KEY_FILE are set together or not at all")
	default:
		ca, err = egressproxy.NewCA()
	}
	if err != nil {
		return nil, fmt.Errorf("egress proxy CA: %w", err)
	}
	cfg := egressproxy.Config{CA: ca, InternalHosts: map[string]bool{}}
	for _, h := range strings.Split(os.Getenv("SANDBOX_EGRESS_PROXY_INTERNAL_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			cfg.InternalHosts[h] = true
		}
	}
	if f := os.Getenv("SANDBOX_EGRESS_PROXY_UPSTREAM_CA_FILE"); f != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		raw, err := os.ReadFile(f)
		if err != nil || !pool.AppendCertsFromPEM(raw) {
			return nil, fmt.Errorf("SANDBOX_EGRESS_PROXY_UPSTREAM_CA_FILE: no certificates read from %s", f)
		}
		cfg.UpstreamRoots = pool
	}
	if dir := os.Getenv("SANDBOX_EGRESS_PROXY_SECRETS_DIR"); dir != "" {
		store, err := credentials.Open(dir)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_EGRESS_PROXY_SECRETS_DIR: %w", err)
		}
		cfg.Secrets = store
	}
	if u := os.Getenv("SANDBOX_EGRESS_USAGE_SINK_URL"); u != "" {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return nil, fmt.Errorf("SANDBOX_EGRESS_USAGE_SINK_URL must be an absolute http(s) URL, got %q", u)
		}
		var token string
		if f := os.Getenv("SANDBOX_EGRESS_USAGE_SINK_TOKEN_FILE"); f != "" {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("SANDBOX_EGRESS_USAGE_SINK_TOKEN_FILE: %w", err)
			}
			token = strings.TrimSpace(string(raw))
		}
		cfg.Sink = metering.NewSink(u, token, runnerID)
	}
	return egressproxy.New(cfg), nil
}

// EgressExecEnv returns the environment of executions in proxy sandboxes: the
// proxy's URL and where the CA is.
func EgressExecEnv(srv *egressproxy.Server, host runnerruntime.EgressProxyHost) func(sandboxID string) map[string]string {
	return func(sandboxID string) map[string]string {
		if !srv.Has(sandboxID) {
			return nil
		}
		u, ok := host.EgressProxyURL(sandboxID)
		if !ok {
			return nil
		}
		return proxyEnv(u)
	}
}

// proxyEnv is what every execution in a proxy sandbox gets, where the request
// does not set it itself.
func proxyEnv(proxyURL string) map[string]string {
	return map[string]string{
		"HTTPS_PROXY": proxyURL, "HTTP_PROXY": proxyURL, "https_proxy": proxyURL, "http_proxy": proxyURL,
		"NO_PROXY": "localhost,127.0.0.1,::1", "no_proxy": "localhost,127.0.0.1,::1",
		"NODE_EXTRA_CA_CERTS": guestCAPath,
		"SSL_CERT_FILE":       guestBundlePath, "REQUESTS_CA_BUNDLE": guestBundlePath,
		"CURL_CA_BUNDLE": guestBundlePath, "GIT_SSL_CAINFO": guestBundlePath,
	}
}

// installGuestCA writes the egress CA into a new proxy sandbox's guest, alone
// and appended to the guest's own bundle so hosts the proxy tunnels still
// verify.
func installGuestCA(ctx context.Context, daemonURL string, caPEM []byte) error {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: runnerruntime.RefuseRedirect}
	system, err := daemonRead(ctx, client, daemonURL, guestSystemCAs)
	if err != nil {
		system = nil // a guest without a bundle still gets the CA
	}
	bundle := append(append(bytes.TrimRight(system, "\n"), '\n'), caPEM...)
	for path, data := range map[string][]byte{guestCAPath: caPEM, guestBundlePath: bundle} {
		if err := daemonWrite(ctx, client, daemonURL, path, data); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func daemonRead(ctx context.Context, client *http.Client, base, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/files/content?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon answered %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func daemonWrite(ctx context.Context, client *http.Client, base, path string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/files?path="+url.QueryEscape(path), bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon answered %d", resp.StatusCode)
	}
	return nil
}
