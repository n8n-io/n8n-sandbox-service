package gitfilter

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Refs is which refs a fetch may see: a ref matching Hide is hidden unless it
// also matches Show. An entry ending in '/' or '.' is a prefix, any other one
// exact ref. The Git server sends any object it holds to a protocol v2 fetch
// that names its id, so a hidden ref's objects stay unreadable only while no
// fetch may name an id the proxy has not listed (see WantsListed).
type Refs struct {
	Hide, Show []string
}

// Active reports whether any ref is hidden; an inactive Refs filters nothing.
func (v Refs) Active() bool { return len(v.Hide) > 0 }

// Visible reports whether ref may be listed and fetched.
func (v Refs) Visible(ref string) bool {
	return !matchRef(ref, v.Hide) || matchRef(ref, v.Show)
}

func matchRef(ref string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/") || strings.HasSuffix(p, ".") {
			if strings.HasPrefix(ref, p) && len(ref) > len(p) {
				return true
			}
		} else if ref == p {
			return true
		}
	}
	return false
}

// ProtocolV2 reports whether a Git-Protocol header asks for protocol v2, the
// only one whose ref listing and wants the proxy filters.
func ProtocolV2(header string) bool {
	for _, kv := range strings.Split(header, ":") {
		if kv == "version=2" {
			return true
		}
	}
	return false
}

// Request is one protocol v2 command a client sent to git-upload-pack.
// Command is empty for an empty request (a lone flush).
type Request struct {
	Command string
	Caps    []string
	Args    []string
}

// LsRefsRequest is the listing the proxy asks for itself, with the client's
// capabilities (its object format among them).
func LsRefsRequest(caps []string) []byte {
	var b bytes.Buffer
	line := func(l string) { fmt.Fprintf(&b, "%04x%s\n", len(l)+5, l) }
	line("command=ls-refs")
	for _, c := range caps {
		line(c)
	}
	b.WriteString("0001")
	line("symrefs")
	line("peel")
	b.WriteString("0000")
	return b.Bytes()
}

// MaxRequestBytes bounds a v2 request body, held in memory to be checked: a
// fetch's wants and haves, some 50 bytes each.
const MaxRequestBytes = 4 << 20

// ReadRequest parses a whole protocol v2 request: "command=<name>",
// capability lines, a delimiter, arguments, a flush, and nothing after it.
func ReadRequest(body []byte) (Request, error) {
	br := bufio.NewReader(bytes.NewReader(body))
	var req Request
	var scratch bytes.Buffer
	inArgs := false
	for first := true; ; first = false {
		payload, special, err := readV2Pkt(br, &scratch)
		if err != nil {
			return Request{}, err
		}
		switch special {
		case pktFlush:
			if _, err := br.ReadByte(); err != io.EOF {
				return Request{}, ErrMalformed
			}
			return req, nil
		case pktDelim:
			if inArgs || first {
				return Request{}, ErrMalformed
			}
			inArgs = true
			continue
		case pktData:
		default:
			return Request{}, ErrMalformed
		}
		line := strings.TrimSuffix(string(payload), "\n")
		switch {
		case first:
			name, ok := strings.CutPrefix(line, "command=")
			if !ok || name == "" {
				return Request{}, ErrMalformed
			}
			req.Command = name
		case inArgs:
			req.Args = append(req.Args, line)
		case !validCapability(line):
			return Request{}, ErrMalformed
		default:
			req.Caps = append(req.Caps, line)
		}
	}
}

