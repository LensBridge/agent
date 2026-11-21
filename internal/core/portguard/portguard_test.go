package portguard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSys struct {
	carrier  string
	conn     string // NetworkManager connection on eth0
	auto     string // the service port's connection.autoconnect
	leases   string
	neigh    string
	commands []string
}

func (f *fakeSys) guard() *Guard {
	return New(Deps{
		ReadFile: func(p string) ([]byte, error) {
			switch p {
			case CarrierPath:
				return []byte(f.carrier + "\n"), nil
			case LeasesPath:
				return []byte(f.leases), nil
			}
			return nil, errors.New("no such file")
		},
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			cmd := strings.Join(append([]string{name}, args...), " ")
			switch {
			case strings.HasPrefix(cmd, "nmcli -g GENERAL.CONNECTION"):
				return f.conn + "\n", nil
			case strings.HasPrefix(cmd, "nmcli -g connection.autoconnect"):
				return f.auto + "\n", nil
			case strings.HasPrefix(cmd, "ip -o neigh"):
				return f.neigh, nil
			}
			f.commands = append(f.commands, cmd)
			return "", nil
		},
	})
}

const (
	phoneLease = "1795240000 aa:bb:cc:00:00:01 10.77.0.42 phone *\n"
	netLeases  = phoneLease +
		"1795240001 aa:bb:cc:00:00:02 10.77.0.43 printer *\n" +
		"1795240002 aa:bb:cc:00:00:03 10.77.0.44 desk-pc *\n"
)

func TestAPhoneIsLeftAlone(t *testing.T) {
	f := &fakeSys{carrier: "1", conn: Conn, auto: "yes", leases: phoneLease,
		neigh: "10.77.0.42 dev eth0 lladdr aa:bb:cc:00:00:01 REACHABLE\n"}
	f.guard().Check(context.Background())
	if len(f.commands) != 0 {
		t.Fatalf("commands = %q", f.commands)
	}
}

func TestANetworkTripsTheGuard(t *testing.T) {
	f := &fakeSys{carrier: "1", conn: Conn, auto: "yes", leases: netLeases}
	f.guard().Check(context.Background())
	want := []string{
		"sudo -n /usr/bin/nmcli connection modify " + Conn + " connection.autoconnect no",
		"sudo -n /usr/bin/nmcli connection down " + Conn,
	}
	if strings.Join(f.commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q", f.commands)
	}
}

// Neighbours count too: a network whose devices never asked the board for a
// lease still shows up in the neighbour table.
func TestNeighboursCount(t *testing.T) {
	f := &fakeSys{carrier: "1", conn: Conn, auto: "yes", leases: phoneLease, neigh: "" +
		"10.0.0.1 dev eth0 lladdr 11:22:33:44:55:66 STALE\n" +
		"10.0.0.7 dev eth0 lladdr 11:22:33:44:55:77 REACHABLE\n" +
		"10.0.0.9 dev eth0  FAILED\n"}
	f.guard().Check(context.Background())
	if len(f.commands) != 2 {
		t.Fatalf("commands = %q", f.commands)
	}
}

func TestAsAClientOnANetworkNothingHappens(t *testing.T) {
	f := &fakeSys{carrier: "1", conn: "musallahboard-lan", auto: "yes", leases: netLeases}
	f.guard().Check(context.Background())
	if len(f.commands) != 0 {
		t.Fatalf("commands = %q", f.commands)
	}
}

func TestUnpluggingRearms(t *testing.T) {
	f := &fakeSys{carrier: "0", auto: "no"}
	f.guard().Check(context.Background())
	if len(f.commands) != 2 || f.commands[0] != "sudo -n /usr/bin/nmcli connection modify "+Conn+" connection.autoconnect yes" ||
		f.commands[1] != "sudo -n /usr/bin/nmcli connection modify "+LANConn+" connection.autoconnect-retries 1" {
		t.Fatalf("commands = %q", f.commands)
	}
	f = &fakeSys{carrier: "0", auto: "yes"}
	f.guard().Check(context.Background())
	if len(f.commands) != 0 {
		t.Fatalf("already armed: commands = %q", f.commands)
	}
}

func TestSeenDevices(t *testing.T) {
	got := SeenDevices(netLeases, "10.77.0.42 dev eth0 lladdr AA:BB:CC:00:00:01 REACHABLE\n10.77.0.50 dev eth0 INCOMPLETE\n")
	if len(got) != 3 || !got["aa:bb:cc:00:00:01"] {
		t.Fatalf("got %v", got)
	}
}
