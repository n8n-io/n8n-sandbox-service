//go:build !linux

package firecracker

import (
	"context"
	"fmt"
	"net"
)

// dialContextInNetNS is unavailable outside Linux because setns and network
// namespaces are Linux-specific.
func dialContextInNetNS(context.Context, string, string, string) (net.Conn, error) {
	return nil, fmt.Errorf("network namespace dialing is only supported on linux")
}
