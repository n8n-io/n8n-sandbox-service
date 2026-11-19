package egressproxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// refsFixture is a Forgejo-like repository (any object id may be wanted, as
// Forgejo configures upload-pack) holding instance i1's refs, which the
// sandbox may see, and i2's and i10's, which it may not, behind the proxy.
type refsFixture struct {
	t                 *testing.T
	h                 *harness
	host, repoDir     string
	env, clientEnv    []string
	home              string
	ownTip, ownParent string
	otherTip          string // factory/i2/secret
	otherParent       string
	otherWIP          string // factory-wip/i2.s1
	i10Tip            string // factory/i10/x: "i1" must not match it
	dangling          string
	secretBlob        string
	spec              string
	pushes            int
	uploads           atomic.Int32 // upload-pack POSTs that reached the server
}

const refsSpec = `"hide_refs":["refs/heads/factory/","refs/heads/factory-wip/"],
	"show_refs":["refs/heads/factory/i1/","refs/heads/factory-wip/i1."],
	"push_refs":["refs/heads/factory/i1/","refs/heads/factory-wip/i1.s1"]`

func newRefsFixture(t *testing.T) *refsFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := &refsFixture{t: t, home: t.TempDir()}
	f.env = gitEnv(f.home)
	execPath := strings.TrimSpace(mustGit(t, f.home, f.env, "--exec-path"))
	root := t.TempDir()
	f.repoDir = filepath.Join(root, "org/repo.git")
	mustGit(t, root, f.env, "init", "--bare", "-b", "main", f.repoDir)
	for _, kv := range [][2]string{{"http.receivepack", "true"}, {"uploadpack.allowAnySHA1InWant", "true"}, {"uploadpack.allowFilter", "true"}} {
		mustGit(t, f.repoDir, f.env, "config", kv[0], kv[1])
	}

	seed := filepath.Join(t.TempDir(), "seed")
	mustGit(t, f.home, f.env, "init", "-b", "main", seed)
	commit := func(msg string) string {
		mustGit(t, seed, f.env, "commit", "--allow-empty", "-m", msg)
		return strings.TrimSpace(mustGit(t, seed, f.env, "rev-parse", "HEAD"))
	}
	push := func(refspec string) { mustGit(t, seed, f.env, "push", "-q", f.repoDir, refspec) }
	commit("base")
	push("HEAD:refs/heads/main")
	f.ownParent = commit("own 1")
	f.ownTip = commit("own 2")
	push("HEAD:refs/heads/factory/i1/existing")
	mustGit(t, seed, f.env, "checkout", "-q", "-B", "wip", "main")
	commit("own checkpoint")
	push("HEAD:refs/heads/factory-wip/i1.s0")
	mustGit(t, seed, f.env, "checkout", "-q", "-B", "other", "main")
	f.otherParent = commit("other 1")
	if err := os.WriteFile(filepath.Join(seed, "secret.txt"), []byte("other instance's unpublished work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seed, f.env, "add", "secret.txt")
	f.otherTip = commit("other 2")
	f.secretBlob = strings.TrimSpace(mustGit(t, seed, f.env, "rev-parse", "HEAD:secret.txt"))
	push("HEAD:refs/heads/factory/i2/secret")
	f.otherWIP = commit("other checkpoint")
	push("HEAD:refs/heads/factory-wip/i2.s1")
	mustGit(t, seed, f.env, "checkout", "-q", "-B", "ten", "main")
	f.i10Tip = commit("i10")
	push("HEAD:refs/heads/factory/i10/x")
	push("HEAD:refs/heads/factory-wip/i10.s1")
	f.dangling = commit("dangling")
	push("HEAD:refs/heads/tmp")
	push(":refs/heads/tmp")

	backend := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + f.home},
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			f.pushes++
		}
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			f.uploads.Add(1)
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	_, port := hostPort(t, up.URL)
	f.host = "git.test:" + port
	f.spec = `{"allow":[{"name":"git","hosts":["` + f.host + `"]}],
		"repos":[{"host":"` + f.host + `","path":"org/repo","access":"write"}],` + refsSpec + `}`
	f.h = newHarness(t, f.spec, nil, "git.test")
	f.clientEnv = gitEnv(f.home, "http_proxy="+f.h.proxy.URL, "HTTP_PROXY="+f.h.proxy.URL, "no_proxy=", "NO_PROXY=")
	return f
}

