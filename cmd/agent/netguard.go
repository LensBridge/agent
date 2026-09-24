package main

import (
	"fmt"
	"strings"
)

// The service port runs NetworkManager's "shared" mode on eth0: a DHCP and
// DNS server. Started on a real network, it hands out 10.77.0.x addresses to
// every machine there and cuts the board's own connection. These helpers
// decide, from parsed command output so they can be tested without a Pi,
// whether eth0 is already in use.

// ifaceInUse reports whether iface is carrying some connection other than
// ownConn, judging from `nmcli -t -f DEVICE,CONNECTION device` and
// `ip -4 route show default` output. reason is a plain-English why.
func ifaceInUse(iface, ownConn, nmcliDevices, defaultRoutes string) (inUse bool, reason string) {
	if conn := connectionOnDevice(nmcliDevices, iface); conn != "" && conn != ownConn {
		return true, fmt.Sprintf("NetworkManager connection %q is active on it", conn)
	}
	for _, line := range strings.Split(defaultRoutes, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "dev" && f[i+1] == iface {
				return true, "the board's default route (its way to the internet) goes through it"
			}
		}
	}
	return false, ""
}

// connectionOnDevice returns the connection nmcli's terse DEVICE,CONNECTION
// listing shows on iface, or "" if none.
func connectionOnDevice(nmcliDevices, iface string) string {
	for _, line := range strings.Split(nmcliDevices, "\n") {
		dev, conn, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok || dev != iface {
			continue
		}
		conn = unescapeTerse(conn)
		if conn == "--" {
			return ""
		}
		return conn
	}
	return ""
}

// unescapeTerse undoes nmcli -t escaping: "\:" for ":" and "\\" for "\".
func unescapeTerse(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
