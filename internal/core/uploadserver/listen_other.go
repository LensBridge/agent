//go:build !linux

package uploadserver

import "net"

// Listen binds ListenAddr. Only Linux can bind an address that is not up yet.
func Listen() (net.Listener, error) { return net.Listen("tcp4", ListenAddr) }
