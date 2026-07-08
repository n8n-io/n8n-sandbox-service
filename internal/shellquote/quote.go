// Package shellquote provides POSIX shell single-quoting for script fragments.
package shellquote

import "strings"

// Quote wraps s in single quotes for safe embedding in /bin/sh -c scripts.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
