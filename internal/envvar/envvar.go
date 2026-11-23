// Package envvar reads configuration from environment variables. Values are
// trimmed of surrounding whitespace, an unset or blank variable leaves the
// destination at its default, and a value that does not parse is an error
// that names the variable.
package envvar

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

var errInvalidValue = errors.New("invalid value")

// Integer is the set of integer types the integer readers fill.
type Integer interface {
	~int | ~int32 | ~int64
}

// Get returns the trimmed value of the variable, or "" when it is unset.
func Get(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// String sets dst to the trimmed value when the variable is set.
func String(name string, dst *string) {
	if v := Get(name); v != "" {
		*dst = v
	}
}

// Bool reads a value strconv.ParseBool accepts.
func Bool(name string, dst *bool) error {
	return read(name, dst, strconv.ParseBool, func(bool) bool { return true }, "a boolean")
}

// PositiveInt reads an integer above 0.
func PositiveInt[T Integer](name string, dst *T) error {
	return read(name, dst, parseInt[T], func(n T) bool { return n > 0 }, "a positive integer")
}

// NonNegativeInt reads an integer of 0 or more.
func NonNegativeInt[T Integer](name string, dst *T) error {
	return read(name, dst, parseInt[T], func(n T) bool { return n >= 0 }, "a non-negative integer")
}

// IntBetween reads an integer from lo to hi, both included.
func IntBetween[T Integer](name string, dst *T, lo, hi T) error {
	return read(name, dst, parseInt[T], func(n T) bool { return n >= lo && n <= hi },
		fmt.Sprintf("an integer between %d and %d", lo, hi))
}

// PositiveDuration reads a time.ParseDuration value above 0.
func PositiveDuration(name string, dst *time.Duration) error {
	return read(name, dst, time.ParseDuration, func(d time.Duration) bool { return d > 0 }, "a positive duration")
}

// NonNegativeDuration reads a time.ParseDuration value of 0 or more.
func NonNegativeDuration(name string, dst *time.Duration) error {
	return read(name, dst, time.ParseDuration, func(d time.Duration) bool { return d >= 0 }, "a non-negative duration")
}

// DurationBetween reads a time.ParseDuration value from lo to hi, both included.
func DurationBetween(name string, dst *time.Duration, lo, hi time.Duration) error {
	return read(name, dst, time.ParseDuration, func(d time.Duration) bool { return d >= lo && d <= hi },
		fmt.Sprintf("a duration between %s and %s", lo, hi))
}

func read[T any](name string, dst *T, parse func(string) (T, error), valid func(T) bool, want string) error {
	v := Get(name)
	if v == "" {
		return nil
	}
	parsed, err := parse(v)
	if err != nil || !valid(parsed) {
		return fmt.Errorf("%w: %s must be %s, got %q", errInvalidValue, name, want, v)
	}
	*dst = parsed
	return nil
}

// parseInt refuses a value that does not fit T rather than wrapping it.
func parseInt[T Integer](v string) (T, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	if int64(T(n)) != n {
		return 0, strconv.ErrRange
	}
	return T(n), nil
}
