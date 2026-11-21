// Package portguard keeps the ethernet service port from serving addresses on
// a real network (docs/architecture.md, section 13).
//
// The service port is NetworkManager's shared mode on eth0: a DHCP server, at
// the lowest autoconnect priority, tried only after the DHCP client profile
// got no answer. That is what makes "plug a phone or laptop in" work with no
// switch to flip. It also means a board on a building network whose DHCP
// server is slow or down could start handing out 10.77.0.x addresses there.
//
// A phone or laptop is one device on the cable; a network is many. While the
// service port is up, the guard counts the devices it can see on eth0 (the
// leases the board handed out, and eth0's neighbour table). At Devices or
// more it switches the service port's autoconnect off and takes it down, so
// eth0 goes back to being a DHCP client. When the cable is unplugged, it
// switches autoconnect back on, ready for the next phone.
//
// It keeps no state of its own: whether it is tripped is NetworkManager's
// autoconnect setting, so an agent restart while tripped neither loses the
// trip nor leaves the service port off for good.
package portguard

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Names shared with setup.sh and `musallahboard-agent service-port`.
const (
	Conn = "musallahboard-service-port"
	// LANConn is eth0's DHCP client profile, tried first.
	LANConn = "musallahboard-lan"
	Iface   = "eth0"
	// Devices seen on eth0 that mean a network, not a phone or laptop.
	Devices = 3
	// Interval between checks.
	Interval = 15 * time.Second
	// LeasesPath is where NetworkManager's dnsmasq keeps eth0's leases.
	LeasesPath = "/var/lib/NetworkManager/dnsmasq-" + Iface + ".leases"
	// CarrierPath says whether a cable is plugged in ("1").
	CarrierPath = "/sys/class/net/" + Iface + "/carrier"
	// NMCLI is the path the sudoers allow-list names.
	NMCLI = "/usr/bin/nmcli"
)

// Deps are the guard's view of the system, replaced in tests.
type Deps struct {
	Logger *slog.Logger
	// Run runs a command and returns its combined output.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// ReadFile reads a file (leases, carrier).
	ReadFile func(path string) ([]byte, error)
	Interval time.Duration
}

// Guard watches the service port.
type Guard struct{ d Deps }

// New returns a Guard; nil fields of d get the real system.
func New(d Deps) *Guard {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.ReadFile == nil {
		d.ReadFile = os.ReadFile
	}
	if d.Interval == 0 {
		d.Interval = Interval
	}
	return &Guard{d: d}
}

// Run checks every Interval until ctx ends.
func (g *Guard) Run(ctx context.Context) {
	t := time.NewTicker(g.d.Interval)
	defer t.Stop()
	for {
		g.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check runs one round: re-arm with no cable, trip on a network.
func (g *Guard) Check(ctx context.Context) {
	carrier, err := g.d.ReadFile(CarrierPath)
	if err != nil {
		return // no eth0
	}
	if strings.TrimSpace(string(carrier)) != "1" {
		g.rearm(ctx)
		return
	}
	conn, err := g.d.Run(ctx, "nmcli", "-g", "GENERAL.CONNECTION", "device", "show", Iface)
	if err != nil || strings.TrimSpace(conn) != Conn {
		return
	}
	leases, _ := g.d.ReadFile(LeasesPath)
	neigh, _ := g.d.Run(ctx, "ip", "-o", "neigh", "show", "dev", Iface)
	macs := SeenDevices(string(leases), neigh)
	if len(macs) < Devices {
		return
	}
	g.d.Logger.Warn("service port: eth0 looks like a network, not a phone or laptop, so the board stopped handing out addresses there; "+
		"it is a DHCP client on eth0 again until the cable is unplugged",
		"devices", len(macs), "threshold", Devices)
	if out, err := g.sudoNM(ctx, "connection", "modify", Conn, "connection.autoconnect", "no"); err != nil {
		g.d.Logger.Error("service port: could not switch its autoconnect off", "err", err, "out", firstLine(out))
	}
	if out, err := g.sudoNM(ctx, "connection", "down", Conn); err != nil {
		g.d.Logger.Error("service port: could not take it down", "err", err, "out", firstLine(out))
	}
}

// rearm switches the service port's autoconnect back on, with no cable
// plugged in, if the guard (or anything else) left it off.
func (g *Guard) rearm(ctx context.Context) {
	auto, err := g.d.Run(ctx, "nmcli", "-g", "connection.autoconnect", "connection", "show", Conn)
	if err != nil || strings.TrimSpace(auto) != "no" {
		return
	}
	if out, err := g.sudoNM(ctx, "connection", "modify", Conn, "connection.autoconnect", "yes"); err != nil {
		g.d.Logger.Error("service port: could not switch its autoconnect back on", "err", err, "out", firstLine(out))
		return
	}
	// As `service-port on` does: one DHCP attempt, so a phone gets an
	// address in about 30 s rather than after NetworkManager's four.
	if out, err := g.sudoNM(ctx, "connection", "modify", LANConn, "connection.autoconnect-retries", "1"); err != nil {
		g.d.Logger.Warn("service port: could not set the DHCP client to one attempt; a phone may wait a few minutes for an address",
			"err", err, "out", firstLine(out))
	}
	g.d.Logger.Info("service port: ready again; plug a phone or laptop into eth0")
}

// sudoNM runs nmcli as root through the sudoers allow-list
// (packaging/sudoers.d), which names exactly these argument sets.
func (g *Guard) sudoNM(ctx context.Context, args ...string) (string, error) {
	return g.d.Run(ctx, "sudo", append([]string{"-n", NMCLI}, args...)...)
}

// SeenDevices is the set of hardware addresses in dnsmasq's leases file and
// in `ip -o neigh show dev eth0` output, leaving out neighbours that never
// answered.
func SeenDevices(leases, neigh string) map[string]bool {
	macs := map[string]bool{}
	for _, line := range strings.Split(leases, "\n") {
		// expiry mac ip hostname client-id
		if f := strings.Fields(line); len(f) >= 3 && strings.Count(f[1], ":") == 5 {
			macs[strings.ToLower(f[1])] = true
		}
	}
	for _, line := range strings.Split(neigh, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "lladdr" && !strings.Contains(line, "FAILED") && !strings.Contains(line, "INCOMPLETE") {
				macs[strings.ToLower(f[i+1])] = true
			}
		}
	}
	return macs
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
