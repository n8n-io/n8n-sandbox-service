package volumecache

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// apiServer stands in for the API's volume object listener: TLS from the
// registration material, and no request without a client certificate of the
// registration CA.
type apiServer struct {
	url    string
	data   []byte
	hits   atomic.Int32
	client atomic.Value
}

func serveAPI(t *testing.T, files registrationFiles, data []byte) *apiServer {
	t.Helper()
	tlsConfig, err := grpctls.NewServerTLSConfig(files.serverCert, files.serverKey, files.ca, tls.RequireAndVerifyClientCert)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	api := &apiServer{url: "https://" + listener.Addr().String(), data: data}
	server := &http.Server{
		TLSConfig: tlsConfig,
		// The handshakes these tests have refused would fill the output.
		ErrorLog: log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
			api.hits.Add(1)
			api.client.Store(req.TLS.PeerCertificates[0].Subject.CommonName)
			writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = writer.Write(data)
		}),
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	return api
}

func (a *apiServer) source() Source {
	return Source{URL: a.url + "/volume-objects/a?" + token, Size: int64(len(a.data))}
}

func (a *apiServer) fetch(cache *Cache) (string, error) {
	return cache.Acquire(context.Background(), keyFor(a.data), a.source(), "sandbox-a")
}

// apiConfig is the cache config of a runner that registers with apiHost using
// the client certificate in files.
func apiConfig(t *testing.T, files registrationFiles, apiHost, serverName string) Config {
	t.Helper()
	apiTLS, err := grpctls.NewClientTLSConfig(files.ca, files.clientCert, files.clientKey, serverName)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := testConfig(filepath.Join(root, "cache"), filepath.Join(root, "data"))
	cfg.APIHost, cfg.APITLS = apiHost, apiTLS
	return cfg
}

func openAPICache(t *testing.T, files registrationFiles, apiHost, serverName string) (*Cache, *metrics.RunnerRecorder) {
	t.Helper()
	return openCache(t, apiConfig(t, files, apiHost, serverName))
}

func TestAPIHostIsFetchedWithTheRegistrationIdentity(t *testing.T) {
	for _, tc := range []struct{ name, certHost, serverName string }{
		{"host named by the certificate", "127.0.0.1", ""},
		// As with SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME set.
		{"server name set", "sandbox-api", "sandbox-api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := writeRegistrationFiles(t, newTestCA(t, "registration CA"), tc.certHost)
			api := serveAPI(t, files, randomBytes(t, 64<<10))
			cache, rec := openAPICache(t, files, "127.0.0.1", tc.serverName)

			path, err := api.fetch(cache)
			if err != nil {
				t.Fatalf("Acquire() from the API's host failed: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, api.data) {
				t.Error("cached content differs from the served object")
			}
			if client := api.client.Load(); client != "runner" {
				t.Errorf("the API saw the client certificate of %v, want the runner's", client)
			}
			wantFetches(t, rec, metrics.VolumeFetchDownloaded, 1)
		})
	}
}

// The same listener under any other host name: the runner neither trusts the
// registration CA there nor offers its certificate.
func TestRegistrationIdentityStaysOnTheAPIHost(t *testing.T) {
	ca := newTestCA(t, "registration CA")
	files := writeRegistrationFiles(t, ca, "127.0.0.1")
	api := serveAPI(t, files, randomBytes(t, 4096))

	for name, roots := range map[string]*x509.CertPool{
		// The plain transport as New made it.
		"system CAs": nil,
		// Leaves the missing client certificate as the only reason.
		"registration CA trusted anyway": ca.pool(),
	} {
		t.Run(name, func(t *testing.T) {
			cache, rec := newCache(t, apiConfig(t, files, "sandbox-api.internal", ""))
			if roots != nil {
				cache.trust(roots)
			}

			if _, err := api.fetch(cache); err == nil {
				t.Fatal("Acquire() succeeded off the API's host, want it refused")
			}
			wantFetches(t, rec, metrics.VolumeFetchHTTP, 1)
		})
	}
	if hits := api.hits.Load(); hits != 0 {
		t.Errorf("the API served %d requests, want 0", hits)
	}
}

// On the API's host only the registration CA counts, even for a certificate the
// plain transport accepts.
func TestAPIHostNeedsACertificateOfTheRegistrationCA(t *testing.T) {
	ca := newTestCA(t, "registration CA")
	files := writeRegistrationFiles(t, ca, "127.0.0.1")
	impostor := serve(t, randomBytes(t, 4096), false)
	cache, rec := openAPICache(t, files, "127.0.0.1", "")
	if roots := cache.apiTransport.TLSClientConfig.RootCAs; roots == nil || !roots.Equal(ca.pool()) {
		t.Fatal("the API transport trusts more or less than the registration CA")
	}

	if _, err := impostor.fetch(cache, "sandbox-a"); err == nil {
		t.Fatal("Acquire() succeeded against a certificate of another CA, want it refused")
	}
	wantFetches(t, rec, metrics.VolumeFetchHTTP, 1)
	if hits := impostor.hits.Load(); hits != 0 {
		t.Errorf("the server saw %d requests, want 0", hits)
	}

	// The same server and certificate are fine for a runner with another API.
	elsewhere, _ := openAPICache(t, files, "sandbox-api.internal", "")
	impostor.acquire(t, elsewhere, "sandbox-a")
}

