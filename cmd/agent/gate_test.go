package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGateRequest(t *testing.T) {
	ok := []struct {
		raw  string
		want gateRequest
	}{
		{"status", gateRequest{verb: "status"}},
		{"status --json", gateRequest{verb: "status", json: true}},
		{"  clock  ", gateRequest{verb: "clock"}},
		{"clock-set 1790000000", gateRequest{verb: "clock-set", epoch: 1790000000}},
		{"bundle-install", gateRequest{verb: "bundle-install"}},
		{"app-install", gateRequest{verb: "app-install"}},
		// What clients actually send; the forced command hands it over verbatim.
		{"sudo -n musallahboard-agent gate bundle-install", gateRequest{verb: "bundle-install"}},
		{"sudo -n /usr/bin/musallahboard-agent gate status --json", gateRequest{verb: "status", json: true}},
	}
	for _, tc := range ok {
		got, err := parseGateRequest(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("parseGateRequest(%q) = %+v, %v; want %+v", tc.raw, got, err, tc.want)
		}
	}

	refused := []string{
		"",
		"   ",
		"bash",
		"sh -c id",
		"status; id",
		"status --json; id",
		"status --json extra",
		"clock && reboot",
		"clock-set",
		"clock-set 1790000000 1790000000",
		"clock-set -1790000000",
		"clock-set +1790000000",
		"clock-set 0x6AB0B380",
		"clock-set 1790000000.5",
		"clock-set 0",
		"clock-set 1000000000", // 2001: before the floor
		"clock-set 4102444800", // 2100: at the ceiling
		"clock-set 99999999999999999999",
		"bundle-install /etc/shadow",
		"app-install ../../x",
		"bundle install /tmp/x.zip", // the admin CLI form, not a gate request
		"mode online",
		"internal-sftp",
		"scp -t /tmp/x.zip",
		"sudo -n musallahboard-agent mode online",
		"sudo -n musallahboard-agent gate",
		"sudo musallahboard-agent gate status",
		"sudo -n /tmp/musallahboard-agent gate status",
		"sudo -n musallahboard-agent gate sudo -n musallahboard-agent gate status",
	}
	for _, raw := range refused {
		if got, err := parseGateRequest(raw); err == nil {
			t.Errorf("parseGateRequest(%q) = %+v, want refused", raw, got)
		}
	}
}

func TestSaveLimited(t *testing.T) {
	dir := t.TempDir()

	p := filepath.Join(dir, "ok.zip")
	if err := saveLimited(p, strings.NewReader("12345"), 5); err != nil {
		t.Fatalf("exactly at the limit: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "12345" {
		t.Errorf("saved %q", b)
	}

	if err := saveLimited(filepath.Join(dir, "big.zip"), strings.NewReader("123456"), 5); err == nil {
		t.Error("over the limit: want error")
	}
	if err := saveLimited(filepath.Join(dir, "empty.zip"), strings.NewReader(""), 5); err == nil {
		t.Error("empty upload: want error")
	}
	// O_EXCL: never write through something already at the path.
	if err := saveLimited(p, strings.NewReader("x"), 5); err == nil {
		t.Error("existing file: want error")
	}
}
