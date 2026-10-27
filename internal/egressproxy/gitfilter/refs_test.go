package gitfilter

import (
	"errors"
	"strings"
	"testing"
)

var factoryRefs = Refs{
	Hide: []string{"refs/heads/factory/", "refs/heads/factory-wip/"},
	Show: []string{"refs/heads/factory/i1/", "refs/heads/factory-wip/i1."},
}

const (
	oidA = "1111111111111111111111111111111111111111"
	oidB = "2222222222222222222222222222222222222222"
	oidC = "3333333333333333333333333333333333333333"
)

func TestRefsVisible(t *testing.T) {
	for ref, want := range map[string]bool{
		"refs/heads/main":                  true,
		"refs/heads/integration":           true,
		"refs/tags/v1":                     true,
		"refs/heads/factory":               true, // not under the prefix
		"refs/heads/factory/i1/impl":       true,
		"refs/heads/factory-wip/i1.a.b.1":  true,
		"refs/heads/factory/i1/":           false, // a prefix matches longer refs only
		"refs/heads/factory/i2/impl":       false,
		"refs/heads/factory/i10/impl":      false,
		"refs/heads/factory-wip/i10.a.b.1": false,
		"refs/heads/factory-wip/i1":        false,
		"refs/heads/factory-wip/i2.a":      false,
	} {
		if got := factoryRefs.Visible(ref); got != want {
			t.Errorf("Visible(%q) = %v, want %v", ref, got, want)
		}
	}
	if (Refs{}).Active() || !factoryRefs.Active() {
		t.Fatal("Active")
	}
	exact := Refs{Hide: []string{"refs/heads/secret"}}
	if exact.Visible("refs/heads/secret") || !exact.Visible("refs/heads/secret2") {
		t.Fatal("an exact entry")
	}
}

func TestProtocolV2(t *testing.T) {
	for h, want := range map[string]bool{
		"version=2": true, "version=2:object-format=sha1": true, "foo=bar:version=2": true,
		"": false, "version=1": false, "version=0": false, "version=20": false, "version=2x": false,
	} {
		if ProtocolV2(h) != want {
			t.Errorf("ProtocolV2(%q) != %v", h, want)
		}
	}
}

func v2(command string, caps []string, args ...string) string {
	var b strings.Builder
	b.WriteString(pkt("command=" + command + "\n"))
	for _, c := range caps {
		b.WriteString(pkt(c + "\n"))
	}
	b.WriteString("0001")
	for _, a := range args {
		b.WriteString(pkt(a + "\n"))
	}
	return b.String() + "0000"
}

func TestReadRequest(t *testing.T) {
	req, err := ReadRequest([]byte(v2("fetch", []string{"agent=git/2", "object-format=sha1"}, "want "+oidA, "done")))
	if err != nil || req.Command != "fetch" || len(req.Caps) != 2 || len(req.Args) != 2 || req.Args[0] != "want "+oidA {
		t.Fatalf("%+v %v", req, err)
	}
	if req, err := ReadRequest([]byte("0000")); err != nil || req.Command != "" {
		t.Fatalf("empty request: %+v %v", req, err)
	}
	if req, err := ReadRequest([]byte(pkt("command=ls-refs\n") + "0000")); err != nil || req.Command != "ls-refs" {
		t.Fatalf("no arguments: %+v %v", req, err)
	}
	for name, body := range map[string]string{
		"nothing":            "",
		"no flush":           pkt("command=ls-refs\n") + "0001",
		"after the flush":    v2("ls-refs", nil) + "0000",
		"two commands":       v2("ls-refs", nil) + v2("ls-refs", nil),
		"no command":         pkt("agent=x\n") + "0001" + "0000",
		"delimiter first":    "0001" + "0000",
		"two delimiters":     pkt("command=fetch\n") + "0001" + "0001" + "0000",
		"a response end":     pkt("command=fetch\n") + "0002" + "0000",
		"an odd capability":  pkt("command=fetch\n") + pkt("Agent=x\n") + "0001" + "0000",
		"a short packet":     pkt("command=fetch\n") + "0003",
		"a bad length":       "zzzz",
		"a truncated packet": "0010command",
	} {
		if _, err := ReadRequest([]byte(body)); err == nil {
			t.Errorf("%s: accepted %q", name, body)
		}
	}
}

