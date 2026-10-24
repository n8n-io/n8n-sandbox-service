package egressproxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/egressproxy/credentials"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/gitfilter"
	"github.com/n8n-io/sandbox-service/internal/egressproxy/metering"
)

// hopHeaders are never forwarded in either direction.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHop(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// requestHandler serves the requests a guest sends to one destination. The
// policy is read afresh for each, so a replaced spec applies to the next.
func (s *Server) requestHandler(sb *sandbox, t target) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := sb.policy.Load()
		provider := p.models[t.key]

		// The connection's destination decides everything; a request that names
		// another host on it is refused rather than routed there.
		defaultPort := 443
		if t.scheme == "http" {
			defaultPort = 80
		}
		host, port, ok := parseAuthority(r.Host, defaultPort)
		if !ok || destKey(host, port, defaultPort) != t.key || !p.permits(t) {
			s.refuseLogged(w, r, sb, http.StatusForbidden, ReasonHostNotAllowed, provider, r.Host)
			return
		}
		if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" ||
			(r.Header.Get("Expect") != "" && !strings.EqualFold(r.Header.Get("Expect"), "100-continue")) {
			s.refuseLogged(w, r, sb, http.StatusForbidden, ReasonPathNotAllowed, provider, t.key)
			return
		}

		var stripAdvert bool
		if repos, ok := p.repos[t.key]; ok {
			op, _, reason := gitfilter.Check(r.Method, r.URL.EscapedPath(), r.URL.RawQuery, repos)
			if reason != "" {
				s.refuseLogged(w, r, sb, http.StatusForbidden, string(reason), "", t.key)
				return
			}
			switch op {
			case gitfilter.OpPush:
				recoded := r.Header.Get("Content-Encoding") != "" && !strings.EqualFold(r.Header.Get("Content-Encoding"), "identity")
				body, reason := pushBody(r, p.pushRefs)
				if reason != "" {
					s.refuseLogged(w, r, sb, http.StatusForbidden, reason, "", t.key)
					return
				}
				r.Body = body
				// The replayed body is byte for byte the one received unless it
				// was decoded, which leaves its length unknown.
				if recoded {
					r.ContentLength = -1
					r.Header.Del("Content-Length")
					r.Header.Del("Content-Encoding")
				}
			case gitfilter.OpAdvertisePush:
				stripAdvert = true
			}
		}

		var metered bool
		if provider != "" {
			if sb.blocked.Load() {
				s.refuseLogged(w, r, sb, http.StatusForbidden, ReasonModelBlocked, provider, t.key)
				return
			}
			path, ok := canonicalModelPath(r.URL.EscapedPath())
			if !ok || !metering.Allowed(provider, r.Method, path) {
				s.refuseLogged(w, r, sb, http.StatusForbidden, ReasonPathNotAllowed, provider, t.key)
				return
			}
			metered = metering.Metered(provider, r.Method, path)
			if metered && provider == metering.OpenAI && path == "/v1/responses" {
				// A background response answers before any usage exists, so it
				// would spend unmetered.
				body, err := io.ReadAll(io.LimitReader(r.Body, maxMeteredRequestBody+1))
				if err != nil || len(body) > maxMeteredRequestBody || metering.RequestsBackground(body) {
					s.refuseLogged(w, r, sb, http.StatusForbidden, ReasonPathNotAllowed, provider, t.key)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			}
		}

		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.URL = &url.URL{Scheme: t.scheme, Host: t.authority(), Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
		out.Host = r.Host
		stripHop(out.Header)
		if metered {
			// Usage is read from the response, so it must not be compressed.
			out.Header.Del("Accept-Encoding")
		}
		var masks [][]byte
		if c, ok := p.creds[t.key]; ok && (t.scheme == "https" || c.AllowPlainHTTP) {
			secret, err := s.secret(sb, c.SecretRef)
			if err != nil {
				slog.Error("egress credential unavailable", "sandbox_id", sb.id, "tenant_id", sb.tenant, "secret_ref", c.SecretRef, "err", err)
				refuse(w, http.StatusBadGateway, ReasonCredentialUnavailable, provider, "")
				return
			}
			credentials.Inject(out.Header, c, secret)
			// A response that echoes the credential is scrubbed, which needs
			// it uncompressed.
			masks = credentials.Masks(c, secret)
			out.Header.Del("Accept-Encoding")
			// A range of an echo could split the secret past every mask.
			out.Header.Del("Range")
			out.Header.Del("If-Range")
		} else {
			// A credential header the guest made up never reaches the host the
			// proxy holds a credential for.
			out.Header.Del("Proxy-Authorization")
		}

		started := time.Now()
		out = out.WithContext(context.WithValue(out.Context(), dialAddrKey{}, net.JoinHostPort(t.addr.String(), strconv.Itoa(t.port))))
		resp, err := s.transport(t, p.allow[t.key]).RoundTrip(out)
		if err != nil {
			if metered {
				s.record(sb, p, t, provider, r.URL.Path, nil, metering.Usage{}, started)
			}
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		body := io.Reader(resp.Body)
		if len(masks) > 0 {
			if !scrubbable(resp) {
				http.Error(w, "upstream sent a response the proxy cannot scrub", http.StatusBadGateway)
				return
			}
			credentials.ScrubHeader(resp.Header, masks)
			body = credentials.Scrub(body, masks)
		}
		if stripAdvert && resp.StatusCode == http.StatusOK {
			if body, err = gitfilter.StripPushCert(body); err != nil {
				http.Error(w, "upstream answered a malformed advertisement", http.StatusBadGateway)
				return
			}
			resp.Header.Del("Content-Length")
		}
		var meter *metering.Reader
		if metered {
			stream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
			meter = metering.NewReader(body, provider, stream)
			body = meter
		}

		stripHop(resp.Header)
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		copyFlushing(w, body)
		if meter != nil {
			s.record(sb, p, t, provider, r.URL.Path, resp, meter.Usage(), started)
		}
	})
}

