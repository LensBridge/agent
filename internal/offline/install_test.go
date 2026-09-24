package offline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallBundleSwapsCurrent(t *testing.T) {
	requireSymlinks(t)
	p := testPaths(t)
	src := t.TempDir()

	res, err := InstallBundle(p, newBundle("2026-09-24", 14, "2026-09-24T14:02:11Z").write(t, src), testDevice)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "20260924T140211Z" || res.Previous != "" {
		t.Errorf("result = %+v", res)
	}
	dir, m, err := CurrentBundle(p)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != res.Name || m.LastDay != "2026-10-07" {
		t.Errorf("current = %s, lastDay %s", dir, m.LastDay)
	}
	// The link is relative, so the offline root can move.
	if target, _ := os.Readlink(p.Current()); target != filepath.Join("bundles", res.Name) {
		t.Errorf("current -> %q", target)
	}
	// No staging directory or temp link left behind.
	assertOnlyUnder(t, p.Bundles(), res.Name)
	if _, err := os.Lstat(p.Current() + ".tmp"); !os.IsNotExist(err) {
		t.Error("current.tmp left behind")
	}
}

// A failed install must leave the served bundle exactly as it was.
func TestFailedInstallLeavesCurrentUntouched(t *testing.T) {
	requireSymlinks(t)
	p := testPaths(t)
	src := t.TempDir()

	good, err := InstallBundle(p, newBundle("2026-09-24", 3, "2026-09-24T14:02:11Z").write(t, src), testDevice)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Readlink(p.Current())

	failures := map[string]func(*bundleSpec){
		"hash mismatch": func(b *bundleSpec) { b.media["media/"+sha(testImage)+".jpg"] = []byte("tampered bytes!!!!!!!!!") },
		"wrong device":  func(b *bundleSpec) { b.manifest.DeviceID = "other" },
		"zip slip":      addEntry("../../escape.json"),
		"missing day":   func(b *bundleSpec) { delete(b.payloads, "2026-10-02") },
	}
	for name, mutate := range failures {
		t.Run(name, func(t *testing.T) {
			b := newBundle("2026-10-01", 3, "2026-10-01T09:00:00Z")
			mutate(b)
			if _, err := InstallBundle(p, b.write(t, src), testDevice); err == nil {
				t.Fatal("install succeeded")
			}
			after, _ := os.Readlink(p.Current())
			if after != before {
				t.Errorf("current moved from %q to %q", before, after)
			}
			// The staging directory is gone too; only the good bundle remains.
			assertOnlyUnder(t, p.Bundles(), good.Name)
			if _, err := os.Stat(filepath.Join(filepath.Dir(p.Root), "escape.json")); !os.IsNotExist(err) {
				t.Error("zip-slip entry was written")
			}
		})
	}
}

func TestInstallPrunesToCurrentAndPrevious(t *testing.T) {
	requireSymlinks(t)
	p := testPaths(t)
	src := t.TempDir()

	gens := []string{"2026-09-01T10:00:00Z", "2026-09-10T10:00:00Z", "2026-09-20T10:00:00Z", "2026-09-30T10:00:00Z"}
	var last *InstallResult
	for _, g := range gens {
		res, err := InstallBundle(p, newBundle(g[:10], 7, g).write(t, src), testDevice)
		if err != nil {
			t.Fatal(err)
		}
		last = res
	}
	assertOnlyUnder(t, p.Bundles(), "20260920T100000Z", "20260930T100000Z")
	if last.Previous != "20260920T100000Z" {
		t.Errorf("previous = %q", last.Previous)
	}
	if strings.Join(last.Removed, ",") != "20260910T100000Z" {
		t.Errorf("removed = %v", last.Removed)
	}

	// Reinstalling the same bundle gets a distinct directory rather than
	// clobbering the one being served.
	res, err := InstallBundle(p, newBundle("2026-09-30", 7, gens[3]).write(t, src), testDevice)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "20260930T100000Z.2" {
		t.Errorf("reinstall name = %q", res.Name)
	}
	assertOnlyUnder(t, p.Bundles(), "20260930T100000Z", "20260930T100000Z.2")
}

// Stale staging directories from an install that died mid-way are cleaned
// up by the next successful one.
func TestInstallRemovesStaleStaging(t *testing.T) {
	requireSymlinks(t)
	p := testPaths(t)
	if err := os.MkdirAll(filepath.Join(p.Bundles(), ".staging-dead", "payloads"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := InstallBundle(p, newBundle("2026-09-24", 2, "2026-09-24T00:00:00Z").write(t, t.TempDir()), testDevice)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyUnder(t, p.Bundles(), res.Name)
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c", ".staging-x"} {
		if err := os.Mkdir(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := prune(dir, "c", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(removed, ",") != ".staging-x,b" {
		t.Errorf("removed = %v", removed)
	}
	assertOnlyUnder(t, dir, "a", "c")
}

func TestCurrentBundleNoneInstalled(t *testing.T) {
	if _, _, err := CurrentBundle(testPaths(t)); err != ErrNoBundle {
		t.Errorf("err = %v, want ErrNoBundle", err)
	}
}
