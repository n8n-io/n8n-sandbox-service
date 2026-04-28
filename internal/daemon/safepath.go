package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrSymlink is returned when a path component is a symbolic link.
var ErrSymlink = errors.New("path component is a symbolic link")

// ErrPathEscape is returned when the resolved path escapes the base directory.
var ErrPathEscape = errors.New("path escapes base directory")

// SafeResolve takes a base directory and a user-supplied path, and returns the
// absolute path of the target after verifying:
//
//  1. The resolved path does not escape base.
//  2. No component of the path (relative to base) is a symbolic link.
//
// It is designed to be unit-testable on any OS (all filesystem calls are
// isolated to EvalSymlinks and Lstat, which callers can substitute in tests
// via the package-level hooks below).
func SafeResolve(base, userPath string) (string, error) {
	// Resolve base to an absolute, clean path without following symlinks in
	// its components — we trust base to already be a real directory.
	base = filepath.Clean(base)

	// Clean and join the user path to produce a candidate absolute path.
	// filepath.Join already calls filepath.Clean internally.
	candidate := filepath.Join(base, filepath.FromSlash(userPath))

	// Ensure the candidate starts with base (pre-symlink-check sanity guard).
	// When base is the filesystem root "/", the separator is already present.
	basePrefix := base + string(filepath.Separator)
	if base == "/" {
		basePrefix = "/"
	}
	if !strings.HasPrefix(candidate, basePrefix) && candidate != base {
		return "", ErrPathEscape
	}

	// Walk each path component from base down to candidate, checking for
	// symlinks at every step.
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return "", fmt.Errorf("computing relative path: %w", err)
	}

	// A clean relative path of "." means userPath resolves to base itself,
	// which is fine.
	if rel == "." {
		return candidate, nil
	}

	parts := strings.Split(rel, string(filepath.Separator))
	current := base
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)

		info, err := lstatFn(current)
		if err != nil {
			if os.IsNotExist(err) {
				// Path does not exist yet — that's acceptable for writes.
				// Once the first missing component is found, subsequent
				// components cannot be symlinks either, so we stop here.
				break
			}
			return "", fmt.Errorf("stat %s: %w", current, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s", ErrSymlink, current)
		}
	}

	// After walking, resolve the final path via EvalSymlinks to obtain the
	// canonical real path — but only if the path actually exists.
	resolved, err := evalSymlinksFn(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			// Path doesn't exist yet; use the clean candidate directly.
			resolved = candidate
		} else {
			return "", fmt.Errorf("eval symlinks %s: %w", candidate, err)
		}
	}

	// Final escape check against the canonical base path.
	resolvedBase, err := evalSymlinksFn(base)
	if err != nil {
		resolvedBase = base
	}
	resolvedBase = filepath.Clean(resolvedBase)
	resolved = filepath.Clean(resolved)

	resolvedPrefix := resolvedBase + string(filepath.Separator)
	if resolvedBase == "/" {
		resolvedPrefix = "/"
	}
	if !strings.HasPrefix(resolved, resolvedPrefix) && resolved != resolvedBase {
		return "", ErrPathEscape
	}

	return resolved, nil
}

// lstatFn and evalSymlinksFn are package-level variables so that tests can
// substitute fake implementations without touching the real filesystem.
var lstatFn = os.Lstat
var evalSymlinksFn = filepath.EvalSymlinks
