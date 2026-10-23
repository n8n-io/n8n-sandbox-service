package runtime

import (
	"strings"
	"testing"
)

const validProxyOptions = `{"egress":"proxy","tenant":"t1","egress_proxy":{
	"allow":[{"name":"git","hosts":["git.example.com","forgejo:3000"]},{"name":"anthropic","hosts":["api.anthropic.com"]}],
	"repos":[{"host":"git.example.com","path":"org/repo","access":"write"},{"host":"forgejo:3000","path":"org/other","access":"read"}],
	"push_refs":["refs/heads/factory/abc/","refs/heads/factory-wip/s1"],
	"credentials":[{"host":"api.anthropic.com","secret_ref":"factory/anthropic-key","scheme":"header","header":"x-api-key"},
		{"host":"forgejo:3000","secret_ref":"factory/git-token","scheme":"basic","username":"factory-agent","allow_plain_http":true},
		{"host":"git.example.com","secret_ref":"factory/git-token"}],
	"model_hosts":[{"host":"api.anthropic.com","provider":"anthropic"}],
	"labels":{"instance_id":"i-1","session_id":"0b6f9c1e-0000-4000-8000-000000000001"}
}}`

func TestParseCreateOptionsEgressProxy(t *testing.T) {
	opts, err := ParseCreateOptions(validProxyOptions)
	if err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	if opts.Egress != EgressProxy || opts.EgressProxy == nil || len(opts.EgressProxy.Repos) != 2 {
		t.Fatalf("parsed = %+v", opts)
	}
	// An omitted scheme is stored as bearer, so it compares equal to one named.
	if got := opts.EgressProxy.Credentials[2].Scheme; got != CredentialBearer {
		t.Fatalf("default scheme = %q, want bearer", got)
	}
	if !opts.BlockEgress() {
		t.Fatal("proxy sandbox must have no direct egress")
	}
}

