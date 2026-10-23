package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"
)

var _ interface {
	ServeEgressProxy(context.Context, func(func(string) (string, bool)) http.Handler) error
	EgressProxyURL(string) (string, bool)
} = (*Runtime)(nil)

// egressHandlerFunc builds the proxy's handler for one listener.
type egressHandlerFunc = func(identify func(remoteAddr string) (string, bool)) http.Handler

// ServeEgressProxy makes the proxy available to egress-proxy sandboxes until
// ctx ends. Each such sandbox gets a listener of its own, inside its slot's
// network namespace on the TAP gateway, opened on every activation: the
// listener alone names the sandbox, nothing a guest sends does.
func (r *Runtime) ServeEgressProxy(ctx context.Context, handler egressHandlerFunc) error {
	r.egressHandler.Store(&handler)
	<-ctx.Done()
	r.egressHandler.Store(nil)
	return nil
}

// EgressProxyURL is the TAP gateway's proxy port; the same in every slot.
func (r *Runtime) EgressProxyURL(sandboxID string) (string, bool) {
	r.mu.Lock()
	state, ok := r.sandboxes[sandboxID]
	proxy := ok && state.egressProxy
	r.mu.Unlock()
	if !proxy {
		return "", false
	}
	return "http://" + net.JoinHostPort(r.egressGatewayIP(), strconv.Itoa(r.config.EgressProxyPort)), true
}

func (r *Runtime) egressGatewayIP() string {
	ip, _, err := net.ParseCIDR(r.config.HostTapIPCIDR)
	if err != nil {
		return ""
	}
	return ip.String()
}

// startEgressListener opens the sandbox's proxy listener in its slot. Called
// after the slot's network is set up and before the guest runs, so its first
// request finds the proxy; closed by teardownRunningVM.
func (r *Runtime) startEgressListener(state *sandboxState) error {
	h := r.egressHandler.Load()
	if h == nil {
		return errors.New("egress proxy is not running")
	}
	addr := net.JoinHostPort(r.egressGatewayIP(), strconv.Itoa(r.config.EgressProxyPort))
	ln, err := r.deps.listenEgress(filepath.Join("/run/netns", state.netnsName), "tcp", addr)
	if err != nil {
		return fmt.Errorf("egress proxy listener: %w", err)
	}
	id := state.id
	srv := &http.Server{
		Handler:           (*h)(func(string) (string, bool) { return id, true }),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			slog.Warn("firecracker egress proxy listener stopped", "sandbox_id", id, "err", err)
		}
	}()
	r.mu.Lock()
	state.egressServer = srv
	r.mu.Unlock()
	return nil
}
