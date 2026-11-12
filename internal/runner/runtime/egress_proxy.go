package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Limits on an EgressProxySpec. The create body limit bounds the total; these
// keep any one list small enough for the proxy to check per request.
const (
	maxEgressProxyAllow         = 64
	maxEgressProxyHostsPerEntry = 16
	maxEgressProxyRepos         = 64
	maxEgressProxyPushRefs      = 16
	maxEgressProxyRefPatterns   = 16
	maxEgressProxyCredentials   = 32
	maxEgressProxyModelHosts    = 8
	maxEgressProxyLabels        = 16
	maxRepoPathLen              = 256
	maxRefLen                   = 255
	maxSecretRefLen             = 255
)

// RepoAccess is what a sandbox may do to a Git repository through the proxy.
type RepoAccess string

const (
	// RepoRead allows fetch and clone (git-upload-pack).
	RepoRead RepoAccess = "read"
	// RepoWrite also allows push (git-receive-pack), to PushRefs only.
	RepoWrite RepoAccess = "write"
)

// EgressProxySpec is what an egress "proxy" sandbox may reach, fixed at
// creation. Unknown fields are refused rather than ignored, so a misspelt rule
// cannot silently widen or drop a restriction.
type EgressProxySpec struct {
	// Allow is every destination the proxy lets through, by named entry.
	Allow []EgressAllowEntry `json:"allow"`
	// AllowPublic also lets through any other host whose every address is
	// public, on the scheme's default port, tunnelled as is: no credential,
	// repo filter or metering applies to a host Allow does not name.
	AllowPublic bool `json:"allow_public,omitempty"`
	// Repos are the Git repositories the sandbox may use, on Allow hosts.
	Repos []EgressRepo `json:"repos,omitempty"`
	// PushRefs are the refs a push to a RepoWrite repository may update. An
	// entry ending in "/" is a prefix; any other entry is one exact ref.
	PushRefs []string `json:"push_refs,omitempty"`
	// HideRefs are refs a fetch from Repos must not see: they are left out of
	// every ref listing, cannot be fetched by name, and only listed tips may be
	// fetched by object id. An entry ending in "/" or "." is a prefix; any
	// other entry is one exact ref. Setting it requires Git protocol v2 for
	// fetches.
	HideRefs []string `json:"hide_refs,omitempty"`
	// ShowRefs are exceptions to HideRefs, in the same form.
	ShowRefs []string `json:"show_refs,omitempty"`
	// Credentials are injected by the proxy per destination host. Only the
	// reference is held here; the secret never reaches the runner's guest.
	Credentials []EgressCredential `json:"credentials,omitempty"`
	// ModelHosts are metered and subject to the model block.
	ModelHosts []EgressModelHost `json:"model_hosts,omitempty"`
	// Labels are opaque attribution for usage records and refusal logs.
	Labels map[string]string `json:"labels,omitempty"`
}

// EgressAllowEntry names a group of hosts, e.g. a model provider.
type EgressAllowEntry struct {
	Name  string   `json:"name"`
	Hosts []string `json:"hosts"`
}

// EgressRepo is one Git repository on an allowed host.
type EgressRepo struct {
	Host   string     `json:"host"`
	Path   string     `json:"path"`
	Access RepoAccess `json:"access"`
}

// CredentialScheme is how the proxy presents a secret.
type CredentialScheme string

const (
	// CredentialBearer sends "Authorization: Bearer <secret>". The default.
	CredentialBearer CredentialScheme = "bearer"
	// CredentialBasic sends "Authorization: Basic" with Username and the secret.
	CredentialBasic CredentialScheme = "basic"
	// CredentialHeader sends the secret as the value of Header.
	CredentialHeader CredentialScheme = "header"
)

