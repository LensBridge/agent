package netinfo

import (
	"slices"
	"testing"
)

func TestRoutableIPv4DropsLinkLocal(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "keeps private lan addresses",
			in:   []string{"192.168.1.42", "10.0.0.7", "172.16.3.1"},
			want: []string{"192.168.1.42", "10.0.0.7", "172.16.3.1"},
		},
		{
			// A 169.254.x address means DHCP never answered. Nothing on the LAN
			// can reach it, so it must never reach the enrollment splash.
			name: "drops apipa",
			in:   []string{"169.254.11.9", "192.168.1.42"},
			want: []string{"192.168.1.42"},
		},
		{
			name: "apipa only yields nothing",
			in:   []string{"169.254.11.9"},
			want: nil,
		},
		{
			name: "drops unparseable",
			in:   []string{"not-an-ip", "192.168.1.42"},
			want: []string{"192.168.1.42"},
		},
		{
			name: "empty input",
			in:   nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := routableIPv4(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("routableIPv4(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestInfoEqual(t *testing.T) {
	base := Info{IPv4: []string{"192.168.1.42"}, SSID: "MSA", Hostname: "lobby"}

	tests := []struct {
		name  string
		other Info
		want  bool
	}{
		{"identical", Info{IPv4: []string{"192.168.1.42"}, SSID: "MSA", Hostname: "lobby"}, true},
		{"ip changed", Info{IPv4: []string{"192.168.1.43"}, SSID: "MSA", Hostname: "lobby"}, false},
		{"ip added", Info{IPv4: []string{"192.168.1.42", "10.0.0.7"}, SSID: "MSA", Hostname: "lobby"}, false},
		{"ssid changed", Info{IPv4: []string{"192.168.1.42"}, SSID: "Guest", Hostname: "lobby"}, false},
		{"hostname changed", Info{IPv4: []string{"192.168.1.42"}, SSID: "MSA", Hostname: "hall"}, false},
		{"went offline", Info{SSID: "MSA", Hostname: "lobby"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := base.Equal(tt.other); got != tt.want {
				t.Errorf("Equal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInfoOnline(t *testing.T) {
	if (Info{}).Online() {
		t.Error("empty Info reported online")
	}
	if (Info{IPv4: []string{}}).Online() {
		t.Error("Info with zero addresses reported online")
	}
	if !(Info{IPv4: []string{"192.168.1.42"}}).Online() {
		t.Error("Info with an address reported offline")
	}
}