// validCapability accepts a request capability line: a lowercase key and an
// optional value. Capabilities name no ref or object, so any is forwarded.
func validCapability(line string) bool {
	key, _, _ := strings.Cut(line, "=")
	if key == "" {
		return false
	}
	for _, c := range key {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return !strings.ContainsAny(line, "\x00\n")
}

// CheckLsRefs refuses an ls-refs request with an argument the proxy does not
// know (ErrMalformed). Its response is filtered by FilterLsRefs whatever it asked.
func CheckLsRefs(args []string) error {
	for _, a := range args {
		switch {
		case a == "symrefs", a == "peel", a == "unborn", strings.HasPrefix(a, "ref-prefix "):
		default:
			return ErrMalformed
		}
	}
	return nil
}

// FetchWants checks a fetch request's arguments and returns the object ids it
// wants. A want-ref of a hidden ref, deepen-not, which names a ref or object,
// and a sparse or combined filter, which reads a blob the request names, are
// ErrRefused; an argument the proxy does not know is ErrMalformed.
func FetchWants(args []string, v Refs) ([]string, error) {
	var wants []string
	for _, a := range args {
		key, val, _ := strings.Cut(a, " ")
		switch key {
		case "want":
			if !isHexID(val) {
				return nil, ErrMalformed
			}
			wants = append(wants, val)
		case "want-ref":
			if !v.Visible(val) {
				return nil, ErrRefused
			}
		case "have", "shallow":
			if !isHexID(val) {
				return nil, ErrMalformed
			}
		case "filter":
			if !allowedFilter(val) {
				return nil, ErrRefused
			}
		case "deepen", "deepen-since", "packfile-uris":
			if val == "" {
				return nil, ErrMalformed
			}
		case "done", "thin-pack", "no-progress", "include-tag", "ofs-delta", "deepen-relative", "sideband-all", "wait-for-done":
			if val != "" {
				return nil, ErrMalformed
			}
		case "deepen-not":
			return nil, ErrRefused
		default:
			return nil, ErrMalformed
		}
	}
	return wants, nil
}

func allowedFilter(spec string) bool {
	switch {
	case spec == "blob:none":
		return true
	case strings.HasPrefix(spec, "blob:limit="), strings.HasPrefix(spec, "tree:"), strings.HasPrefix(spec, "object:type="):
		return !strings.ContainsAny(spec, "+%")
	}
	return false
}

// MaxListingBytes bounds a ref listing, held in memory to be filtered.
const MaxListingBytes = 64 << 20

// FilterLsRefs reads an ls-refs response, "<oid> <ref>[ <attr>]*" lines then a
// flush, and returns it without the hidden refs or any name but HEAD and
// refs/*. A symref is kept only when
// its target is visible, and HEAD only as a symref, so a detached or hidden
// HEAD lists no hidden tip. seen receives each listed object id, peeled ones
// included.
func FilterLsRefs(r io.Reader, v Refs, seen func(oid string)) ([]byte, error) {
	br := bufio.NewReader(io.LimitReader(r, MaxListingBytes+1))
	var out, scratch bytes.Buffer
	for {
		scratch.Reset()
		payload, special, err := readV2Pkt(br, &scratch)
		if err != nil {
			return nil, err
		}
		if special == pktFlush {
			if _, err := br.ReadByte(); err != io.EOF {
				return nil, ErrMalformed
			}
			out.WriteString("0000")
			return out.Bytes(), nil
		}
		if special != pktData {
			return nil, ErrMalformed
		}
		line := strings.TrimSuffix(string(payload), "\n")
		fields := strings.Split(line, " ")
		if len(fields) < 2 || strings.ContainsAny(line, "\n\x00\t") {
			return nil, ErrMalformed
		}
		oid, ref, target := fields[0], fields[1], ""
		if !isHexID(oid) && oid != "unborn" {
			return nil, ErrMalformed
		}
		ids := []string{oid}
		for _, attr := range fields[2:] {
			if t, ok := strings.CutPrefix(attr, "symref-target:"); ok {
				target = t
			} else if p, ok := strings.CutPrefix(attr, "peeled:"); ok {
				ids = append(ids, p)
			}
		}
		named := ref == "HEAD" || strings.HasPrefix(ref, "refs/")
		if !named || !v.Visible(ref) || (target != "" && !v.Visible(target)) || (ref == "HEAD" && target == "") {
			continue
		}
		for _, id := range ids {
			if isHexID(id) {
				seen(id)
			}
		}
		out.Write(scratch.Bytes())
	}
}

// v2Commands are the commands a filtered upload-pack offers: the two whose
// requests and responses the proxy reads. object-info and bundle-uri are
// left out, and refused if sent.
var v2Commands = map[string]bool{"ls-refs": true, "fetch": true}

// v2Keys are the other capabilities a filtered advertisement keeps.
var v2Keys = map[string]bool{"agent": true, "server-option": true, "object-format": true, "session-id": true}

// FilterV2Advert reads an upload-pack protocol v2 capability advertisement,
// optionally after a "# service=" line and flush, and keeps only the commands
// the proxy filters. A server that answers with another protocol is an error.
func FilterV2Advert(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(io.LimitReader(r, MaxListingBytes+1))
	var out, scratch bytes.Buffer
	payload, special, err := readV2Pkt(br, &scratch)
	if err != nil {
		return nil, err
	}
	if special == pktData && strings.HasPrefix(string(payload), "# service=") {
		out.Write(scratch.Bytes())
		scratch.Reset()
		if _, special, err = readV2Pkt(br, &scratch); err != nil || special != pktFlush {
			return nil, ErrMalformed
		}
		out.Write(scratch.Bytes())
		scratch.Reset()
		if payload, special, err = readV2Pkt(br, &scratch); err != nil {
			return nil, err
		}
	}
	if special != pktData || strings.TrimSuffix(string(payload), "\n") != "version 2" {
		return nil, ErrMalformed
	}
	out.Write(scratch.Bytes())
	for {
		scratch.Reset()
		payload, special, err := readV2Pkt(br, &scratch)
		if err != nil {
			return nil, err
		}
		if special == pktFlush {
			out.WriteString("0000")
			return out.Bytes(), nil
		}
		if special != pktData {
			return nil, ErrMalformed
		}
		key, _, _ := strings.Cut(strings.TrimSuffix(string(payload), "\n"), "=")
		if v2Commands[key] || v2Keys[key] {
			out.Write(scratch.Bytes())
		}
	}
}

// FilterPushAdvert rewrites a receive-pack ref advertisement, the service
// line, a flush, ref lines and a flush: no push-cert capability (see
// StripPushCert), no hidden ref and no ".have" line, which names an object
// of another repository. The capabilities move to the first ref kept, or to a
// "capabilities^{}" line when none is.
func FilterPushAdvert(r io.Reader, v Refs) ([]byte, error) {
	br := bufio.NewReader(io.LimitReader(r, MaxListingBytes+1))
	var out bytes.Buffer
	payload, flush, err := readPkt(br, &out)
	if err != nil || flush || !strings.HasPrefix(string(payload), "# service=") {
		return nil, ErrMalformed
	}
	if _, flush, err = readPkt(br, &out); err != nil || !flush {
		return nil, ErrMalformed
	}
	var refs []string
	caps, zero := "", strings.Repeat("0", 40)
	for first := true; ; first = false {
		var raw bytes.Buffer
		payload, flush, err := readPkt(br, &raw)
		if err != nil {
			return nil, err
		}
		if flush {
			break
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if first {
			i := strings.IndexByte(line, 0)
			if i < 0 {
				return nil, ErrMalformed
			}
			caps, line = withoutPushCert(line[i+1:]), line[:i]
		}
		oid, ref, ok := strings.Cut(line, " ")
		if !ok || !isHexID(oid) || strings.IndexByte(ref, 0) >= 0 {
			return nil, ErrMalformed
		}
		zero = strings.Repeat("0", len(oid))
		if ref != "capabilities^{}" && ref != ".have" && v.Visible(ref) {
			refs = append(refs, line)
		}
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return nil, ErrMalformed
	}
	if len(refs) == 0 {
		refs = []string{zero + " capabilities^{}"}
	}
	for i, line := range refs {
		if i == 0 {
			line += "\x00" + caps
		}
		line += "\n"
		fmt.Fprintf(&out, "%04x%s", len(line)+4, line)
	}
	out.WriteString("0000")
	return out.Bytes(), nil
}

func withoutPushCert(caps string) string {
	kept := []string{}
	for _, c := range strings.Fields(caps) {
		if c != "push-cert" && !strings.HasPrefix(c, "push-cert=") {
			kept = append(kept, c)
		}
	}
	return strings.Join(kept, " ")
}

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

// readV2Pkt reads one pkt-line into raw, telling the protocol v2 special
// packets apart.
func readV2Pkt(r *bufio.Reader, raw *bytes.Buffer) ([]byte, pktKind, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, 0, ErrMalformed
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return nil, 0, ErrMalformed
	}
	raw.Write(hdr[:])
	switch n {
	case 0:
		return nil, pktFlush, nil
	case 1:
		return nil, pktDelim, nil
	case 2:
		return nil, pktResponseEnd, nil
	case 3, 4:
		return nil, 0, ErrMalformed
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, 0, ErrMalformed
	}
	raw.Write(payload)
	return payload, pktData, nil
}
