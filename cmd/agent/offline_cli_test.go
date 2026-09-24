package main

import (
	"testing"

	"github.com/LensBridge/agent/internal/kioskurl"
	"github.com/LensBridge/agent/internal/offline"
)

// A trimmed /proc/net/tcp. 10.77.0.1 is 01004D0A in host (little-endian)
// order; port 22 is 0016.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 2 1 0000000000000000 100 0 0 10 0
   2: 0A00A8C0:0016 6400A8C0:D431 01 00000000:00000000 02:0004E1B2 00000000     0        0 3 4 0000000000000000 20 4 29 10 -1
`

func TestHasEstablishedTCP(t *testing.T) {
	cases := []struct {
		name string
		data string
		ip   string
		want bool
	}{
		{"only a LAN session", procNetTCP, "10.77.0.1", false},
		{"LAN session found on its own address", procNetTCP, "192.168.0.10", true},
		{"service-port session", procNetTCP + "   3: 01004D0A:0016 3A4D000A:E1F2 01 00000000:00000000 02:0004E1B2 00000000     0        0 4 4 0 20 4 29 10 -1\n", "10.77.0.1", true},
		{"service-port socket closing, not established", procNetTCP + "   3: 01004D0A:0016 3A4D000A:E1F2 06 00000000:00000000 00:00000000 00000000     0        0 0 3 0\n", "10.77.0.1", false},
		{"other port on service address", procNetTCP + "   3: 01004D0A:1F90 3A4D000A:E1F2 01 0 0 0 0 0 0 0\n", "10.77.0.1", false},
		{"empty", "", "10.77.0.1", false},
		{"bad ip", procNetTCP, "not-an-ip", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasEstablishedTCP(tc.data, tc.ip, 22); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The kiosk is pointed at the URL kioskurl composes; the server listens where
// internal/offline says. The two packages must not drift apart.
func TestOfflineURLMatchesListenAddr(t *testing.T) {
	if want := "http://" + offline.ListenAddr + "/"; kioskurl.OfflineBaseURL != want {
		t.Errorf("kioskurl.OfflineBaseURL = %q, want %q", kioskurl.OfflineBaseURL, want)
	}
}
