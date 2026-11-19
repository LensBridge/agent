// Package netinfo reports the device's own network identity: the IPv4
// addresses it can be reached on, the Wi-Fi network it joined, and its
// hostname.
//
// Two consumers, with deliberately different filtering:
//
//   - telemetry's heartbeat, via IPv4Addrs — reports every non-loopback IPv4
//     the box holds, because the backend wants the raw picture.
//   - the pre-enrollment splash, via Collect — reports only addresses an
//     operator can actually connect to, because that address is printed on
//     the screen as an instruction.
package netinfo

import (
	"context"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Info is what the device knows about its own place on the network.
type Info struct {
	IPv4     []string `json:"ipv4"`
	SSID     string   `json:"ssid,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
}

// Equal reports whether two snapshots describe the same network state. Used to
// keep a polling loop from logging the same address every few seconds.
func (i Info) Equal(o Info) bool {
	return i.SSID == o.SSID && i.Hostname == o.Hostname && slices.Equal(i.IPv4, o.IPv4)
}

// Online reports whether the device holds at least one usable address.
func (i Info) Online() bool { return len(i.IPv4) > 0 }

// Collect gathers a snapshot for display. Every field is best-effort: a
// failure anywhere leaves that field empty rather than failing the call.
func Collect(ctx context.Context) Info {
	info := Info{IPv4: routableIPv4(IPv4Addrs())}
	info.SSID, _ = SSID(ctx)
	info.Hostname, _ = os.Hostname()
	return info
}

// IPv4Addrs returns every non-loopback IPv4 address bound to this host.
func IPv4Addrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var ips []string
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			ips = append(ips, ipnet.IP.String())
		}
	}
	return ips
}

// routableIPv4 drops 169.254.0.0/16. A link-local address means DHCP never
// answered: the interface is "up" and the box has an IP, but nothing on the
// LAN can reach it. Printing one on the enrollment splash would send an
// operator off to SSH into an address that cannot work.
func routableIPv4(ips []string) []string {
	var out []string
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, s)
	}
	return out
}

// ssidTimeout bounds the iwgetid call. It shells out to a wireless-tools
// binary that is missing on ethernet-only boards, so it must never block a
// caller for long.
const ssidTimeout = 2 * time.Second

// SSID returns the joined Wi-Fi network name. Errors when the device has no
// wireless interface, is not associated, or lacks iwgetid — all normal.
func SSID(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, ssidTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "iwgetid", "-r").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
