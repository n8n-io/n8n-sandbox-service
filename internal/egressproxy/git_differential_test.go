package egressproxy

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitEnv is an environment where git uses nothing from the host's config.
func gitEnv(home string, extra ...string) []string {
	return append([]string{
		"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_TERMINAL_PROMPT=0",
	}, extra...)
}

func runGit(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, err := runGit(t, dir, env, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// A real git client against a real git http-backend, through the proxy:
// fetch and push succeed where the spec allows them, and every push with a
// ref update outside push_refs is refused whole, the server untouched.
func TestGitDifferential(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	env := gitEnv(home)
	execPath := strings.TrimSpace(mustGit(t, home, env, "--exec-path"))
	root := t.TempDir()
	for _, repo := range []string{"org/repo.git", "org/ro.git", "org/secret.git"} {
		dir := filepath.Join(root, repo)
		mustGit(t, root, env, "init", "--bare", "-b", "main", dir)
		mustGit(t, dir, env, "config", "http.receivepack", "true")
	}
	seed := filepath.Join(t.TempDir(), "seed")
	mustGit(t, home, env, "init", "-b", "main", seed)
	mustGit(t, seed, env, "commit", "--allow-empty", "-m", "base")
	for _, repo := range []string{"org/repo.git", "org/ro.git", "org/secret.git"} {
		mustGit(t, seed, env, "push", filepath.Join(root, repo), "main", "refs/heads/main:refs/heads/factory/i1/existing")
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("factory-agent:git-secret"))
	backend := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	var sawPush bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			http.Error(w, "no credential", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			sawPush = true
		}
		backend.ServeHTTP(w, r)
	}))
	defer up.Close()
	_, port := hostPort(t, up.URL)
	host := "git.test:" + port
	h := newHarness(t, `{"allow":[{"name":"git","hosts":["`+host+`"]}],
		"repos":[{"host":"`+host+`","path":"org/repo","access":"write"},{"host":"`+host+`","path":"org/ro","access":"read"}],
		"push_refs":["refs/heads/factory/i1/","refs/heads/factory-wip/s1"],
		"credentials":[{"host":"`+host+`","secret_ref":"factory/git-token","scheme":"basic","username":"factory-agent","allow_plain_http":true}]}`,
		nil, "git.test")
	clientEnv := gitEnv(home, "http_proxy="+h.proxy.URL, "HTTP_PROXY="+h.proxy.URL, "no_proxy=", "NO_PROXY=")

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, home, clientEnv, "clone", "http://"+host+"/org/repo.git", work)
	mustGit(t, work, clientEnv, "commit", "--allow-empty", "-m", "change")

	serverRef := func(repo, ref string) string {
		out, err := runGit(t, filepath.Join(root, repo), env, "rev-parse", "--verify", "-q", ref)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	mainBefore := serverRef("org/repo.git", "refs/heads/main")
	head := strings.TrimSpace(mustGit(t, work, env, "rev-parse", "HEAD"))

	for _, tc := range []struct {
		name    string
		args    []string
		ok      bool
		changed map[string]string // ref -> expected value after ("" = absent)
	}{
		{"allowed branch", []string{"push", "origin", "HEAD:refs/heads/factory/i1/impl"}, true,
			map[string]string{"refs/heads/factory/i1/impl": head}},
		{"checkpoint", []string{"push", "origin", "HEAD:refs/heads/factory-wip/s1"}, true,
			map[string]string{"refs/heads/factory-wip/s1": head}},
		{"main", []string{"push", "origin", "HEAD:refs/heads/main"}, false,
			map[string]string{"refs/heads/main": mainBefore}},
		{"another session", []string{"push", "origin", "HEAD:refs/heads/factory-wip/s2"}, false,
			map[string]string{"refs/heads/factory-wip/s2": ""}},
		{"tag", []string{"push", "origin", "HEAD:refs/tags/v1"}, false,
			map[string]string{"refs/tags/v1": ""}},
		{"mixed", []string{"push", "origin", "HEAD:refs/heads/factory/i1/mixed", "HEAD:refs/heads/main"}, false,
			map[string]string{"refs/heads/factory/i1/mixed": "", "refs/heads/main": mainBefore}},
		{"delete allowed", []string{"push", "origin", ":refs/heads/factory/i1/existing"}, true,
			map[string]string{"refs/heads/factory/i1/existing": ""}},
		{"delete main", []string{"push", "origin", ":refs/heads/main"}, false,
			map[string]string{"refs/heads/main": mainBefore}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawPush = false
			out, err := runGit(t, work, clientEnv, tc.args...)
			if (err == nil) != tc.ok {
				t.Fatalf("git %v: err=%v, want ok=%v\n%s", tc.args, err, tc.ok, out)
			}
			if !tc.ok && sawPush {
				t.Fatal("a refused push reached the server")
			}
			for ref, want := range tc.changed {
				if got := serverRef("org/repo.git", ref); got != want {
					t.Fatalf("server %s = %q, want %q", ref, got, want)
				}
			}
		})
	}

	// A read repository fetches but cannot push; an unlisted one cannot even fetch.
	ro := filepath.Join(t.TempDir(), "ro")
	mustGit(t, home, clientEnv, "clone", "http://"+host+"/org/ro.git", ro)
	mustGit(t, ro, clientEnv, "commit", "--allow-empty", "-m", "x")
	if out, err := runGit(t, ro, clientEnv, "push", "origin", "HEAD:refs/heads/factory/i1/ro"); err == nil {
		t.Fatalf("push to a read repository succeeded:\n%s", out)
	}
	if out, err := runGit(t, home, clientEnv, "clone", "http://"+host+"/org/secret.git", filepath.Join(t.TempDir(), "s")); err == nil {
		t.Fatalf("clone of an unlisted repository succeeded:\n%s", out)
	}
	if out, err := runGit(t, home, clientEnv, "clone", "http://"+host+"/Org/Secret.git", filepath.Join(t.TempDir(), "s2")); err == nil {
		t.Fatalf("clone of an unlisted repository by another spelling succeeded:\n%s", out)
	}
}

// A gzip push body is decoded to read its commands and forwarded decoded; any
// other coding is refused.
func TestPushBodyEncodings(t *testing.T) {
	const a, b = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	line := a + " " + b + " refs/heads/factory/i1/x\x00report-status\n"
	cmds := fmt.Sprintf("%04x%s0000PACKDATA", len(line)+4, line)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(cmds))
	_ = zw.Close()
	for _, tc := range []struct {
		enc    string
		body   []byte
		reason string
	}{
		{"", []byte(cmds), ""},
		{"gzip", gz.Bytes(), ""},
		{"br", []byte(cmds), ReasonPathNotAllowed},
		{"gzip", []byte("not gzip"), ReasonPathNotAllowed},
	} {
		r := httptest.NewRequest("POST", "/org/repo/git-receive-pack", bytes.NewReader(tc.body))
		if tc.enc != "" {
			r.Header.Set("Content-Encoding", tc.enc)
		}
		body, reason := pushBody(r, []string{"refs/heads/factory/i1/"})
		if reason != tc.reason {
			t.Fatalf("%q: reason %q, want %q", tc.enc, reason, tc.reason)
		}
		if reason == "" {
			got, _ := io.ReadAll(body)
			if string(got) != cmds {
				t.Fatalf("%q: forwarded %q", tc.enc, got)
			}
		}
	}
}
