package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errDockerNotFound marks a 404 from dockerd. Its text matches isDockerNotFound's
// string checks too, so callers need not care which path produced the error.
var errDockerNotFound = errors.New("not found")

const (
	// dockerAPIBaseURL is only what net/http puts on the request line, which
	// accepts nothing but http(s) URLs. The connection itself always goes to
	// the unix:// socket in SANDBOX_RUNNER_DOCKER_HOST: the transport dials it
	// and ignores this host.
	dockerAPIBaseURL = "http://docker"
	// maxDockerAPIResponse bounds how much of a response body is read. The
	// largest response is the container list: ~2.5 KiB per container, so the
	// default runner capacity of 1000 sandboxes is ~2.5 MiB. 16 MiB leaves
	// about 6x headroom.
	maxDockerAPIResponse = 16 << 20
	// maxDockerAPIErrorBody bounds how much of an error body is read for its
	// message, and how much of any body is drained to reuse the connection.
	// dockerd's errors are a one-line {"message": …}; 64 KiB is far above
	// that while keeping a misbehaving peer from making us read much.
	maxDockerAPIErrorBody = 64 << 10
)

// dockerAPI talks to dockerd's REST API over its unix socket. It serves the
// lookups on the request path, where spawning the docker CLI dominated the cost;
// everything else stays on the CLI. The connection is kept alive between calls.
type dockerAPI struct {
	client *http.Client
}

// newDockerAPI returns a client for host, which must be a unix:// endpoint.
func newDockerAPI(host string) (*dockerAPI, error) {
	socket, ok := strings.CutPrefix(host, "unix://")
	if !ok || socket == "" {
		return nil, fmt.Errorf("unsupported docker host %q: only unix:// is supported", host)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &dockerAPI{client: &http.Client{
		Transport: transport,
		// A redirect would send the lookup to a path the caller did not ask for.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// get decodes the JSON response of GET path into out. A failed request reports
// only dockerd's error message, never the body: inspect bodies carry the
// container's environment.
func (a *dockerAPI) get(ctx context.Context, path string, query url.Values, out any) error {
	u := dockerAPIBaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("docker GET %s: %w", path, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker GET %s: %w", path, err)
	}
	defer func() {
		// Drain so the connection goes back to the pool.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDockerAPIErrorBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := dockerAPIErrorMessage(resp)
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("docker GET %s: %s: %w", path, msg, errDockerNotFound)
		}
		return fmt.Errorf("docker GET %s: %s (HTTP %d)", path, msg, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDockerAPIResponse)).Decode(out); err != nil {
		return fmt.Errorf("docker GET %s: decode response: %w", path, err)
	}
	return nil
}

// dockerAPIErrorMessage returns the message field of dockerd's error body, or
// the status text when there is none.
func dockerAPIErrorMessage(resp *http.Response) string {
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDockerAPIErrorBody)).Decode(&body); err == nil {
		if msg := strings.TrimSpace(body.Message); msg != "" {
			return msg
		}
	}
	return http.StatusText(resp.StatusCode)
}

func (a *dockerAPI) inspectContainer(ctx context.Context, containerID string) (*containerInspect, error) {
	if containerID == "" {
		return nil, errors.New("inspect container: empty container ID")
	}
	var inspect containerInspect
	if err := a.get(ctx, "/containers/"+url.PathEscape(containerID)+"/json", nil, &inspect); err != nil {
		return nil, err
	}
	return &inspect, nil
}

// listContainerIDs returns the full IDs of every container, running or not,
// matching filterArgs. Each is a CLI-style filter, "key=value", e.g.
// "label=k=v".
func (a *dockerAPI) listContainerIDs(ctx context.Context, filterArgs ...string) ([]string, error) {
	filters, err := dockerAPIFilters(filterArgs)
	if err != nil {
		return nil, err
	}
	query := url.Values{"all": {"true"}}
	if filters != "" {
		query.Set("filters", filters)
	}
	var containers []struct {
		ID string `json:"Id"`
	}
	if err := a.get(ctx, "/containers/json", query, &containers); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(containers))
	for _, c := range containers {
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// dockerAPIFilters translates CLI-style "key=value" filters into the JSON
// object the REST API takes, e.g. {"label":["k=v"]}.
func dockerAPIFilters(filterArgs []string) (string, error) {
	if len(filterArgs) == 0 {
		return "", nil
	}
	filters := make(map[string][]string)
	for _, f := range filterArgs {
		key, value, ok := strings.Cut(f, "=")
		if !ok || key == "" {
			return "", fmt.Errorf("invalid docker filter %q", f)
		}
		filters[key] = append(filters[key], value)
	}
	b, err := json.Marshal(filters)
	if err != nil {
		return "", fmt.Errorf("encode docker filters: %w", err)
	}
	return string(b), nil
}
