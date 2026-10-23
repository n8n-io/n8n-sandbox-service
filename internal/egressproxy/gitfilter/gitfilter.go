// Package gitfilter decides which Git smart-HTTP requests a sandbox may make
// through the egress proxy: fetch from listed repositories, push only to
// writable ones and only to allowed refs. Every other request to a Git host is
// refused, because the credential the proxy injects reaches the whole org.
package gitfilter

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Reason says why a request was refused; the proxy reports it as is.
type Reason string

const (
	ReasonPathNotAllowed Reason = "path_not_allowed"
	ReasonRepoNotAllowed Reason = "repo_not_allowed"
	ReasonRefNotAllowed  Reason = "ref_not_allowed"
)

// Op is what an allowed request does.
type Op int

const (
	OpFetch         Op = iota + 1 // info/refs for upload-pack, or git-upload-pack
	OpAdvertisePush               // info/refs for receive-pack
	OpPush                        // git-receive-pack
)

// Repo is one repository the sandbox may use. Path is canonical: lowercase,
// '/'-separated, without ".git".
type Repo struct {
	Path  string
	Write bool
}

// Check classifies a request by its method, escaped path and raw query. It
// returns the repository it is for, or the reason it is refused.
func Check(method, escapedPath, rawQuery string, repos []Repo) (Op, string, Reason) {
	path, ok := canonicalPath(escapedPath)
	if !ok {
		return 0, "", ReasonPathNotAllowed
	}
	var op Op
	var repo string
	switch {
	case method == "GET" && strings.HasSuffix(path, "/info/refs"):
		repo = strings.TrimSuffix(path, "/info/refs")
		switch rawQuery {
		case "service=git-upload-pack":
			op = OpFetch
		case "service=git-receive-pack":
			op = OpAdvertisePush
		default:
			return 0, "", ReasonPathNotAllowed
		}
	case method == "POST" && rawQuery == "" && strings.HasSuffix(path, "/git-upload-pack"):
		repo, op = strings.TrimSuffix(path, "/git-upload-pack"), OpFetch
	case method == "POST" && rawQuery == "" && strings.HasSuffix(path, "/git-receive-pack"):
		repo, op = strings.TrimSuffix(path, "/git-receive-pack"), OpPush
	default:
		return 0, "", ReasonPathNotAllowed
	}
	repo = strings.TrimPrefix(strings.TrimSuffix(repo, ".git"), "/")
	if repo == "" {
		return 0, "", ReasonPathNotAllowed
	}
	for _, r := range repos {
		if r.Path != repo {
			continue
		}
		if op != OpFetch && !r.Write {
			return 0, "", ReasonRepoNotAllowed
		}
		return op, repo, ""
	}
	return 0, "", ReasonRepoNotAllowed
}

// canonicalPath lowercases an escaped request path, ASCII only, and refuses
// any form a Git server could read differently from this filter: escapes,
// empty or dot segments, a trailing '/', and non-ASCII bytes.
func canonicalPath(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") || len(p) > 1024 {
		return "", false
	}
	b := []byte(p)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '/', c == '.', c == '_', c == '-':
		default:
			return "", false
		}
	}
	for _, seg := range strings.Split(string(b[1:]), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return string(b), true
}

// RefAllowed matches a ref against push_refs: an entry ending in '/' is a
// prefix, any other one exact ref.
func RefAllowed(ref string, pushRefs []string) bool {
	for _, p := range pushRefs {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(ref, p) && len(ref) > len(p) {
				return true
			}
		} else if ref == p {
			return true
		}
	}
	return false
}

// Errors from ReadCommands. ErrRefused carries no detail a guest could use.
var (
	ErrMalformed = errors.New("malformed push command list")
	ErrRefused   = errors.New("push updates a ref outside the allowed refs")
)

// Bounds on one push's command list, held in memory while it is checked.
const (
	maxCommands  = 1024
	maxHeadBytes = 1 << 20
)

