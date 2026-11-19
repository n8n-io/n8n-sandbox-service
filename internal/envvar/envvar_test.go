package envvar

import (
	"math"
	"strings"
	"testing"
	"time"
)

const name = "ENVVAR_TEST_VALUE"

func TestGetTrims(t *testing.T) {
	t.Setenv(name, "  value \n")
	if got := Get(name); got != "value" {
		t.Fatalf("Get = %q, want %q", got, "value")
	}
}

func TestStringKeepsDefaultWhenBlank(t *testing.T) {
	t.Setenv(name, "   ")
	got := "default"
	String(name, &got)
	if got != "default" {
		t.Fatalf("String = %q, want the default", got)
	}

	t.Setenv(name, " set ")
	String(name, &got)
	if got != "set" {
		t.Fatalf("String = %q, want %q", got, "set")
	}
}

func TestReadersKeepDefaultWhenUnset(t *testing.T) {
	t.Setenv(name, "")
	b, n, d := true, int64(7), time.Second
	for _, err := range []error{
		Bool(name, &b),
		PositiveInt(name, &n),
		NonNegativeInt(name, &n),
		IntBetween(name, &n, 1, 10),
		PositiveDuration(name, &d),
		NonNegativeDuration(name, &d),
		DurationBetween(name, &d, time.Second, time.Minute),
	} {
		if err != nil {
			t.Fatalf("unset variable: %v", err)
		}
	}
	if !b || n != 7 || d != time.Second {
		t.Fatalf("defaults changed: %v %d %s", b, n, d)
	}
}

func TestReadersParseTrimmedValues(t *testing.T) {
	t.Setenv(name, " 42 ")
	var n int
	if err := PositiveInt(name, &n); err != nil || n != 42 {
		t.Fatalf("PositiveInt = %d, %v", n, err)
	}

	t.Setenv(name, " 90s ")
	var d time.Duration
	if err := PositiveDuration(name, &d); err != nil || d != 90*time.Second {
		t.Fatalf("PositiveDuration = %s, %v", d, err)
	}

	t.Setenv(name, " false ")
	b := true
	if err := Bool(name, &b); err != nil || b {
		t.Fatalf("Bool = %v, %v", b, err)
	}
}

func TestReadersRefuseBadValues(t *testing.T) {
	cases := []struct {
		value string
		read  func() error
		want  string
	}{
		{"yes please", func() error { var b bool; return Bool(name, &b) }, "a boolean"},
		{"0", func() error { var n int; return PositiveInt(name, &n) }, "a positive integer"},
		{"abc", func() error { var n int64; return PositiveInt(name, &n) }, "a positive integer"},
		{"-1", func() error { var n int; return NonNegativeInt(name, &n) }, "a non-negative integer"},
		{"2147483648", func() error { var n int32; return NonNegativeInt(name, &n) }, "a non-negative integer"},
		{"11", func() error { var n int; return IntBetween(name, &n, 1, 10) }, "an integer between 1 and 10"},
		{"0s", func() error { var d time.Duration; return PositiveDuration(name, &d) }, "a positive duration"},
		{"10", func() error { var d time.Duration; return PositiveDuration(name, &d) }, "a positive duration"},
		{"-1s", func() error { var d time.Duration; return NonNegativeDuration(name, &d) }, "a non-negative duration"},
		{"2m", func() error {
			var d time.Duration
			return DurationBetween(name, &d, time.Second, time.Minute)
		}, "a duration between 1s and 1m0s"},
	}
	for _, tc := range cases {
		t.Setenv(name, tc.value)
		err := tc.read()
		if err == nil {
			t.Fatalf("%q: no error", tc.value)
		}
		if !strings.Contains(err.Error(), name+" must be "+tc.want) {
			t.Fatalf("%q: error %q does not say %q", tc.value, err, tc.want)
		}
	}
}

func TestBadValueLeavesDestination(t *testing.T) {
	t.Setenv(name, "-5")
	n := 3
	if err := PositiveInt(name, &n); err == nil {
		t.Fatal("no error")
	}
	if n != 3 {
		t.Fatalf("destination changed to %d", n)
	}
}

func TestIntBetweenBounds(t *testing.T) {
	for _, v := range []string{"0", "2147483647"} {
		t.Setenv(name, v)
		var n int
		if err := IntBetween(name, &n, 0, math.MaxInt32); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
	}
}
