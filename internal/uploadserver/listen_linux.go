//go:build linux

package uploadserver

import (
	"context"
	"net"
	"syscall"
)

// Listen binds ListenAddr with IP_FREEBIND, so the server is ready before
// NetworkManager has put 10.77.0.1 on eth0 (it only does when a cable is in),
// and keeps working across cable unplug and replug.
func Listen() (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	return lc.Listen(context.Background(), "tcp4", ListenAddr)
}
