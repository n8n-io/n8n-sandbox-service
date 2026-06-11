package firecracker

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"time"
)

// tcpDaemonProxy forwards host-local TCP connections into a sandbox network namespace.
type tcpDaemonProxy struct {
	listener net.Listener
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	dial     netnsDialFunc
}

type netnsDialFunc func(ctx context.Context, netnsPath string, network string, address string) (net.Conn, error)

// daemonProxyDialTimeout bounds guest daemon dials so VM teardown cannot leave
// proxy handlers blocked indefinitely.
const daemonProxyDialTimeout = 5 * time.Second

// startDaemonProxy exposes one sandbox daemon on a host-local TCP port. Each
// accepted connection is forwarded from inside the sandbox's network namespace.
func startDaemonProxy(ctx context.Context, listenAddr string, netnsName string, guestAddr string) (daemonProxy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	proxyCtx, cancel := context.WithCancel(context.Background())
	proxy := &tcpDaemonProxy{
		listener: listener,
		done:     make(chan struct{}),
		ctx:      proxyCtx,
		cancel:   cancel,
		dial:     dialContextInNetNS,
	}
	go proxy.serve(netnsName, guestAddr)
	return proxy, nil
}

// serve accepts host-side proxy connections until Stop closes the listener.
func (p *tcpDaemonProxy) serve(netnsName string, guestAddr string) {
	defer close(p.done)
	for {
		p.wg.Add(1)
		clientConn, err := p.listener.Accept()
		if err != nil {
			p.wg.Done()
			return
		}
		slog.Debug("firecracker daemon proxy accepted connection", "local_addr", clientConn.LocalAddr().String(), "remote_addr", clientConn.RemoteAddr().String(), "netns", netnsName, "guest_addr", guestAddr)
		go func() {
			defer p.wg.Done()
			p.handle(clientConn, netnsName, guestAddr)
		}()
	}
}

// handle bridges one host connection to the guest daemon from inside the
// sandbox network namespace.
func (p *tcpDaemonProxy) handle(clientConn net.Conn, netnsName string, guestAddr string) {
	defer clientConn.Close()
	dialCtx, cancelDial := context.WithTimeout(p.ctx, daemonProxyDialTimeout)
	defer cancelDial()
	guestConn, err := p.dial(dialCtx, filepath.Join("/run/netns", netnsName), "tcp", guestAddr)
	if err != nil {
		slog.Warn("firecracker daemon proxy guest dial failed", "netns", netnsName, "guest_addr", guestAddr, "client_remote_addr", clientConn.RemoteAddr().String(), "err", err)
		return
	}
	defer guestConn.Close()

	handleDone := make(chan struct{})
	defer close(handleDone)
	go func() {
		select {
		case <-p.ctx.Done():
			_ = clientConn.Close()
			_ = guestConn.Close()
		case <-handleDone:
		}
	}()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(guestConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, guestConn)
		done <- struct{}{}
	}()
	<-done
	_ = clientConn.Close()
	_ = guestConn.Close()
	<-done
}

// Stop closes the listener, cancels active dials, and waits for handlers to exit.
func (p *tcpDaemonProxy) Stop() error {
	p.cancel()
	err := p.listener.Close()
	<-p.done
	p.wg.Wait()
	return err
}