// EgressCredential names the secret the proxy injects for requests to Host.
type EgressCredential struct {
	Host      string           `json:"host"`
	SecretRef string           `json:"secret_ref"`
	Scheme    CredentialScheme `json:"scheme"`
	Username  string           `json:"username,omitempty"`
	Header    string           `json:"header,omitempty"`
	// AllowPlainHTTP also injects on plain-HTTP requests; otherwise only on
	// TLS the proxy terminates.
	AllowPlainHTTP bool `json:"allow_plain_http,omitempty"`
}

// ModelProvider is a model API format the proxy meters.
type ModelProvider string

const (
	ModelAnthropic ModelProvider = "anthropic"
	ModelOpenAI    ModelProvider = "openai"
)

// EgressModelHost marks an allowed host as a model provider's API.
type EgressModelHost struct {
	Host     string        `json:"host"`
	Provider ModelProvider `json:"provider"`
}

// UnmarshalJSON decodes strictly: unknown fields, field names that differ
// only in case, repeated keys and trailing data are errors. encoding/json
// alone accepts the case variants and lets the last repeated key win, so a
// spec could read one way to a schema validator and another way here.
func (s *EgressProxySpec) UnmarshalJSON(b []byte) error {
	if err := checkSpecKeys(b); err != nil {
		return fmt.Errorf("egress_proxy: %w", err)
	}
	type plain EgressProxySpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("egress_proxy: %w", err)
	}
	if dec.More() {
		return errors.New("egress_proxy: trailing data")
	}
	*s = EgressProxySpec(p)
	return nil
}

// The exact JSON keys of each object in a spec.
var (
	specKeys       = jsonKeys(EgressProxySpec{})
	specListKeys   = map[string]map[string]bool{"allow": jsonKeys(EgressAllowEntry{}), "repos": jsonKeys(EgressRepo{}), "credentials": jsonKeys(EgressCredential{}), "model_hosts": jsonKeys(EgressModelHost{})}
	errNotAnObject = errors.New("not an object")
)

func jsonKeys(v any) map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		keys[name] = true
	}
	return keys
}

// checkSpecKeys checks every object key in a spec against the exact names
// (labels: any name) and refuses repeats. Values are left to the decoder.
func checkSpecKeys(b []byte) error {
	top, err := objectKeys(b, specKeys)
	if err == errNotAnObject {
		return nil // the decoder reports the type error
	}
	if err != nil {
		return err
	}
	for field, keys := range specListKeys {
		var items []json.RawMessage
		if json.Unmarshal(top[field], &items) != nil {
			continue
		}
		for i, item := range items {
			if _, err := objectKeys(item, keys); err != nil && err != errNotAnObject {
				return fmt.Errorf("%s[%d]: %w", field, i, err)
			}
		}
	}
	if labels, ok := top["labels"]; ok {
		if _, err := objectKeys(labels, nil); err != nil && err != errNotAnObject {
			return fmt.Errorf("labels: %w", err)
		}
	}
	return nil
}

// objectKeys reads one JSON object's members, refusing a repeated key and,
// when want is set, any key not in it.
func objectKeys(b []byte, want map[string]bool) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errNotAnObject
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, errNotAnObject
		}
		k, _ := t.(string)
		if want != nil && !want[k] {
			return nil, fmt.Errorf("unknown field %q", k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("repeated field %q", k)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, errNotAnObject
		}
		out[k] = v
	}
	return out, nil
}

