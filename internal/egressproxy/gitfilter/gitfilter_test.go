package gitfilter

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

var repos = []Repo{{Path: "org/repo", Write: true}, {Path: "org/docs"}}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		method, path, query string
		op                  Op
		reason              Reason
	}{
		{"GET", "/org/repo/info/refs", "service=git-upload-pack", OpFetch, ""},
		{"GET", "/org/repo.git/info/refs", "service=git-upload-pack", OpFetch, ""},
		{"GET", "/Org/Repo.git/info/refs", "service=git-upload-pack", OpFetch, ""},
		{"POST", "/org/repo/git-upload-pack", "", OpFetch, ""},
		{"GET", "/org/repo/info/refs", "service=git-receive-pack", OpAdvertisePush, ""},
		{"POST", "/org/repo.git/git-receive-pack", "", OpPush, ""},
		{"POST", "/org/docs/git-upload-pack", "", OpFetch, ""},
		// A read repository cannot push, nor see the push advertisement.
		{"POST", "/org/docs/git-receive-pack", "", 0, ReasonRepoNotAllowed},
		{"GET", "/org/docs/info/refs", "service=git-receive-pack", 0, ReasonRepoNotAllowed},
		{"POST", "/org/other/git-upload-pack", "", 0, ReasonRepoNotAllowed},
		{"POST", "/org/repo/subdir/git-upload-pack", "", 0, ReasonRepoNotAllowed},
		// Everything else on a Git host: the API, the web UI, LFS, odd forms.
		{"GET", "/api/v1/repos/org/repo", "", 0, ReasonPathNotAllowed},
		{"GET", "/org/repo", "", 0, ReasonPathNotAllowed},
		{"POST", "/org/repo/info/lfs/objects/batch", "", 0, ReasonPathNotAllowed},
		{"GET", "/org/repo/info/refs", "", 0, ReasonPathNotAllowed},
		{"GET", "/org/repo/info/refs", "service=git-upload-pack&x=1", 0, ReasonPathNotAllowed},
		{"GET", "/org/repo/info/refs", "service=git-upload-pack&service=git-receive-pack", 0, ReasonPathNotAllowed},
		{"POST", "/org/repo/git-upload-pack", "x=1", 0, ReasonPathNotAllowed},
		{"GET", "/org/%72epo/info/refs", "service=git-upload-pack", 0, ReasonPathNotAllowed},
		{"GET", "/org//repo/info/refs", "service=git-upload-pack", 0, ReasonPathNotAllowed},
		{"GET", "/org/./repo/info/refs", "service=git-upload-pack", 0, ReasonPathNotAllowed},
		{"GET", "/x/../org/repo/info/refs", "service=git-upload-pack", 0, ReasonPathNotAllowed},
		{"GET", "/org/repo/info/refs/", "service=git-upload-pack", 0, ReasonPathNotAllowed},
		{"GET", "/org/repoK/info/refs", "service=git-upload-pack", 0, ReasonPathNotAllowed},
		{"PUT", "/org/repo/git-receive-pack", "", 0, ReasonPathNotAllowed},
		{"GET", "/.git/info/refs", "service=git-upload-pack", 0, ReasonPathNotAllowed},
	} {
		op, _, reason := Check(tc.method, tc.path, tc.query, repos)
		if op != tc.op || reason != tc.reason {
			t.Errorf("%s %s?%s = %v %q, want %v %q", tc.method, tc.path, tc.query, op, reason, tc.op, tc.reason)
		}
	}
}

func TestRefAllowed(t *testing.T) {
	refs := []string{"refs/heads/factory/i1/", "refs/heads/factory-wip/s1"}
	for ref, want := range map[string]bool{
		"refs/heads/factory/i1/impl":  true,
		"refs/heads/factory/i1/a/b":   true,
		"refs/heads/factory/i1/":      false,
		"refs/heads/factory/i10/x":    false,
		"refs/heads/factory-wip/s1":   true,
		"refs/heads/factory-wip/s1/x": false,
		"refs/heads/factory-wip/s10":  false,
		"refs/heads/main":             false,
		"refs/tags/factory/i1/x":      false,
	} {
		if got := RefAllowed(ref, refs); got != want {
			t.Errorf("RefAllowed(%q) = %v", ref, got)
		}
	}
}

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

const (
	zero = "0000000000000000000000000000000000000000"
	a    = "1111111111111111111111111111111111111111"
	b    = "2222222222222222222222222222222222222222"
)

var pushRefs = []string{"refs/heads/factory/i1/", "refs/heads/factory-wip/s1"}

