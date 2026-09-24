package offline

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name     string
	body     string
	typeflag byte
}

func writeTarGz(t *testing.T, entries []tarEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dist.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: e.typeflag}
		switch e.typeflag {
		case 0:
			h.Typeflag = tar.TypeReg
		case tar.TypeDir:
			h.Mode, h.Size = 0o755, 0
		case tar.TypeSymlink:
			h.Linkname, h.Size = e.body, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func readSPA(t *testing.T, p Paths, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.SPA, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallAppFromTarGz(t *testing.T) {
	requireSymlinks(t)
	p := testPaths(t)

	// A tarball of dist/ — index.html one level down.
	src := writeTarGz(t, []tarEntry{
		{name: "dist/", typeflag: tar.TypeDir},
		{name: "dist/index.html", body: "v1"},
		{name: "dist/assets/app.js", body: "js"},
	})
	res, err := InstallApp(p, src)
	if err != nil {
		t.Fatal(err)
	}
	if got := readSPA(t, p, "index.html"); got != "v1" {
		t.Errorf("index.html = %q", got)
	}
	if got := readSPA(t, p, "assets/app.js"); got != "js" {
		t.Errorf("app.js = %q", got)
	}
	if res.Previous != "" {
		t.Errorf("previous = %q", res.Previous)
	}
}

func TestInstallAppFromDirAdoptsPlainDirAndPrunes(t *testing.T) {
	requireSymlinks(t)
	p := testPaths(t)
	// An SPA copied in by hand before app install existed.
	if err := os.MkdirAll(p.SPA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SPA, "index.html"), []byte("hand"), 0o644); err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, v := range []string{"v1", "v2", "v3"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := InstallApp(p, dir)
		if err != nil {
			t.Fatal(err)
		}
		if v == "v1" && res.Previous != "adopted" {
			t.Errorf("first install previous = %q, want adopted", res.Previous)
		}
		if got := readSPA(t, p, "index.html"); got != v {
			t.Errorf("index.html = %q, want %q", got, v)
		}
		names = append(names, res.Name)
	}
	fi, err := os.Lstat(p.SPA)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("SPA dir is not a symlink after install: %v", err)
	}
	got := listDir(t, p.appReleases())
	for _, n := range got {
		if n == "adopted" || n == names[0] || strings.HasPrefix(n, ".staging-") {
			t.Errorf("not pruned: %s (have %v)", n, got)
		}
	}
}

func TestInstallAppRejects(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
		wantErr string
	}{
		{"no index.html", []tarEntry{{name: "dist/app.js", body: "x"}}, "no index.html"},
		{"parent traversal", []tarEntry{{name: "index.html", body: "x"}, {name: "../evil", body: "x"}}, "unsafe name"},
		{"hidden traversal", []tarEntry{{name: "index.html", body: "x"}, {name: "assets/../../evil", body: "x"}}, "unsafe name"},
		{"absolute", []tarEntry{{name: "/etc/cron.d/evil", body: "x"}}, "unsafe name"},
		{"backslash", []tarEntry{{name: `..\evil`, body: "x"}}, "unsafe name"},
		{"symlink", []tarEntry{{name: "index.html", body: "x"}, {name: "leak", body: "/etc/shadow", typeflag: tar.TypeSymlink}}, "link or special"},
		{"two top-level dirs", []tarEntry{{name: "a/index.html", body: "x"}, {name: "b/index.html", body: "y"}}, "no index.html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			_, err := InstallApp(p, writeTarGz(t, tc.entries))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if _, err := os.Lstat(p.SPA); !os.IsNotExist(err) {
				t.Error("SPA path created by a failed install")
			}
			for _, n := range listDir(t, p.appReleases()) {
				if n != ".lock" {
					t.Errorf("left behind in releases: %s", n)
				}
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(p.Root), "evil")); !os.IsNotExist(err) {
				t.Error("traversal entry was written")
			}
		})
	}
}

func TestInstallAppRejectsOtherFiles(t *testing.T) {
	p := testPaths(t)
	f := filepath.Join(t.TempDir(), "dist.zip")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if _, err := InstallApp(p, f); err == nil || !strings.Contains(err.Error(), "neither a directory nor a .tar.gz") {
		t.Errorf("err = %v", err)
	}
}
