package egressproxy

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/egressproxy/gitfilter"
)

// gitListing is a Git response the proxy rewrites before the guest sees it.
type gitListing int

const (
	listingNone gitListing = iota
	listingPush            // receive-pack ref advertisement
	listingCaps            // upload-pack v2 capability advertisement
	listingRefs            // ls-refs response
)

// checkFetch applies hide_refs to an upload-pack request: protocol v2 only,
// whose listing the proxy filters, and a fetch may want only ids the proxy
// listed to this sandbox. It returns the response to filter, or a refusal.
func (s *Server) checkFetch(r *http.Request, sb *sandbox, p *policy, t target) (gitListing, string) {
	if !gitfilter.ProtocolV2(r.Header.Get("Git-Protocol")) {
		return listingNone, ReasonProtocolNotAllowed
	}
	if r.Method == http.MethodGet {
		return listingCaps, ""
	}
	body, err := readGitRequest(r)
	if err != nil {
		return listingNone, ReasonPathNotAllowed
	}
	req, err := gitfilter.ReadRequest(body)
	if err != nil {
		return listingNone, ReasonPathNotAllowed
	}
	switch req.Command {
	case "":
		return listingNone, ""
	case "ls-refs":
		if gitfilter.CheckLsRefs(req.Args) != nil {
			return listingNone, ReasonPathNotAllowed
		}
		return listingRefs, ""
	case "fetch":
		wants, err := gitfilter.FetchWants(req.Args, p.refs)
		if errors.Is(err, gitfilter.ErrRefused) {
			return listingNone, ReasonRefNotAllowed
		}
		if err != nil {
			return listingNone, ReasonPathNotAllowed
		}
		// A fetch of ids alone lists no refs first, so the proxy lists them.
		if !sb.allListed(wants) {
			if err := s.listRefs(r, sb, p, t, req.Caps); err != nil {
				return listingNone, ReasonRefNotAllowed
			}
		}
		if !sb.allListed(wants) {
			return listingNone, ReasonRefNotAllowed
		}
		return listingNone, ""
	default:
		return listingNone, ReasonPathNotAllowed
	}
}

// readGitRequest reads a whole upload-pack request body, decoding gzip, and
// leaves r to forward it decoded.
func readGitRequest(r *http.Request) ([]byte, error) {
	var src io.Reader = r.Body
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		src = zr
	default:
		return nil, fmt.Errorf("content encoding %q", enc)
	}
	body, err := io.ReadAll(io.LimitReader(src, gitfilter.MaxRequestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > gitfilter.MaxRequestBytes {
		return nil, errors.New("request too large")
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Content-Encoding")
	r.Header.Del("Transfer-Encoding")
	return body, nil
}

// listRefs asks the Git server for the repository's refs, as the guest's
// request would, and records the visible ones' ids as listed.
func (s *Server) listRefs(r *http.Request, sb *sandbox, p *policy, t target, caps []string) error {
	body := gitfilter.LsRefsRequest(caps)
	lr := r.Clone(r.Context())
	lr.Body = io.NopCloser(bytes.NewReader(body))
	lr.ContentLength = int64(len(body))
	lr.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	lr.Header.Set("Accept", "application/x-git-upload-pack-result")
	lr.Header.Del("Content-Encoding")
	out, _, err := s.upstream(sb, p, t, lr, true)
	if err != nil {
		return err
	}
	resp, err := s.transport(t, p.allow[t.key]).RoundTrip(out)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !identityEncoded(resp) {
		return fmt.Errorf("ref listing: status %d", resp.StatusCode)
	}
	var ids []string
	if _, err := gitfilter.FilterLsRefs(resp.Body, p.refs, func(id string) { ids = append(ids, id) }); err != nil {
		return err
	}
	sb.addListed(p, ids)
	return nil
}

// filterListing rewrites a successful listing response for the guest.
func filterListing(kind gitListing, body io.Reader, resp *http.Response, sb *sandbox, p *policy) (io.Reader, error) {
	if !identityEncoded(resp) {
		return nil, errors.New("encoded listing")
	}
	var out []byte
	var err error
	switch kind {
	case listingPush:
		if !p.refs.Active() {
			return gitfilter.StripPushCert(body)
		}
		out, err = gitfilter.FilterPushAdvert(body, p.refs)
	case listingCaps:
		out, err = gitfilter.FilterV2Advert(body)
	case listingRefs:
		var ids []string
		out, err = gitfilter.FilterLsRefs(body, p.refs, func(id string) { ids = append(ids, id) })
		if err == nil {
			sb.addListed(p, ids)
		}
	default:
		return body, nil
	}
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(out), nil
}

func identityEncoded(resp *http.Response) bool {
	for _, v := range resp.Header.Values("Content-Encoding") {
		for _, enc := range strings.Split(v, ",") {
			if enc = strings.TrimSpace(enc); enc != "" && !strings.EqualFold(enc, "identity") {
				return false
			}
		}
	}
	return true
}