// ReadCommands reads a receive-pack request's command list from r and returns
// its exact bytes, to be forwarded before the rest of the body. The list must
// be any number of "shallow" lines, then "<old> <new> <ref>" lines, then a
// flush; anything else is ErrMalformed, a push certificate included. Every
// ref must be a valid branch name allowed by pushRefs, or the whole push is
// ErrRefused.
func ReadCommands(r *bufio.Reader, pushRefs []string) ([]byte, error) {
	var head bytes.Buffer
	commands, shallowDone := 0, false
	for {
		payload, flush, err := readPkt(r, &head)
		if err != nil {
			return nil, err
		}
		if head.Len() > maxHeadBytes {
			return nil, ErrMalformed
		}
		if flush {
			if commands == 0 {
				return nil, ErrMalformed
			}
			return head.Bytes(), nil
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if commands == 0 {
			// Capabilities ride on the first command after a NUL.
			if i := strings.IndexByte(line, 0); i >= 0 {
				line = line[:i]
			}
		}
		if strings.HasPrefix(line, "shallow ") {
			if shallowDone || !isHexID(strings.TrimPrefix(line, "shallow ")) {
				return nil, ErrMalformed
			}
			continue
		}
		shallowDone = true
		parts := strings.Split(line, " ")
		if len(parts) != 3 || !isHexID(parts[0]) || !isHexID(parts[1]) || len(parts[0]) != len(parts[1]) {
			return nil, ErrMalformed
		}
		if !validBranchRef(parts[2]) || !RefAllowed(parts[2], pushRefs) {
			return nil, ErrRefused
		}
		if commands++; commands > maxCommands {
			return nil, ErrMalformed
		}
	}
}

// readPkt reads one pkt-line into head and returns its payload. Special
// packets other than a flush are refused.
func readPkt(r *bufio.Reader, head *bytes.Buffer) ([]byte, bool, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, false, ErrMalformed
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return nil, false, ErrMalformed
	}
	head.Write(hdr[:])
	if n == 0 {
		return nil, true, nil
	}
	if n < 5 {
		return nil, false, ErrMalformed // 0001, 0002, 0003 and the empty 0004
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, false, ErrMalformed
	}
	head.Write(payload)
	return payload, false, nil
}

func isHexID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validBranchRef applies git check-ref-format to a branch ref.
func validBranchRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/heads/") || len(ref) > 255 || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return false
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") || ref == "refs/heads/@" {
		return false
	}
	for _, c := range ref {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return false
		}
	}
	for _, seg := range strings.Split(ref, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
	}
	return true
}

// StripPushCert rewrites a receive-pack ref advertisement so that it offers no
// push-cert capability: a client that is not offered it sends no certificate,
// whose commands ReadCommands could not see. The advertisement is the service
// line, a flush, then refs with capabilities after the first ref's NUL.
func StripPushCert(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	var out bytes.Buffer
	for flushes := 0; flushes < 2; {
		var raw bytes.Buffer
		payload, flush, err := readPkt(br, &raw)
		if err != nil {
			return nil, fmt.Errorf("receive-pack advertisement: %w", err)
		}
		if flush || !bytes.Contains(payload, []byte{0}) {
			out.Write(raw.Bytes())
			if flush {
				flushes++
			}
			continue
		}
		nul := bytes.IndexByte(payload, 0)
		caps := strings.Fields(strings.TrimSuffix(string(payload[nul+1:]), "\n"))
		kept := caps[:0]
		for _, c := range caps {
			if c != "push-cert" && !strings.HasPrefix(c, "push-cert=") {
				kept = append(kept, c)
			}
		}
		line := string(payload[:nul+1]) + strings.Join(kept, " ") + "\n"
		fmt.Fprintf(&out, "%04x%s", len(line)+4, line)
		break
	}
	return io.MultiReader(&out, br), nil
}