// Each change turns the valid spec into one that must be refused.
func TestParseCreateOptionsRefusesBadEgressProxy(t *testing.T) {
	for _, tc := range []struct {
		name, old, new string
	}{
		{"unknown field", `"push_refs"`, `"push_ref"`},
		{"field in other case", `"push_refs"`, `"PUSH_REFS"`},
		{"nested field in other case", `"access":"write"`, `"Access":"write"`},
		{"repeated field", `"push_refs":[`, `"push_refs":[],"push_refs":[`},
		{"repeated nested field", `"access":"write"`, `"access":"read","access":"write"`},
		{"repeated label", `"instance_id":"i-1"`, `"instance_id":"i-1","instance_id":"i-2"`},
		{"ip literal", `"git.example.com","forgejo:3000"`, `"git.example.com","10.0.0.1"`},
		{"integer ip", `"git.example.com","forgejo:3000"`, `"git.example.com","167772161"`},
		{"ipv6 literal", `"git.example.com","forgejo:3000"`, `"git.example.com","[::1]"`},
		{"uppercase host", `"api.anthropic.com"]`, `"API.anthropic.com"]`},
		{"bad port", `forgejo:3000"]`, `forgejo:0"]`},
		{"explicit https port", `"api.anthropic.com"]`, `"api.anthropic.com:443"]`},
		{"explicit http port", `"api.anthropic.com"]`, `"api.anthropic.com:80"]`},
		{"hex ip", `"git.example.com","forgejo:3000"`, `"git.example.com","0x7f000001"`},
		{"hex dotted ip", `"git.example.com","forgejo:3000"`, `"git.example.com","0x7f.0.0.1"`},
		{"padded port", `forgejo:3000"]`, `forgejo:03000"]`},
		{"wildcard", `"api.anthropic.com"]`, `"*.anthropic.com"]`},
		{"empty hosts", `"hosts":["api.anthropic.com"]`, `"hosts":[]`},
		{"duplicate name", `"name":"anthropic"`, `"name":"git"`},
		{"repo host not allowed", `"host":"git.example.com","path"`, `"host":"evil.example.com","path"`},
		{"repo path traversal", `"org/repo"`, `"org/../repo"`},
		{"repo path absolute", `"org/repo"`, `"/org/repo"`},
		{"repo access", `"access":"write"`, `"access":"admin"`},
		{"duplicate repo", `"org/other","access":"read"`, `"org/other","access":"read"},{"host":"forgejo:3000","path":"org/other","access":"write"`},
		{"repo path uppercase", `"org/repo"`, `"Org/repo"`},
		{"repo path git suffix", `"org/repo"`, `"org/repo.git"`},
		{"ref outside heads", `"refs/heads/factory-wip/s1"`, `"refs/tags/v1"`},
		{"ref all heads", `"refs/heads/factory-wip/s1"`, `"refs/heads/"`},
		{"ref dotdot", `"refs/heads/factory-wip/s1"`, `"refs/heads/a..b"`},
		{"ref lock", `"refs/heads/factory-wip/s1"`, `"refs/heads/x.lock"`},
		{"ref glob", `"refs/heads/factory-wip/s1"`, `"refs/heads/*"`},
		{"ref space", `"refs/heads/factory-wip/s1"`, `"refs/heads/a b"`},
		{"credential host not allowed", `"host":"api.anthropic.com","secret_ref"`, `"host":"api.openai.com","secret_ref"`},
		{"secret ref traversal", `"factory/anthropic-key"`, `"factory/../root"`},
		{"secret ref empty", `"factory/anthropic-key"`, `""`},
		{"unknown scheme", `"scheme":"header"`, `"scheme":"digest"`},
		{"header scheme without header", `,"header":"x-api-key"`, ``},
		{"header scheme host header", `"header":"x-api-key"`, `"header":"host"`},
		{"header scheme proxy header", `"header":"x-api-key"`, `"header":"proxy-authorization"`},
		{"header scheme content header", `"header":"x-api-key"`, `"header":"content-length"`},
		{"header scheme uppercase", `"header":"x-api-key"`, `"header":"X-Api-Key"`},
		{"basic without username", `,"username":"factory-agent"`, ``},
		{"basic with header", `"username":"factory-agent"`, `"username":"factory-agent","header":"x-a"`},
		{"bearer with username", `"secret_ref":"factory/git-token"}]`, `"secret_ref":"factory/git-token","username":"u"}]`},
		{"duplicate credential host", `{"host":"git.example.com","secret_ref":"factory/git-token"}`, `{"host":"forgejo:3000","secret_ref":"factory/git-token"}`},
		{"model host not allowed", `"host":"api.anthropic.com","provider"`, `"host":"api.openai.com","provider"`},
		{"model host is git host", `"host":"api.anthropic.com","provider"`, `"host":"git.example.com","provider"`},
		{"model provider", `"provider":"anthropic"`, `"provider":"gemini"`},
		{"duplicate model host", `"provider":"anthropic"}]`, `"provider":"anthropic"},{"host":"api.anthropic.com","provider":"openai"}]`},
		{"label key", `"instance_id"`, `"Instance"`},
		{"label value newline", `"i-1"`, `"i-1\nforged"`},
		{"label value empty", `"i-1"`, `""`},
		{"trailing data", `}}`, `}}{}`},
		{"no tenant", `"tenant":"t1",`, ``},
		{"tenant with a slash", `"tenant":"t1"`, `"tenant":"t1/../t2"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(validProxyOptions, tc.old) {
				t.Fatalf("test bug: %q not in the valid spec", tc.old)
			}
			s := strings.Replace(validProxyOptions, tc.old, tc.new, 1)
			if _, err := ParseCreateOptions(s); err == nil {
				t.Fatalf("accepted %s", s)
			}
		})
	}
}

func TestParseCreateOptionsTemplate(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{in: `{}`, want: ""},
		{in: `{"template":"default"}`, want: ""},
		{in: `{"template":"python-3.12"}`, want: "python-3.12"},
		{in: `{"template":"Python"}`, wantErr: true},
		{in: `{"template":"../etc"}`, wantErr: true},
		{in: `{"template":"` + strings.Repeat("a", 64) + `"}`, wantErr: true},
	} {
		opts, err := ParseCreateOptions(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: accepted", tc.in)
			}
			continue
		}
		if err != nil || opts.Template != tc.want {
			t.Errorf("%s: template = %+v err=%v, want %q", tc.in, opts, err, tc.want)
		}
	}
}

// Equal sees through decoding: the echo of what was sent is equal to it, and
// any change in the spec is not.
func TestCreateOptionsEqual(t *testing.T) {
	a, err := ParseCreateOptions(validProxyOptions)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ParseCreateOptions(validProxyOptions)
	if !a.Equal(b) {
		t.Fatal("same options not equal")
	}
	b.EgressProxy.PushRefs = append(b.EgressProxy.PushRefs, "refs/heads/main")
	if a.Equal(b) {
		t.Fatal("different push_refs compare equal")
	}
	if !(&CreateOptions{Egress: EgressPublic}).Equal(&CreateOptions{Egress: EgressPublic}) {
		t.Fatal("plain options not equal")
	}
}
