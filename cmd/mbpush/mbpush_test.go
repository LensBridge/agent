package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompareClocks(t *testing.T) {
	base := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	epochOffset := time.Unix(1, 0).Sub(base)
	cases := []struct {
		name      string
		rtt       time.Duration // round trip of the `date +%s` call
		boardAt   time.Duration // board's reading, relative to base
		wantDrift time.Duration
		wantSet   bool
	}{
		{"in step", 0, 0, 0, false},
		{"5 s behind is tolerated", 0, -5 * time.Second, -5 * time.Second, false},
		{"5 s ahead is tolerated", 0, 5 * time.Second, 5 * time.Second, false},
		{"6 s behind is set", 0, -6 * time.Second, -6 * time.Second, true},
		{"6 s ahead is set", 0, 6 * time.Second, 6 * time.Second, true},
		{"days behind (no RTC, power cut)", 0, -3 * 24 * time.Hour, -3 * 24 * time.Hour, true},
		{"board near the epoch", 0, epochOffset, epochOffset, true},
		// A slow link: the laptop's time is the midpoint of the call, so a
		// board that answered in step is not mistaken for 4 s behind.
		{"slow link, in step", 8 * time.Second, 4 * time.Second, 0, false},
		{"slow link, behind", 8 * time.Second, -3 * time.Second, -7 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compareClocks(base, base.Add(tc.rtt), base.Add(tc.boardAt).Unix())
			if c.Drift != tc.wantDrift || c.NeedsSet != tc.wantSet {
				t.Errorf("got drift %v set %v, want %v %v", c.Drift, c.NeedsSet, tc.wantDrift, tc.wantSet)
			}
		})
	}
}

// Sub-second noise on the laptop side must not tip a 5 s drift over.
func TestCompareClocksTruncatesLaptopTime(t *testing.T) {
	sent := time.Date(2026, 9, 24, 14, 0, 0, 900_000_000, time.UTC)
	c := compareClocks(sent, sent, sent.Unix()-5)
	if c.NeedsSet || c.Drift != -5*time.Second {
		t.Errorf("got %+v", c)
	}
}

func TestParseEpoch(t *testing.T) {
	if n, err := parseEpoch(" 1790000000\n"); err != nil || n != 1790000000 {
		t.Errorf("got %d, %v", n, err)
	}
	for _, bad := range []string{"", "sudo: a password is required", "-5", "12.5"} {
		if _, err := parseEpoch(bad); err == nil {
			t.Errorf("parseEpoch(%q) accepted", bad)
		}
	}
}

func TestDescribeDrift(t *testing.T) {
	cases := map[time.Duration]string{
		0:                      "in step with this laptop",
		400 * time.Millisecond: "in step with this laptop",
		-3 * time.Second:       "3 s behind this laptop",
		7 * time.Second:        "7 s ahead of this laptop",
		-10 * time.Minute:      "10 min behind this laptop",
		-5 * time.Hour:         "5 h behind this laptop",
		-72 * time.Hour:        "3 days behind this laptop",
	}
	for d, want := range cases {
		if got := describeDrift(d); got != want {
			t.Errorf("describeDrift(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    options
		wantErr bool
	}{
		{"bundle, defaults", []string{"b.zip"}, options{host: defaultHost, user: defaultUser, args: []string{"b.zip"}}, false},
		{"flags after the bundle", []string{"b.zip", "--user", "admin", "-i", "k"}, options{host: defaultHost, user: "admin", identity: "k", args: []string{"b.zip"}}, false},
		{"flags before", []string{"--host=10.0.0.5", "status"}, options{host: "10.0.0.5", user: defaultUser, args: []string{"status"}}, false},
		{"app", []string{"--app", "dist"}, options{host: defaultHost, user: defaultUser, app: "dist"}, false},
		{"app plus bundle", []string{"--app", "dist", "b.zip"}, options{}, true},
		{"nothing", nil, options{}, true},
		{"two bundles", []string{"a.zip", "b.zip"}, options{}, true},
		{"unknown flag", []string{"--hots", "x", "a.zip"}, options{}, true},
		{"empty user", []string{"--user=", "a.zip"}, options{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted %v", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.host != tc.want.host || got.user != tc.want.user || got.identity != tc.want.identity ||
				got.app != tc.want.app || !slices.Equal(got.args, tc.want.args) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The exact text matters: the agent's gate strips this prefix back off
// SSH_ORIGINAL_COMMAND (cmd/agent/gate.go, parseGateRequest).
func TestGateRequest(t *testing.T) {
	cases := map[string]string{
		"clock":                "sudo -n musallahboard-agent gate clock",
		"clock-set 1790000000": "sudo -n musallahboard-agent gate clock-set 1790000000",
		"status":               "sudo -n musallahboard-agent gate status",
		"bundle-install":       "sudo -n musallahboard-agent gate bundle-install",
		"app-install":          "sudo -n musallahboard-agent gate app-install",
	}
	for in, want := range cases {
		if got := gate(in); got != want {
			t.Errorf("gate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSHOptions(t *testing.T) {
	r := remote{host: defaultHost, user: "ibra", identity: "key"}
	args := strings.Join(r.sshArgs("date +%s"), " ")
	for _, want := range []string{"-i key", "StrictHostKeyChecking=no", "UserKnownHostsFile=", "ibra@10.77.0.1 date +%s"} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args %q missing %q", args, want)
		}
	}
	r.checkHostKey = true
	if args := strings.Join(r.sshArgs("x"), " "); strings.Contains(args, "StrictHostKeyChecking") {
		t.Errorf("--check-host-key still disables checking: %s", args)
	}
}

func TestTarGzDir(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("js"), 0o644))

	p, err := tarGzDir(dir)
	must(err)
	defer os.Remove(p)

	f, err := os.Open(p)
	must(err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	must(err)
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must(err)
		names = append(names, h.Name)
	}
	slices.Sort(names)
	if want := []string{"assets/", "assets/app.js", "index.html"}; !slices.Equal(names, want) {
		t.Errorf("tar entries = %v, want %v", names, want)
	}
}
