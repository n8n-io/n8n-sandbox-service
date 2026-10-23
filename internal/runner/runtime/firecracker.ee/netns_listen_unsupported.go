//go:build !linux

package firecracker

import (
	"fmt"
	"net"
)

// listenInNetNS is unavailable outside Linux, like dialContextInNetNS.
func listenInNetNS(string, string, string) (net.Listener, error) {
	return nil, fmt.Errorf("network namespace listening is only supported on linux")
}
