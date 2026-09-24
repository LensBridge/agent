package main

import (
	"strings"
	"testing"
)

func TestIfaceInUse(t *testing.T) {
	const (
		unplugged  = "eth0:\nwlan0:\nlo:lo\n"
		dashed     = "eth0:--\nlo:lo\n"
		onLAN      = "eth0:Wired connection 1\nwlan0:\nlo:lo\n"
		onOurLAN   = "eth0:musallahboard-lan\nlo:lo\n"
		onService  = "eth0:musallahboard-service-port\nlo:lo\n"
		onWifi     = "eth0:\nwlan0:Musallah WiFi\nlo:lo\n"
		escaped    = `eth0:Office\: floor 2` + "\n"
		viaEth0    = "default via 192.168.0.1 dev eth0 proto dhcp src 192.168.0.42 metric 100\n"
		viaWlan0   = "default via 192.168.1.1 dev wlan0 proto dhcp src 192.168.1.9 metric 600\n"
		viaBoth    = viaWlan0 + viaEth0
		viaEth0Sub = "default via 10.0.0.1 dev eth0.20 proto static\n"
	)
	cases := []struct {
		name    string
		devices string
		routes  string
		want    bool
		reason  string
	}{
		{"nothing plugged in", unplugged, "", false, ""},
		{"nmcli shows -- for none", dashed, "", false, ""},
		{"laptop on the service port already", onService, "", false, ""},
		{"Wi-Fi only: eth0 is free", onWifi, viaWlan0, false, ""},
		{"eth0 on a LAN", onLAN, viaEth0, true, `"Wired connection 1"`},
		{"eth0 on our DHCP client profile", onOurLAN, "", true, `"musallahboard-lan"`},
		{"escaped connection name", escaped, "", true, `"Office: floor 2"`},
		// NetworkManager may not know about it (static config, ifupdown),
		// but the routing table does.
		{"default route via eth0 only", unplugged, viaEth0, true, "default route"},
		{"default routes via both", unplugged, viaBoth, true, "default route"},
		{"VLAN sub-interface is not eth0", unplugged, viaEth0Sub, false, ""},
		{"nmcli unavailable, no routes", "", "", false, ""},
		{"CRLF output", "eth0:Wired connection 1\r\n", "", true, "Wired connection 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ifaceInUse("eth0", serviceConn, tc.devices, tc.routes)
			if got != tc.want {
				t.Errorf("inUse = %v (%q), want %v", got, reason, tc.want)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("reason = %q, want containing %q", reason, tc.reason)
			}
		})
	}
}

func TestUnescapeTerse(t *testing.T) {
	cases := map[string]string{
		"plain":           "plain",
		`a\:b`:            "a:b",
		`back\\slash`:     `back\slash`,
		`trailing\`:       `trailing\`,
		`two\:colons\:ok`: "two:colons:ok",
	}
	for in, want := range cases {
		if got := unescapeTerse(in); got != want {
			t.Errorf("unescapeTerse(%q) = %q, want %q", in, got, want)
		}
	}
}
