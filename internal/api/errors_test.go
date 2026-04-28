package api

import "testing"

func TestSanitizeErrorStripsSandboxPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "merged path",
			in:   "open /var/sandboxes/9d7744a7-7866-4f77-9b63-c4f95c970876/merged/etc/passwd: no such file",
			want: "open /etc/passwd: no such file",
		},
		{
			name: "upper path",
			in:   "stat /var/sandboxes/9d7744a7-7866-4f77-9b63-c4f95c970876/upper/tmp/file: permission denied",
			want: "stat /tmp/file: permission denied",
		},
		{
			name: "work path",
			in:   "rename /var/sandboxes/9d7744a7-7866-4f77-9b63-c4f95c970876/work/work: invalid argument",
			want: "rename /work: invalid argument",
		},
		{
			name: "socket path",
			in:   "timeout waiting for daemon socket at /var/sandboxes/9d7744a7-7866-4f77-9b63-c4f95c970876/socket/daemon.sock",
			want: "timeout waiting for daemon socket at /daemon.sock",
		},
		{
			name: "sandbox prefix replacement",
			in:   "open /sandbox/tmp/out.txt: permission denied",
			want: "open /tmp/out.txt: permission denied",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeError(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeError() = %q, want %q", got, tc.want)
			}
		})
	}
}