func TestFetchWants(t *testing.T) {
	wants, err := FetchWants([]string{
		"want " + oidA, "want " + oidB, "have " + oidC, "shallow " + oidC, "thin-pack", "ofs-delta",
		"no-progress", "include-tag", "deepen 1", "deepen-relative", "filter blob:none", "filter blob:limit=1k", "filter tree:0",
		"want-ref refs/heads/main", "want-ref refs/heads/factory/i1/x", "sideband-all", "wait-for-done", "done",
	}, factoryRefs)
	if err != nil || len(wants) != 2 || wants[0] != oidA || wants[1] != oidB {
		t.Fatalf("%v %v", wants, err)
	}
	for _, tc := range []struct {
		arg  string
		want error
	}{
		{"want-ref refs/heads/factory/i2/x", ErrRefused},
		{"deepen-not refs/heads/main", ErrRefused},
		{"filter sparse:oid=" + oidA, ErrRefused},
		{"filter combine:blob:none+tree:0", ErrRefused},
		{"filter tree:0+sparse:oid=x", ErrRefused},
		{"want refs/heads/main", ErrMalformed},
		{"want " + oidA[:39], ErrMalformed},
		{"have x", ErrMalformed},
		{"done now", ErrMalformed},
		{"frobnicate", ErrMalformed},
	} {
		if _, err := FetchWants([]string{tc.arg}, factoryRefs); !errors.Is(err, tc.want) {
			t.Errorf("%q: %v, want %v", tc.arg, err, tc.want)
		}
	}
	if CheckLsRefs([]string{"symrefs", "peel", "unborn", "ref-prefix refs/heads/"}) != nil || CheckLsRefs([]string{"frob"}) == nil {
		t.Fatal("CheckLsRefs")
	}
}

func TestLsRefsRequest(t *testing.T) {
	req, err := ReadRequest(LsRefsRequest([]string{"object-format=sha256"}))
	if err != nil || req.Command != "ls-refs" || len(req.Caps) != 1 || strings.Join(req.Args, ",") != "symrefs,peel" {
		t.Fatalf("%+v %v", req, err)
	}
}