func TestAcquireRefusesPlainHTTP(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = writer.Write([]byte("x"))
	}))
	defer plain.Close()
	host, _, err := net.SplitHostPort(plain.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	files := writeRegistrationFiles(t, newTestCA(t, "registration CA"), host)
	storageOnly, _ := newTestCache(t, 1<<30)
	onAPIHost, _ := openAPICache(t, files, host, "")

	for name, cache := range map[string]*Cache{"another host": storageOnly, "the API's host": onAPIHost} {
		t.Run(name, func(t *testing.T) {
			_, err := cache.Acquire(context.Background(), keyFor([]byte("x")), Source{URL: plain.URL + "/a?" + token, Size: 1}, "sandbox-a")
			if err == nil || !strings.Contains(err.Error(), "not https") {
				t.Fatalf("Acquire() error = %v, want the scheme refused", err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the http server saw %d requests, want 0", got)
	}
}

// A stand-in proxy is set on the plain transport: Go reads the environment's
// once per process and never uses it for loopback, where the test servers
// listen.
func TestAPIHostBypassesTheProxy(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		http.Error(writer, "refused", http.StatusBadGateway)
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	files := writeRegistrationFiles(t, newTestCA(t, "registration CA"), "127.0.0.1")
	api := serveAPI(t, files, randomBytes(t, 4096))
	cache, _ := openAPICache(t, files, "127.0.0.1", "")
	if cache.transport.Proxy == nil {
		t.Fatal("the plain transport has no proxy function, want the environment's")
	}
	if cache.apiTransport.Proxy != nil {
		t.Fatal("the API transport has a proxy function, want none")
	}
	cache.transport.Proxy = http.ProxyURL(proxyURL)

	if _, err := api.fetch(cache); err != nil {
		t.Fatalf("Acquire() from the API's host failed: %v", err)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Errorf("the proxy saw %d requests for the API's host, want 0", got)
	}

	// Without this the check above would prove nothing.
	_, err = cache.Acquire(context.Background(), keyFor([]byte("x")), Source{URL: "https://storage.invalid/a?" + token, Size: 1}, "sandbox-a")
	if err == nil {
		t.Fatal("Acquire() through a proxy that refuses succeeded")
	}
	if got := proxyHits.Load(); got != 1 {
		t.Errorf("the proxy saw %d requests for another host, want 1", got)
	}
}

func TestTransportForMatchesTheAPIHostOnly(t *testing.T) {
	plain, api := &http.Transport{}, &http.Transport{}
	for _, tc := range []struct {
		apiHost string
		source  string
		wantAPI bool
	}{
		{"sandbox-api.internal", "https://sandbox-api.internal/volume-objects/a", true},
		{"sandbox-api.internal", "https://sandbox-api.internal:8443/volume-objects/a", true},
		{"sandbox-api.internal", "https://sandbox-api.internal:9090/volume-objects/a", true},
		{"Sandbox-API.internal", "https://SANDBOX-api.INTERNAL:8443/a", true},
		{"10.0.0.5", "https://10.0.0.5:8443/a", true},
		// As net.SplitHostPort returns an IPv6 host: without brackets.
		{"fd00::5", "https://[fd00::5]:8443/a", true},

		{"sandbox-api.internal", "https://sandbox-api.internal.evil.example/a", false},
		{"sandbox-api.internal", "https://evil-sandbox-api.internal/a", false},
		{"sandbox-api.internal", "https://api.internal/a", false},
		{"sandbox-api.internal", "https://sandbox-api.internal@evil.example/a", false},
		{"sandbox-api.internal", "https://evil.example/sandbox-api.internal", false},
		{"sandbox-api.internal", "https://evil.example/?host=sandbox-api.internal", false},
		{"sandbox-api.internal", "https://evil.example/#@sandbox-api.internal", false},
		{"sandbox-api.internal", "https://sandbox-api.internal./a", false},
		{"10.0.0.5", "https://10.0.0.50/a", false},
		// Unicode case folding would match these: a long s and a Kelvin sign.
		{"sandbox-api.internal", "https://\u017fandbox-api.internal/a", false},
		{"kube-api.internal", "https://\u212aube-api.internal/a", false},
	} {
		parsed, err := parseSource(Source{URL: tc.source, Size: 1})
		if err != nil {
			t.Fatalf("parseSource(%q): %v", tc.source, err)
		}
		cache := &Cache{config: Config{APIHost: tc.apiHost}, transport: plain, apiTransport: api}
		if got := cache.transportFor(parsed) == api; got != tc.wantAPI {
			t.Errorf("API host %q, source %q: API transport = %v, want %v", tc.apiHost, tc.source, got, tc.wantAPI)
		}
	}

	parsed, err := parseSource(Source{URL: "https://sandbox-api.internal/a", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	withoutAPI := &Cache{transport: plain}
	if withoutAPI.transportFor(parsed) != plain {
		t.Error("a cache without an API host did not use the plain transport")
	}
}

func TestNewNeedsTheAPIHostAndItsTLSConfigTogether(t *testing.T) {
	root := t.TempDir()
	for name, change := range map[string]func(*Config){
		"host alone":       func(cfg *Config) { cfg.APIHost = "sandbox-api.internal" },
		"TLS config alone": func(cfg *Config) { cfg.APITLS = &tls.Config{MinVersion: tls.VersionTLS13} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(filepath.Join(root, "cache"), filepath.Join(root, "data"))
			change(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New() accepted half an API identity")
			}
		})
	}
}
