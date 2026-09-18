package runtime

import "testing"

func TestBlockEgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts *CreateOptions
		want bool
	}{
		{name: "nil", opts: nil, want: false},
		{name: "empty", opts: &CreateOptions{}, want: false},
		{name: "public", opts: &CreateOptions{Egress: EgressPublic}, want: false},
		{name: "none", opts: &CreateOptions{Egress: EgressNone}, want: true},
	} {
		if got := tc.opts.BlockEgress(); got != tc.want {
			t.Errorf("%s: BlockEgress() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A value ParseEgress would have refused cannot be read as a policy.
func TestBlockEgressPanicsOnUnvalidatedValue(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("BlockEgress() did not panic")
		}
	}()
	(&CreateOptions{Egress: "allow-all"}).BlockEgress()
}
