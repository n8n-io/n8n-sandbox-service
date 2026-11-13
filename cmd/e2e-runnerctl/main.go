// Command e2e-runnerctl calls a runner's SandboxControl gRPC for e2e tests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  %[1]s stop <sandbox-id>
  %[1]s build-volume [-entries N -content-bytes N -decompressed-bytes N] [-hold DURATION] <tar> <image>

build-volume has the runner build an ext4 image from the tar and prints the
outcome as one line of JSON. It counts the tar's entries, content and
decompressed length with the volume tar policy, unless all three counts are
given. -hold waits that long between the header and the tar.
`, os.Args[0])
	os.Exit(2)
}

type target struct {
	addr   string
	apiKey string
	tls    *runnerctl.TLS
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "stop":
		if len(os.Args) != 3 {
			usage()
		}
		stop(strings.TrimSpace(os.Args[2]))
	case "build-volume":
		buildVolume(os.Args[2:])
	default:
		usage()
	}
}

func runner() target {
	addr := strings.TrimSpace(os.Getenv("E2E_RUNNER_CONTROL_GRPC_ADDR"))
	apiKey := strings.TrimSpace(os.Getenv("E2E_RUNNER_API_KEY"))
	if addr == "" || apiKey == "" {
		fmt.Fprintln(os.Stderr, "E2E_RUNNER_CONTROL_GRPC_ADDR and E2E_RUNNER_API_KEY must be set")
		os.Exit(2)
	}

	tlsCfg := &runnerctl.TLS{
		CAFile:     os.Getenv("E2E_RUNNER_CONTROL_TLS_CA"),
		CertFile:   os.Getenv("E2E_RUNNER_CONTROL_TLS_CERT"),
		KeyFile:    os.Getenv("E2E_RUNNER_CONTROL_TLS_KEY"),
		ServerName: strings.TrimSpace(os.Getenv("E2E_RUNNER_CONTROL_TLS_SERVER_NAME")),
	}
	if tlsCfg.CAFile == "" || tlsCfg.CertFile == "" || tlsCfg.KeyFile == "" {
		fmt.Fprintln(os.Stderr, "E2E_RUNNER_CONTROL_TLS_CA, _CERT, and _KEY must be set")
		os.Exit(2)
	}
	return target{addr: addr, apiKey: apiKey, tls: tlsCfg}
}

func stop(sandboxID string) {
	if sandboxID == "" {
		fmt.Fprintln(os.Stderr, "sandbox id is required")
		os.Exit(2)
	}
	to := runner()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runnerctl.StopSandbox(ctx, to.addr, to.apiKey, to.tls, sandboxID); err != nil {
		fmt.Fprintf(os.Stderr, "stop sandbox: %v\n", err)
		os.Exit(1)
	}
}

// buildOutcome is what build-volume prints.
type buildOutcome struct {
	// Result is "ok", "failed" (the build ran and failed, see Reason),
	// "refused" (a capacity refusal) or "error".
	Result     string `json:"result"`
	Reason     string `json:"reason,omitempty"`
	ImageBytes int64  `json:"image_bytes,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

func buildVolume(args []string) {
	flags := flag.NewFlagSet("build-volume", flag.ExitOnError)
	entries := flags.Int64("entries", -1, "entry count for the header, instead of counting the tar's")
	contentBytes := flags.Int64("content-bytes", -1, "content bytes for the header, instead of counting the tar's")
	decompressedBytes := flags.Int64("decompressed-bytes", -1, "decompressed length for the header, instead of counting the tar's")
	hold := flags.Duration("hold", 0, "wait between the header and the tar")
	_ = flags.Parse(args)
	counted := *entries < 0
	if flags.NArg() != 2 || (*contentBytes < 0) != counted || (*decompressedBytes < 0) != counted {
		usage()
	}
	tarPath, imagePath := flags.Arg(0), flags.Arg(1)
	to := runner()

	tar, err := os.Open(tarPath)
	exitOn("open tar", err)
	defer tar.Close()
	info, err := tar.Stat()
	exitOn("stat tar", err)
	header := &pb.BuildVolumeHeader{TarBytes: info.Size(), Entries: *entries, ContentBytes: *contentBytes, DecompressedBytes: *decompressedBytes}
	if counted {
		stats, err := safetar.Validate(tar, volume.TarPolicy(math.MaxInt32, 0, 0))
		exitOn("validate tar", err)
		header.Entries, header.ContentBytes, header.DecompressedBytes = int64(stats.Entries), stats.Bytes, stats.DecompressedBytes
		_, err = tar.Seek(0, io.SeekStart)
		exitOn("rewind tar", err)
	}

	image, err := os.OpenFile(imagePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	exitOn("create image", err)
	defer image.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	built, err := runnerctl.BuildVolume(ctx, to.addr, to.apiKey, to.tls, header, &heldReader{reader: tar, hold: *hold}, image)
	if err == nil {
		err = image.Close()
	}

	outcome := buildOutcome{Result: "ok", ImageBytes: built.Bytes, SHA256: built.SHA256}
	if err != nil {
		fmt.Fprintf(os.Stderr, "build volume: %v\n", err)
		outcome = buildOutcome{Result: "error"}
		if reason, failed := runnerctl.BuildFailureReason(err); failed {
			outcome = buildOutcome{Result: "failed", Reason: reason}
		} else if runnerctl.IsCapacityRefusal(err) {
			outcome = buildOutcome{Result: "refused"}
		}
	}
	line, _ := json.Marshal(outcome)
	fmt.Println(string(line))
	if err != nil {
		os.Exit(1)
	}
}

// heldReader waits before its first read: the header is sent by then.
type heldReader struct {
	reader io.Reader
	hold   time.Duration
}

func (h *heldReader) Read(buf []byte) (int, error) {
	if h.hold > 0 {
		time.Sleep(h.hold)
		h.hold = 0
	}
	return h.reader.Read(buf)
}

func exitOn(action string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
		os.Exit(1)
	}
}