func (f *refsFixture) url() string { return "http://" + f.host + "/org/repo.git" }

// emptyRepo is a client repository with origin set and nothing fetched.
func (f *refsFixture) emptyRepo() string {
	dir := filepath.Join(f.t.TempDir(), "c")
	mustGit(f.t, f.home, f.env, "init", "-q", "-b", "main", dir)
	mustGit(f.t, dir, f.env, "remote", "add", "origin", f.url())
	return dir
}

func (f *refsFixture) serverRef(ref string) string {
	out, err := runGit(f.t, f.repoDir, f.env, "rev-parse", "--verify", "-q", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// raw sends one request to the Git host through the proxy as a guest would.
func (f *refsFixture) raw(method, path, protocol, contentType string, body []byte) (*http.Response, []byte) {
	f.t.Helper()
	req, _ := http.NewRequest(method, "http://"+f.host+"/org/repo.git"+path, bytes.NewReader(body))
	if protocol != "" {
		req.Header.Set("Git-Protocol", protocol)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := f.h.guest.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// v2Request frames a protocol v2 command with its arguments.
func v2Request(command string, args ...string) []byte {
	var b strings.Builder
	b.WriteString(pktLine("command=" + command + "\n"))
	b.WriteString(pktLine("object-format=sha1\n"))
	b.WriteString("0001")
	for _, a := range args {
		b.WriteString(pktLine(a + "\n"))
	}
	b.WriteString("0000")
	return []byte(b.String())
}

// A sandbox sees and fetches only the base refs and its own instance's
// factory refs: other instances' refs are not listed, not fetchable by name,
// and their objects not fetchable by id, while its own work, the base and its
// pushes keep working.
func TestGitRefVisibility(t *testing.T) {
	f := newRefsFixture(t)
	hidden := []string{"factory/i2/", "factory-wip/i2.", "factory/i10/", "factory-wip/i10.", f.otherTip, f.otherWIP, f.i10Tip}

	t.Run("ls-remote lists the base and the own refs only", func(t *testing.T) {
		out := mustGit(t, f.home, f.clientEnv, "ls-remote", "--symref", f.url())
		for _, want := range []string{"ref: refs/heads/main\tHEAD", "refs/heads/main", "refs/heads/factory/i1/existing", "refs/heads/factory-wip/i1.s0", f.ownTip} {
			if !strings.Contains(out, want) {
				t.Fatalf("ls-remote lacks %q:\n%s", want, out)
			}
		}
		for _, h := range hidden {
			if strings.Contains(out, h) {
				t.Fatalf("ls-remote shows %q:\n%s", h, out)
			}
		}
	})

	t.Run("clone gets the visible refs and none of the hidden objects", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clone")
		mustGit(t, f.home, f.clientEnv, "clone", "-q", "--mirror", f.url(), dir)
		refs := mustGit(t, dir, f.env, "for-each-ref", "--format=%(refname)")
		for _, h := range hidden[:4] {
			if strings.Contains(refs, h) {
				t.Fatalf("clone has %q:\n%s", h, refs)
			}
		}
		for _, id := range []string{f.otherTip, f.otherParent, f.otherWIP, f.i10Tip, f.secretBlob} {
			if _, err := runGit(t, dir, f.env, "cat-file", "-e", id); err == nil {
				t.Fatalf("clone holds hidden object %s", id)
			}
		}
		if _, err := runGit(t, dir, f.env, "cat-file", "-e", f.ownTip); err != nil {
			t.Fatal("clone lacks the own branch's tip")
		}
	})

	for _, tc := range []struct{ name, what string }{
		{"another instance's branch by name", "refs/heads/factory/i2/secret"},
		{"another instance's checkpoint by name", "refs/heads/factory-wip/i2.s1"},
		{"a prefix-sharing instance's branch by name", "refs/heads/factory/i10/x"},
		{"another instance's tip by id", f.otherTip},
		{"another instance's older commit by id", f.otherParent},
		{"another instance's checkpoint by id", f.otherWIP},
		{"another instance's blob by id", f.secretBlob},
		{"an unreferenced commit by id", f.dangling},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			dir := f.emptyRepo()
			before := f.pushes
			if out, err := runGit(t, dir, f.clientEnv, "fetch", "origin", tc.what); err == nil {
				t.Fatalf("fetch %s succeeded:\n%s", tc.what, out)
			}
			for _, id := range []string{f.otherTip, f.otherParent, f.otherWIP, f.i10Tip, f.secretBlob, f.dangling} {
				if _, err := runGit(t, dir, f.env, "cat-file", "-e", id); err == nil {
					t.Fatalf("fetch of %s brought hidden object %s", tc.what, id)
				}
			}
			if f.pushes != before {
				t.Fatal("a fetch pushed")
			}
		})
	}

	t.Run("fetches the own refs by name and the own tip by id", func(t *testing.T) {
		dir := f.emptyRepo()
		mustGit(t, dir, f.clientEnv, "fetch", "-q", "origin", "refs/heads/factory/i1/existing", "refs/heads/factory-wip/i1.s0", "refs/heads/main")
		dir = f.emptyRepo()
		// A fetch of ids alone lists no refs first: the proxy looks the tips up itself.
		mustGit(t, dir, f.clientEnv, "fetch", "-q", "origin", f.ownTip)
		mustGit(t, dir, f.env, "cat-file", "-e", f.ownTip)
	})

	t.Run("a fetch of a visible tip by id with nothing listed yet", func(t *testing.T) {
		f.h.set(f.spec) // a replaced policy forgets what was listed
		resp, body := f.raw("POST", "/git-upload-pack", "version=2", "application/x-git-upload-pack-request",
			v2Request("fetch", "want "+f.ownTip, "no-progress", "done"))
		if resp.StatusCode != 200 || !bytes.Contains(body, []byte("packfile")) {
			t.Fatalf("fetch of the own tip: %d %q", resp.StatusCode, body)
		}
		f.h.set(f.spec)
		resp, _ = f.raw("POST", "/git-upload-pack", "version=2", "application/x-git-upload-pack-request",
			v2Request("fetch", "want "+f.otherTip, "no-progress", "done"))
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get(RefusalHeader) != ReasonRefNotAllowed {
			t.Fatalf("fetch of a hidden tip: %d %q", resp.StatusCode, resp.Header.Get(RefusalHeader))
		}
	})

	t.Run("the proxy lists refs for a sandbox at most once a second", func(t *testing.T) {
		f.h.set(f.spec)
		for i, id := range []string{strings.Repeat("ab", 20), strings.Repeat("cd", 20), strings.Repeat("ef", 20)} {
			before := f.uploads.Load()
			resp, _ := f.raw("POST", "/git-upload-pack", "version=2", "application/x-git-upload-pack-request",
				v2Request("fetch", "want "+id, "done"))
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get(RefusalHeader) != ReasonRefNotAllowed {
				t.Fatalf("made-up id %d: %d %q", i, resp.StatusCode, resp.Header.Get(RefusalHeader))
			}
			want := int32(0) // within the interval, nothing reaches the server
			if i == 0 {
				want = 1 // the proxy's own listing
			}
			if got := f.uploads.Load() - before; got != want {
				t.Fatalf("made-up id %d: %d requests upstream, want %d", i, got, want)
			}
		}
	})

	t.Run("a tip the sandbox was shown stays fetchable after its ref moves", func(t *testing.T) {
		mustGit(t, f.home, f.clientEnv, "ls-remote", f.url())
		mustGit(t, f.repoDir, f.env, "update-ref", "refs/heads/factory/i1/existing", f.ownParent)
		t.Cleanup(func() { mustGit(t, f.repoDir, f.env, "update-ref", "refs/heads/factory/i1/existing", f.ownTip) })
		dir := f.emptyRepo()
		mustGit(t, dir, f.clientEnv, "fetch", "-q", "origin", f.ownTip)
	})

	t.Run("an id below a visible tip is not wantable on its own", func(t *testing.T) {
		// Documented in the API: only tips are wanted by id, so the proxy needs no object graph.
		dir := f.emptyRepo()
		if out, err := runGit(t, dir, f.clientEnv, "fetch", "origin", f.ownParent); err == nil {
			t.Fatalf("fetch of a non-tip id succeeded:\n%s", out)
		}
	})

	t.Run("pushes the own refs, not another instance's", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "w")
		mustGit(t, f.home, f.clientEnv, "clone", "-q", "-b", "factory/i1/existing", f.url(), dir)
		mustGit(t, dir, f.env, "commit", "--allow-empty", "-m", "agent")
		head := strings.TrimSpace(mustGit(t, dir, f.env, "rev-parse", "HEAD"))
		mustGit(t, dir, f.clientEnv, "push", "-q", "origin", "HEAD:refs/heads/factory/i1/new", "HEAD:refs/heads/factory-wip/i1.s1")
		if f.serverRef("refs/heads/factory/i1/new") != head || f.serverRef("refs/heads/factory-wip/i1.s1") != head {
			t.Fatal("the own push did not land")
		}
		if out, err := runGit(t, dir, f.clientEnv, "push", "origin", "HEAD:refs/heads/factory/i2/secret"); err == nil {
			t.Fatalf("push to another instance's branch succeeded:\n%s", out)
		}
		if f.serverRef("refs/heads/factory/i2/secret") != f.otherTip {
			t.Fatal("another instance's branch moved")
		}
	})

	t.Run("protocol v0 and v1 are refused", func(t *testing.T) {
		for _, v := range []string{"0", "1"} {
			if out, err := runGit(t, f.home, f.clientEnv, "-c", "protocol.version="+v, "ls-remote", f.url()); err == nil {
				t.Fatalf("ls-remote over protocol %s succeeded:\n%s", v, out)
			}
		}
		resp, _ := f.raw("GET", "/info/refs?service=git-upload-pack", "", "", nil)
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get(RefusalHeader) != ReasonProtocolNotAllowed {
			t.Fatalf("v0 advertisement: %d %q", resp.StatusCode, resp.Header.Get(RefusalHeader))
		}
		body := pktLine("want "+f.otherTip+" ofs-delta\n") + "0000" + pktLine("done\n")
		resp, _ = f.raw("POST", "/git-upload-pack", "", "application/x-git-upload-pack-request", []byte(body))
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get(RefusalHeader) != ReasonProtocolNotAllowed {
			t.Fatalf("v0 fetch: %d %q", resp.StatusCode, resp.Header.Get(RefusalHeader))
		}
	})

	t.Run("the push advertisement lists the visible refs only", func(t *testing.T) {
		resp, body := f.raw("GET", "/info/refs?service=git-receive-pack", "", "", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("receive-pack advertisement: %d %s", resp.StatusCode, body)
		}
		for _, h := range hidden {
			if bytes.Contains(body, []byte(h)) {
				t.Fatalf("receive-pack advertisement shows %q:\n%q", h, body)
			}
		}
		if !bytes.Contains(body, []byte("refs/heads/factory/i1/existing")) || bytes.Contains(body, []byte("push-cert")) {
			t.Fatalf("receive-pack advertisement:\n%q", body)
		}
	})

	t.Run("refuses v2 requests that name hidden refs or read objects another way", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body []byte
		}{
			{"object-info", v2Request("object-info", "size", "oid "+f.secretBlob)},
			{"bundle-uri", v2Request("bundle-uri")},
			{"want-ref of a hidden ref", v2Request("fetch", "want-ref refs/heads/factory/i2/secret", "done")},
			{"a sparse filter naming a hidden blob", v2Request("fetch", "want "+f.ownTip, "filter sparse:oid="+f.secretBlob, "done")},
			{"a sparse filter naming a hidden ref", v2Request("fetch", "want "+f.ownTip, "filter sparse:oid=refs/heads/factory/i2/secret:secret.txt", "done")},
			{"deepen-not a hidden ref", v2Request("fetch", "want "+f.ownTip, "deepen-not refs/heads/factory/i2/secret", "done")},
			{"an unknown argument", v2Request("fetch", "want "+f.ownTip, "frobnicate", "done")},
			{"two commands", append(v2Request("ls-refs"), v2Request("ls-refs")...)},
		} {
			resp, body := f.raw("POST", "/git-upload-pack", "version=2", "application/x-git-upload-pack-request", tc.body)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s: %d %q", tc.name, resp.StatusCode, body)
			}
		}
	})

	t.Run("hides HEAD when it names a hidden branch", func(t *testing.T) {
		mustGit(t, f.repoDir, f.env, "symbolic-ref", "HEAD", "refs/heads/factory/i2/secret")
		t.Cleanup(func() { mustGit(t, f.repoDir, f.env, "symbolic-ref", "HEAD", "refs/heads/main") })
		out := mustGit(t, f.home, f.clientEnv, "ls-remote", "--symref", f.url())
		if strings.Contains(out, "HEAD") || strings.Contains(out, f.otherTip) {
			t.Fatalf("ls-remote shows a HEAD on a hidden branch:\n%s", out)
		}
	})

	t.Run("advertises only the commands the proxy filters", func(t *testing.T) {
		resp, body := f.raw("GET", "/info/refs?service=git-upload-pack", "version=2", "", nil)
		if resp.StatusCode != 200 || !bytes.Contains(body, []byte("version 2")) || !bytes.Contains(body, []byte("ls-refs")) {
			t.Fatalf("v2 advertisement: %d %q", resp.StatusCode, body)
		}
	})
}