func TestReadCommands(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"update", pkt(a+" "+b+" refs/heads/factory/i1/impl\x00report-status side-band-64k\n") + "0000PACK", nil},
		{"create and delete", pkt(zero+" "+b+" refs/heads/factory-wip/s1\x00report-status\n") + pkt(a+" "+zero+" refs/heads/factory/i1/old\n") + "0000", nil},
		{"shallow first", pkt("shallow "+a+"\n") + pkt(a+" "+b+" refs/heads/factory/i1/x\x00caps\n") + "0000", nil},
		{"one ref outside", pkt(a+" "+b+" refs/heads/factory/i1/x\x00caps\n") + pkt(a+" "+b+" refs/heads/main\n") + "0000", ErrRefused},
		{"delete outside", pkt(a+" "+zero+" refs/heads/main\x00caps\n") + "0000", ErrRefused},
		{"tag", pkt(zero+" "+b+" refs/tags/v1\x00caps\n") + "0000", ErrRefused},
		{"invalid ref", pkt(a+" "+b+" refs/heads/factory/i1/a..b\x00caps\n") + "0000", ErrRefused},
		{"push cert", pkt("push-cert\x00caps\n") + pkt("certificate version 0.1\n") + pkt(a+" "+b+" refs/heads/main\n") + pkt("push-cert-end\n") + "0000", ErrMalformed},
		{"shallow after command", pkt(a+" "+b+" refs/heads/factory/i1/x\x00caps\n") + pkt("shallow "+a+"\n") + "0000", ErrMalformed},
		{"no commands", "0000", ErrMalformed},
		{"special packet", "0001", ErrMalformed},
		{"delim in list", pkt(a+" "+b+" refs/heads/factory/i1/x\x00caps\n") + "0001", ErrMalformed},
		{"short sha", pkt("abc "+b+" refs/heads/factory/i1/x\n") + "0000", ErrMalformed},
		{"mixed sha lengths", pkt(a+" "+b+b[:24]+" refs/heads/factory/i1/x\n") + "0000", ErrMalformed},
		{"truncated", pkt(a + " " + b + " refs/heads/factory/i1/x\n")[:20], ErrMalformed},
		{"no flush", pkt(a + " " + b + " refs/heads/factory/i1/x\n"), ErrMalformed},
		{"bad length", "zzzz", ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head, err := ReadCommands(bufio.NewReader(strings.NewReader(tc.body)), pushRefs)
			if err != tc.want {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if err == nil && !strings.HasPrefix(tc.body, string(head)) {
				t.Fatalf("head %q is not the body's prefix", head)
			}
		})
	}
}

func TestStripPushCert(t *testing.T) {
	adv := pkt("# service=git-receive-pack\n") + "0000" +
		pkt(a+" refs/heads/main\x00report-status push-cert=1700000000-abc delete-refs atomic\n") +
		pkt(b+" refs/heads/dev\n") + "0000"
	r, err := StripPushCert(strings.NewReader(adv))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	want := pkt("# service=git-receive-pack\n") + "0000" +
		pkt(a+" refs/heads/main\x00report-status delete-refs atomic\n") +
		pkt(b+" refs/heads/dev\n") + "0000"
	if string(out) != want {
		t.Fatalf("got %q\nwant %q", out, want)
	}
	if bytes.Contains(out, []byte("push-cert")) {
		t.Fatal("push-cert survived")
	}
}

// FuzzReadCommands: whatever the body, an accepted push names only allowed
// refs, and what is returned is exactly the body's head.
func FuzzReadCommands(f *testing.F) {
	f.Add([]byte(pkt(a+" "+b+" refs/heads/factory/i1/impl\x00report-status\n") + "0000PACK"))
	f.Add([]byte(pkt("shallow "+a+"\n") + pkt(a+" "+b+" refs/heads/main\n") + "0000"))
	f.Add([]byte("0001"))
	f.Fuzz(func(t *testing.T, body []byte) {
		head, err := ReadCommands(bufio.NewReader(bytes.NewReader(body)), pushRefs)
		if err != nil {
			return
		}
		if !bytes.HasPrefix(body, head) {
			t.Fatalf("head is not the body's prefix")
		}
		// Re-parse the accepted head independently: every command's ref is allowed.
		rest := head
		for len(rest) >= 4 {
			var n int
			if _, err := fmt.Sscanf(string(rest[:4]), "%04x", &n); err != nil {
				t.Fatalf("accepted bad length %q", rest[:4])
			}
			if n == 0 {
				break
			}
			line := string(rest[4:n])
			rest = rest[n:]
			if i := strings.IndexByte(line, 0); i >= 0 {
				line = line[:i]
			}
			line = strings.TrimSuffix(line, "\n")
			if strings.HasPrefix(line, "shallow ") {
				continue
			}
			parts := strings.Split(line, " ")
			if len(parts) != 3 || !RefAllowed(parts[2], pushRefs) {
				t.Fatalf("accepted command %q", line)
			}
		}
	})
}

func FuzzCheck(f *testing.F) {
	f.Add("GET", "/org/repo/info/refs", "service=git-upload-pack")
	f.Add("POST", "/org/docs/git-receive-pack", "")
	f.Fuzz(func(t *testing.T, method, path, query string) {
		op, repo, reason := Check(method, path, query, repos)
		if reason != "" {
			return
		}
		var listed *Repo
		for i := range repos {
			if repos[i].Path == repo {
				listed = &repos[i]
			}
		}
		if listed == nil {
			t.Fatalf("allowed unlisted repo %q for %s %s", repo, method, path)
		}
		if op != OpFetch && !listed.Write {
			t.Fatalf("allowed push to read repo %q", repo)
		}
		if strings.Contains(path, "%") || strings.Contains(path, "//") {
			t.Fatalf("allowed path %q", path)
		}
	})
}
