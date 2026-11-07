package handoff

import (
	"context"
	"net"
	"syscall"
)

// ListenReusePort binds a TCP listener with SO_REUSEPORT set, so that a second
// process can bind the same address while the first is still listening. On
// Linux the kernel then load-balances new connections across all sockets in
// the reuseport group.
func ListenReusePort(ctx context.Context, addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	return lc.Listen(ctx, "tcp", addr)
}