var (
	allowNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	hostLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	numericRe   = regexp.MustCompile(`^[0-9]+$`)
	hexLabelRe  = regexp.MustCompile(`^0x[0-9a-f]*$`)
	repoSegRe   = regexp.MustCompile(`^[a-z0-9._-]+$`)
	secretRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	usernameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	headerRe    = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	labelKeyRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	labelValRe  = regexp.MustCompile(`^[A-Za-z0-9._:@/-]{1,128}$`)
	templateRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// ValidateTemplateName checks the form of a template name, not that any
// runner has it.
func ValidateTemplateName(name string) error {
	if !templateRe.MatchString(name) {
		return fmt.Errorf("invalid template %q: lowercase letters, digits, '.', '_' and '-', at most 63", name)
	}
	return nil
}

// Validate checks the spec's form and fills defaults (a credential's Scheme):
// hosts are names, not IP literals, and Repos, Credentials and ModelHosts may
// only name hosts that Allow lets through. A name can still resolve to an
// internal address, and a SecretRef is not scoped to a tenant; neither is
// checked here.
func (s *EgressProxySpec) Validate() error {
	if len(s.Allow) > maxEgressProxyAllow {
		return fmt.Errorf("egress_proxy.allow: at most %d entries", maxEgressProxyAllow)
	}
	hosts := map[string]bool{}
	names := map[string]bool{}
	for i, e := range s.Allow {
		if !allowNameRe.MatchString(e.Name) {
			return fmt.Errorf("egress_proxy.allow[%d].name %q: lowercase letters, digits and '-', at most 63", i, e.Name)
		}
		if names[e.Name] {
			return fmt.Errorf("egress_proxy.allow[%d].name %q: duplicate", i, e.Name)
		}
		names[e.Name] = true
		if len(e.Hosts) == 0 || len(e.Hosts) > maxEgressProxyHostsPerEntry {
			return fmt.Errorf("egress_proxy.allow[%d].hosts: 1 to %d hosts", i, maxEgressProxyHostsPerEntry)
		}
		for _, h := range e.Hosts {
			if err := validateProxyHost(h); err != nil {
				return fmt.Errorf("egress_proxy.allow[%d].hosts: %w", i, err)
			}
			hosts[h] = true
		}
	}

	if len(s.Repos) > maxEgressProxyRepos {
		return fmt.Errorf("egress_proxy.repos: at most %d entries", maxEgressProxyRepos)
	}
	repos := map[string]bool{}
	for i, r := range s.Repos {
		if !hosts[r.Host] {
			return fmt.Errorf("egress_proxy.repos[%d].host %q: not in egress_proxy.allow", i, r.Host)
		}
		if err := validateRepoPath(r.Path); err != nil {
			return fmt.Errorf("egress_proxy.repos[%d].path: %w", i, err)
		}
		if r.Access != RepoRead && r.Access != RepoWrite {
			return fmt.Errorf("egress_proxy.repos[%d].access %q: must be %q or %q", i, r.Access, RepoRead, RepoWrite)
		}
		key := r.Host + "/" + r.Path
		if repos[key] {
			return fmt.Errorf("egress_proxy.repos[%d]: duplicate repository %q", i, key)
		}
		repos[key] = true
	}

	if len(s.PushRefs) > maxEgressProxyPushRefs {
		return fmt.Errorf("egress_proxy.push_refs: at most %d entries", maxEgressProxyPushRefs)
	}
	for i, ref := range s.PushRefs {
		if err := validatePushRef(ref); err != nil {
			return fmt.Errorf("egress_proxy.push_refs[%d]: %w", i, err)
		}
	}

	for _, l := range []struct {
		field string
		refs  []string
	}{{"hide_refs", s.HideRefs}, {"show_refs", s.ShowRefs}} {
		if len(l.refs) > maxEgressProxyRefPatterns {
			return fmt.Errorf("egress_proxy.%s: at most %d entries", l.field, maxEgressProxyRefPatterns)
		}
		for i, ref := range l.refs {
			if err := validateRefPattern(ref); err != nil {
				return fmt.Errorf("egress_proxy.%s[%d]: %w", l.field, i, err)
			}
		}
	}
	if len(s.ShowRefs) > 0 && len(s.HideRefs) == 0 {
		return errors.New("egress_proxy.show_refs: needs hide_refs")
	}

	if len(s.Credentials) > maxEgressProxyCredentials {
		return fmt.Errorf("egress_proxy.credentials: at most %d entries", maxEgressProxyCredentials)
	}
	credHosts := map[string]bool{}
	for i, c := range s.Credentials {
		if !hosts[c.Host] {
			return fmt.Errorf("egress_proxy.credentials[%d].host %q: not in egress_proxy.allow", i, c.Host)
		}
		if credHosts[c.Host] {
			return fmt.Errorf("egress_proxy.credentials[%d].host %q: duplicate", i, c.Host)
		}
		credHosts[c.Host] = true
		if len(c.SecretRef) > maxSecretRefLen || !secretRefRe.MatchString(c.SecretRef) || strings.Contains(c.SecretRef, "..") {
			return fmt.Errorf("egress_proxy.credentials[%d].secret_ref: letters, digits, '.', '_', '/' and '-', no '..', at most %d", i, maxSecretRefLen)
		}
		if err := validateCredentialScheme(&s.Credentials[i]); err != nil {
			return fmt.Errorf("egress_proxy.credentials[%d]: %w", i, err)
		}
	}

	if len(s.ModelHosts) > maxEgressProxyModelHosts {
		return fmt.Errorf("egress_proxy.model_hosts: at most %d entries", maxEgressProxyModelHosts)
	}
	gitHosts := map[string]bool{}
	for _, r := range s.Repos {
		gitHosts[r.Host] = true
	}
	modelHosts := map[string]bool{}
	for i, m := range s.ModelHosts {
		if !hosts[m.Host] {
			return fmt.Errorf("egress_proxy.model_hosts[%d].host %q: not in egress_proxy.allow", i, m.Host)
		}
		// A Git host forwards smart HTTP only, which no model API is.
		if gitHosts[m.Host] {
			return fmt.Errorf("egress_proxy.model_hosts[%d].host %q: also has repos", i, m.Host)
		}
		if modelHosts[m.Host] {
			return fmt.Errorf("egress_proxy.model_hosts[%d].host %q: duplicate", i, m.Host)
		}
		modelHosts[m.Host] = true
		if m.Provider != ModelAnthropic && m.Provider != ModelOpenAI {
			return fmt.Errorf("egress_proxy.model_hosts[%d].provider %q: must be %q or %q", i, m.Provider, ModelAnthropic, ModelOpenAI)
		}
	}

	// Labels end up in logs and usage records: a narrow charset keeps them
	// from forging log lines.
	if len(s.Labels) > maxEgressProxyLabels {
		return fmt.Errorf("egress_proxy.labels: at most %d", maxEgressProxyLabels)
	}
	for k, v := range s.Labels {
		if !labelKeyRe.MatchString(k) {
			return fmt.Errorf("egress_proxy.labels: key %q: lowercase letters, digits, '_', '.' and '-', at most 63", k)
		}
		if !labelValRe.MatchString(v) {
			return fmt.Errorf("egress_proxy.labels[%q]: 1 to 128 of letters, digits and '._:@/-'", k)
		}
	}
	return nil
}

// Headers a credential may not set: the proxy owns framing, routing and
// cookies, and a credential is never a proxy's own.
var forbiddenCredentialHeaders = map[string]bool{
	"host": true, "connection": true, "keep-alive": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "cookie": true,
}

func validateCredentialScheme(c *EgressCredential) error {
	switch c.Scheme {
	case "", CredentialBearer:
		c.Scheme = CredentialBearer
		if c.Username != "" || c.Header != "" {
			return errors.New("bearer takes neither username nor header")
		}
	case CredentialBasic:
		if !usernameRe.MatchString(c.Username) {
			return errors.New("basic needs a username of 1 to 64 letters, digits, '.', '_' and '-'")
		}
		if c.Header != "" {
			return errors.New("basic takes no header")
		}
	case CredentialHeader:
		if c.Username != "" {
			return errors.New("header takes no username")
		}
		if !headerRe.MatchString(c.Header) || forbiddenCredentialHeaders[c.Header] ||
			strings.HasPrefix(c.Header, "proxy-") || strings.HasPrefix(c.Header, "content-") {
			return fmt.Errorf("header %q: a lowercase header name that is not hop-by-hop, host, content-*, cookie or proxy-*", c.Header)
		}
	default:
		return fmt.Errorf("scheme %q: must be %q, %q or %q", c.Scheme, CredentialBearer, CredentialBasic, CredentialHeader)
	}
	return nil
}

// validateProxyHost accepts a lowercase DNS name with an optional :port. The
// default ports (443 for CONNECT, 80 for plain HTTP) are refused when written
// out, so each destination has one spelling and the checks between lists,
// which compare strings, cannot be split across two spellings of one host.
func validateProxyHost(h string) error {
	name, port, hasPort := strings.Cut(h, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return fmt.Errorf("host %q: invalid port", h)
		}
		if n == 80 || n == 443 {
			return fmt.Errorf("host %q: leave out the default port", h)
		}
	}
	if name == "" || len(name) > 253 {
		return fmt.Errorf("host %q: invalid name", h)
	}
	labels := strings.Split(name, ".")
	allNumeric := true
	for _, l := range labels {
		if !hostLabelRe.MatchString(l) {
			return fmt.Errorf("host %q: lowercase DNS name expected", h)
		}
		// Resolvers parse hex labels as parts of an IPv4 address.
		if hexLabelRe.MatchString(l) {
			return fmt.Errorf("host %q: IP addresses are not allowed, name the host", h)
		}
		if !numericRe.MatchString(l) {
			allNumeric = false
		}
	}
	// Covers dotted IPv4 and the integer forms some resolvers accept.
	if allNumeric {
		return fmt.Errorf("host %q: IP addresses are not allowed, name the host", h)
	}
	return nil
}