// maxMeteredRequestBody bounds a /v1/responses request body, read whole to
// check it is not a background response.
const maxMeteredRequestBody = 16 << 20

const maxTransports = 4096

// copyFlushing streams body to w, flushing after each read, so nothing is
// held back from a streaming client.
func copyFlushing(w http.ResponseWriter, body io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// pushBody checks a receive-pack body's command list and returns a body that
// replays it. A gzip body is decoded to read it; any other coding is refused.
func pushBody(r *http.Request, pushRefs []string) (io.ReadCloser, string) {
	var src io.Reader = r.Body
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, ReasonPathNotAllowed
		}
		src = zr
	default:
		return nil, ReasonPathNotAllowed
	}
	br := bufio.NewReader(src)
	head, err := gitfilter.ReadCommands(br, pushRefs)
	if errors.Is(err, gitfilter.ErrRefused) {
		return nil, ReasonRefNotAllowed
	}
	if err != nil {
		return nil, ReasonPathNotAllowed
	}
	return readCloser{Reader: io.MultiReader(strings.NewReader(string(head)), br), Closer: r.Body}, ""
}

type readCloser struct {
	io.Reader
	io.Closer
}

// canonicalModelPath refuses escapes, empty or dot segments and a trailing
// slash, so the path matched is the path the provider serves.
func canonicalModelPath(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "%") || strings.HasSuffix(p, "/") {
		return "", false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return p, true
}

func (s *Server) secret(sb *sandbox, ref string) ([]byte, error) {
	if s.cfg.Secrets == nil {
		return nil, credentials.ErrUnavailable
	}
	return s.cfg.Secrets.Secret(sb.tenant, ref)
}

// record queues one usage record for the sink.
func (s *Server) record(sb *sandbox, p *policy, t target, provider metering.Provider, path string, resp *http.Response, u metering.Usage, started time.Time) {
	if s.cfg.Sink == nil {
		return
	}
	rec := metering.Record{
		RecordID:                 metering.NewRecordID(),
		SandboxID:                sb.id,
		TenantID:                 sb.tenant,
		Labels:                   p.labels,
		Provider:                 provider,
		Host:                     t.key,
		Path:                     path,
		Model:                    u.Model,
		StartedAt:                started.UTC(),
		EndedAt:                  time.Now().UTC(),
		Complete:                 u.Complete,
		InputTokens:              u.Input,
		OutputTokens:             u.Output,
		CacheReadInputTokens:     u.CacheRead,
		CacheCreationInputTokens: u.CacheCreation,
	}
	if rec.Labels == nil {
		rec.Labels = map[string]string{}
	}
	if resp != nil {
		rec.Status = resp.StatusCode
		rec.Streamed = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
		rec.ProviderRequestID = resp.Header.Get("Request-Id")
		if provider == metering.OpenAI {
			rec.ProviderRequestID = resp.Header.Get("X-Request-Id")
		}
	}
	s.cfg.Sink.Add(rec)
}

// transportKey identifies a pooled upstream: one per destination, whatever
// it resolves to, so the pool is bounded by the allow lists.
type transportKey struct {
	scheme, host string
	port         int
}

// dialAddrKey carries the address a request's destination was checked at.
type dialAddrKey struct{}

// transport returns the upstream for t, pooled when the spec names t's host:
// a host reached only through allow_public gets one of its own, kept out of
// the pool so a guest naming endless hosts cannot churn it for every
// sandbox. It dials only the address the request carries, the one just
// checked, and verifies the certificate against t's host; it never follows a
// redirect, which RoundTrip does not do. A pooled connection was dialled to
// an address checked when it was opened.
func (s *Server) transport(t target, pooled bool) *http.Transport {
	if !pooled {
		tr := s.newTransport(t)
		tr.DisableKeepAlives = true
		return tr
	}
	key := transportKey{t.scheme, t.host, t.port}
	if tr, ok := s.transports.Load(key); ok {
		return tr.(*http.Transport)
	}
	// Bounded across spec changes too: past the cap, drop the pool and start
	// again; open requests keep their connections.
	if s.transportCount.Add(1) > maxTransports {
		s.transports.Range(func(k, v any) bool {
			v.(*http.Transport).CloseIdleConnections()
			s.transports.Delete(k)
			return true
		})
		s.transportCount.Store(1)
	}
	actual, _ := s.transports.LoadOrStore(key, s.newTransport(t))
	return actual.(*http.Transport)
}

func (s *Server) newTransport(t target) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			addr, ok := ctx.Value(dialAddrKey{}).(string)
			if !ok {
				return nil, errors.New("egress proxy: no checked address to dial")
			}
			return s.cfg.Dial(ctx, network, addr)
		},
		TLSClientConfig:   &tls.Config{ServerName: t.host, RootCAs: s.cfg.UpstreamRoots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
		// Encodings are the guest's business, passed through as asked; a
		// metered request asks for none, and the transport must not add one.
		DisableCompression:    true,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		Proxy:                 nil,
	}
}

// scrubbable reports whether a credentialed response can be scrubbed whole:
// not compressed in any listed encoding, and not a partial range.
func scrubbable(resp *http.Response) bool {
	if resp.StatusCode == http.StatusPartialContent {
		return false
	}
	for _, v := range resp.Header.Values("Content-Encoding") {
		for _, enc := range strings.Split(v, ",") {
			if enc = strings.TrimSpace(enc); enc != "" && !strings.EqualFold(enc, "identity") {
				return false
			}
		}
	}
	return true
}