func TestFilterLsRefs(t *testing.T) {
	in := pkt(oidA+" HEAD symref-target:refs/heads/main\n") +
		pkt(oidA+" refs/heads/main\n") +
		pkt(oidB+" refs/heads/factory/i1/impl\n") +
		pkt(oidC+" refs/heads/factory/i2/impl\n") +
		pkt(oidC+" refs/heads/alias symref-target:refs/heads/factory/i2/impl\n") +
		pkt(oidC+" refs/tags/hidden-target peeled:"+oidB+"\n") +
		pkt("unborn refs/heads/x\n") +
		"0000"
	var seen []string
	out, err := FilterLsRefs(strings.NewReader(in), factoryRefs, func(id string) { seen = append(seen, id) })
	if err != nil {
		t.Fatal(err)
	}
	want := pkt(oidA+" HEAD symref-target:refs/heads/main\n") + pkt(oidA+" refs/heads/main\n") +
		pkt(oidB+" refs/heads/factory/i1/impl\n") + pkt(oidC+" refs/tags/hidden-target peeled:"+oidB+"\n") +
		pkt("unborn refs/heads/x\n") + "0000"
	if string(out) != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}
	if strings.Join(seen, ",") != strings.Join([]string{oidA, oidA, oidB, oidC, oidB}, ",") {
		t.Fatalf("seen %v", seen)
	}

	for name, in := range map[string]string{
		"a detached HEAD":      pkt(oidC+" HEAD\n") + "0000",
		"a hidden HEAD":        pkt(oidC+" HEAD symref-target:refs/heads/factory/i2/impl\n") + "0000",
		"a name outside refs/": pkt(oidC+" x/refs/heads/factory/i2/impl\n") + "0000",
	} {
		out, err := FilterLsRefs(strings.NewReader(in), factoryRefs, func(string) {})
		if err != nil || string(out) != "0000" {
			t.Errorf("%s: %q %v", name, out, err)
		}
	}
	for name, in := range map[string]string{
		"no flush":            pkt(oidA + " refs/heads/main\n"),
		"after the flush":     "0000" + pkt(oidC+" refs/heads/factory/i2/impl\n"),
		"one field":           pkt(oidA+"\n") + "0000",
		"a delimiter":         "0001" + "0000",
		"an embedded newline": pkt(oidA+" refs/heads/main\n"+oidC+" refs/heads/factory/i2/impl\n") + "0000",
		"a bad id":            pkt("xyz refs/heads/main\n") + "0000",
	} {
		if _, err := FilterLsRefs(strings.NewReader(in), factoryRefs, func(string) {}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFilterV2Advert(t *testing.T) {
	caps := pkt("version 2\n") + pkt("agent=git/2.54\n") + pkt("ls-refs=unborn\n") + pkt("fetch=shallow wait-for-done filter\n") +
		pkt("server-option\n") + pkt("object-info\n") + pkt("bundle-uri\n") + pkt("object-format=sha1\n") + "0000"
	want := pkt("version 2\n") + pkt("agent=git/2.54\n") + pkt("ls-refs=unborn\n") + pkt("fetch=shallow wait-for-done filter\n") +
		pkt("server-option\n") + pkt("object-format=sha1\n") + "0000"
	service := pkt("# service=git-upload-pack\n") + "0000"
	for name, tc := range map[string]struct{ in, want string }{
		"git http-backend": {caps, want},
		"Forgejo":          {service + caps, service + want},
	} {
		out, err := FilterV2Advert(strings.NewReader(tc.in))
		if err != nil || string(out) != tc.want {
			t.Errorf("%s: %q %v", name, out, err)
		}
	}
	v0 := service + pkt(oidA+" HEAD\x00multi_ack\n") + pkt(oidC+" refs/heads/factory/i2/impl\n") + "0000"
	if _, err := FilterV2Advert(strings.NewReader(v0)); err == nil {
		t.Fatal("a v0 advertisement passed")
	}
}

func TestFilterPushAdvert(t *testing.T) {
	service := pkt("# service=git-receive-pack\n") + "0000"
	in := service + pkt(oidC+" refs/heads/factory/i2/impl\x00report-status push-cert=abc delete-refs\n") +
		pkt(oidA+" refs/heads/main\n") + pkt(oidB+" refs/heads/factory/i1/impl\n") + pkt(oidC+" .have\n") + "0000"
	out, err := FilterPushAdvert(strings.NewReader(in), factoryRefs)
	if err != nil {
		t.Fatal(err)
	}
	want := service + pkt(oidA+" refs/heads/main\x00report-status delete-refs\n") + pkt(oidB+" refs/heads/factory/i1/impl\n") + "0000"
	if string(out) != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}

	only := service + pkt(oidC+" refs/heads/factory/i2/impl\x00report-status\n") + "0000"
	out, err = FilterPushAdvert(strings.NewReader(only), factoryRefs)
	if want := service + pkt(strings.Repeat("0", 40)+" capabilities^{}\x00report-status\n") + "0000"; err != nil || string(out) != want {
		t.Fatalf("all hidden: %q %v", out, err)
	}
	empty := service + pkt(strings.Repeat("0", 40)+" capabilities^{}\x00report-status\n") + "0000"
	if out, err = FilterPushAdvert(strings.NewReader(empty), factoryRefs); err != nil || string(out) != empty {
		t.Fatalf("empty repository: %q %v", out, err)
	}
	for name, in := range map[string]string{
		"no service line": pkt(oidA+" refs/heads/main\x00report-status\n") + "0000",
		"no capabilities": service + pkt(oidA+" refs/heads/main\n") + "0000",
		"trailing data":   empty + "0000",
		"no end":          service + pkt(oidA+" refs/heads/main\x00report-status\n"),
		"a bad id":        service + pkt("xyz refs/heads/main\x00report-status\n") + "0000",
	} {
		if _, err := FilterPushAdvert(strings.NewReader(in), factoryRefs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func FuzzReadRequest(f *testing.F) {
	f.Add([]byte(v2("fetch", []string{"agent=x"}, "want "+oidA, "done")))
	f.Add([]byte("0000"))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := ReadRequest(body)
		if err != nil {
			return
		}
		if req.Command == "fetch" {
			_, _ = FetchWants(req.Args, factoryRefs)
		}
	})
}

func FuzzFilterLsRefs(f *testing.F) {
	f.Add([]byte(pkt(oidA+" HEAD symref-target:refs/heads/main\n") + pkt(oidC+" refs/heads/factory/i2/x\n") + "0000"))
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := FilterLsRefs(strings.NewReader(string(in)), factoryRefs, func(string) {})
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			ref := fields[1]
			if ref != "HEAD" && (!strings.HasPrefix(ref, "refs/") || !factoryRefs.Visible(ref)) {
				t.Fatalf("ref %q passed: %q", ref, out)
			}
			for _, attr := range fields[2:] {
				if target, ok := strings.CutPrefix(attr, "symref-target:"); ok && !factoryRefs.Visible(target) {
					t.Fatalf("symref to %q passed: %q", target, out)
				}
			}
		}
	})
}