func validateRepoPath(p string) error {
	if p == "" || len(p) > maxRepoPathLen {
		return fmt.Errorf("%q: 1 to %d characters", p, maxRepoPathLen)
	}
	// One spelling per repository: the Git service matches paths ignoring
	// case and a ".git" suffix, and so does the proxy.
	if strings.HasSuffix(p, ".git") {
		return fmt.Errorf("%q: name the repository without .git", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || !repoSegRe.MatchString(seg) {
			return fmt.Errorf("%q: '/'-separated segments of lowercase letters, digits, '.', '_' and '-'", p)
		}
	}
	return nil
}

// validateRefPattern applies git check-ref-format's rules to a ref under
// refs/, with a trailing "/" or "." allowed to mark a prefix.
func validateRefPattern(ref string) error {
	if len(ref) > maxRefLen || !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("%q: must start with refs/, at most %d characters", ref, maxRefLen)
	}
	name := ref
	if strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		name = ref[:len(ref)-1]
	}
	if strings.Count(name, "/") < 1 || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return fmt.Errorf("%q: invalid ref", ref)
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return fmt.Errorf("%q: invalid ref", ref)
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return fmt.Errorf("%q: invalid ref", ref)
		}
	}
	return nil
}

// validatePushRef applies git check-ref-format's rules to a branch ref, with a
// trailing "/" allowed to mark a prefix.
func validatePushRef(ref string) error {
	if len(ref) > maxRefLen || !strings.HasPrefix(ref, "refs/heads/") {
		return fmt.Errorf("%q: must start with refs/heads/, at most %d characters", ref, maxRefLen)
	}
	name := strings.TrimSuffix(ref, "/")
	if name == "refs/heads" {
		return fmt.Errorf("%q: must name a branch or a prefix below refs/heads/", ref)
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return fmt.Errorf("%q: invalid ref", ref)
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return fmt.Errorf("%q: invalid ref", ref)
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return fmt.Errorf("%q: invalid ref", ref)
		}
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("%q: invalid ref", ref)
	}
	return nil
}