// The residual the proxy cannot close (docs/security-model.md): a sandbox that
// already knows an object id of another instance can push a commit that
// refers to it, which the Git server accepts because it holds the object, and
// then fetch it from its own ref. Object ids are not listed to it any more, so
// it needs one from elsewhere. This test pins the residual; if it starts
// failing, the gap is closed and the docs should say so.
func TestGitRefVisibilityResidualKnownID(t *testing.T) {
	f := newRefsFixture(t)
	// The attacker's pack is built outside the sandbox: a tree naming the hidden
	// blob, and a commit on the own tip, without the blob itself.
	scratch := filepath.Join(t.TempDir(), "scratch")
	mustGit(t, f.home, f.env, "clone", "-q", "--bare", f.repoDir, scratch)
	tree := strings.TrimSpace(mustGitIn(t, scratch, f.env, "100644 blob "+f.secretBlob+"\tleak.txt\n", "mktree"))
	commit := strings.TrimSpace(mustGit(t, scratch, f.env, "commit-tree", tree, "-p", f.ownTip, "-m", "leak"))
	pack := mustGitIn(t, scratch, f.env, commit+"\n"+tree+"\n", "pack-objects", "--stdout")

	zero := strings.Repeat("0", 40)
	body := pktLine(zero+" "+commit+" refs/heads/factory/i1/leak\x00report-status\n") + "0000" + pack
	resp, out := f.raw("POST", "/git-receive-pack", "", "application/x-git-receive-pack-request", []byte(body))
	if resp.StatusCode != 200 || !bytes.Contains(out, []byte("ok refs/heads/factory/i1/leak")) {
		t.Fatalf("push naming a known hidden object: %d %q", resp.StatusCode, out)
	}
	dir := filepath.Join(t.TempDir(), "w")
	mustGit(t, f.home, f.clientEnv, "clone", "-q", "-b", "factory/i1/leak", f.url(), dir)
	got, err := os.ReadFile(filepath.Join(dir, "leak.txt"))
	if err != nil || !strings.Contains(string(got), "unpublished") {
		t.Fatalf("residual closed? leak.txt: %q %v", got, err)
	}
}

func mustGitIn(t *testing.T, dir string, env []string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
