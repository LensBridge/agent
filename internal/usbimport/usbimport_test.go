package usbimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/notice"
	"github.com/LensBridge/agent/internal/store"
)

func touch(t *testing.T, p string, size int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(c []Candidate, root string) []string {
	var out []string
	for _, x := range c {
		out = append(out, rel(root, x.Path))
	}
	return out
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a.mbu"), 10)
	touch(t, filepath.Join(root, "B.MBU"), 10)
	touch(t, filepath.Join(root, "notes.txt"), 10)
	touch(t, filepath.Join(root, "._a.mbu"), 10)
	touch(t, filepath.Join(root, "musallahBOARD", "c.mbu"), 10)
	touch(t, filepath.Join(root, "musallahBOARD", "deeper", "d.mbu"), 10)
	touch(t, filepath.Join(root, "other", "e.mbu"), 10)
	os.Symlink(filepath.Join(root, "a.mbu"), filepath.Join(root, "link.mbu"))
	os.Mkdir(filepath.Join(root, "dir.mbu"), 0o755)

	files, notes := Scan(root)
	got := strings.Join(names(files, root), ",")
	if got != "B.MBU,a.mbu,musallahBOARD/c.mbu" {
		t.Fatalf("files = %s", got)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "link.mbu: not a regular file") || !strings.Contains(joined, "dir.mbu: not a regular file") {
		t.Errorf("notes = %q", notes)
	}
}

func TestScanSymlinkedFolderIgnored(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	touch(t, filepath.Join(elsewhere, "x.mbu"), 1)
	os.Symlink(elsewhere, filepath.Join(root, "MusallahBoard"))
	if files, _ := Scan(root); len(files) != 0 {
		t.Fatalf("followed a symlinked folder: %v", files)
	}
}

func TestScanLimits(t *testing.T) {
	root := t.TempDir()
	for i := range MaxFiles + 3 {
		touch(t, filepath.Join(root, fmt.Sprintf("p%02d.mbu", i)), 1)
	}
	files, notes := Scan(root)
	if len(files) != MaxFiles {
		t.Fatalf("%d files, want %d", len(files), MaxFiles)
	}
	if len(notes) != 3 {
		t.Fatalf("notes = %q", notes)
	}

	// A file over the package limit is skipped without reading it (sparse).
	root = t.TempDir()
	big := filepath.Join(root, "big.mbu")
	f, _ := os.Create(big)
	f.Truncate(512<<20 + 1)
	f.Close()
	files, notes = Scan(root)
	if len(files) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "larger than") {
		t.Fatalf("files=%v notes=%q", files, notes)
	}
}

func TestInboxName(t *testing.T) {
	for in, want := range map[string]string{
		"musallahboard-app-2.1.0.mbu": "usb-sda1-1-musallahboard-app-2.1.0.mbu",
		"My Board (copy).MBU":         "usb-sda1-1-My_Board__copy_.mbu",
		"...mbu":                      "usb-sda1-1-package.mbu",
	} {
		if got := inboxName("sda1", 1, in); got != want {
			t.Errorf("inboxName(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeSys struct {
	mu    sync.Mutex
	cmds  []string
	fs    string
	stick map[string]int // files the fake mount puts on the stick
	failU bool
}

func (f *fakeSys) exec(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	f.mu.Unlock()
	switch name {
	case "blkid":
		if f.fs == "" {
			return "", errors.New("exit status 2")
		}
		return f.fs + "\n", nil
	case "mount":
		mp := args[len(args)-1]
		for p, size := range f.stick {
			full := filepath.Join(mp, p)
			os.MkdirAll(filepath.Dir(full), 0o755)
			os.WriteFile(full, make([]byte, size), 0o644)
		}
		return "", nil
	case "umount":
		if f.failU && args[0] != "-l" {
			return "busy", errors.New("exit status 32")
		}
		// Unmounted: the mount point is an empty directory again.
		mp := args[len(args)-1]
		entries, _ := os.ReadDir(mp)
		for _, e := range entries {
			os.RemoveAll(filepath.Join(mp, e.Name()))
		}
		return "", nil
	}
	return "", fmt.Errorf("unexpected command %s", name)
}

func newHelper(t *testing.T, f *fakeSys) (*Helper, *[]string) {
	dir := t.TempDir()
	var log []string
	var mu sync.Mutex
	h := &Helper{
		Layout:      store.Layout{Root: filepath.Join(dir, "state")},
		MountRoot:   filepath.Join(dir, "usb"),
		DevDir:      "/dev",
		Exec:        f.exec,
		Logf:        func(format string, a ...any) { mu.Lock(); log = append(log, fmt.Sprintf(format, a...)); mu.Unlock() },
		WaitTimeout: 2 * time.Second,
		Poll:        10 * time.Millisecond,
	}
	return h, &log
}

// fakeDaemon answers every .mbu in the inbox, as RunInbox would.
func fakeDaemon(ctx context.Context, l store.Layout) {
	for ctx.Err() == nil {
		entries, _ := os.ReadDir(l.Inbox())
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".mbu") {
				continue
			}
			os.MkdirAll(l.InboxResults(), 0o770)
			raw, _ := json.Marshal(importer.InboxResult{Result: importer.Result{File: e.Name(), Action: "installed", Message: "Installed " + e.Name()}})
			os.WriteFile(filepath.Join(l.InboxResults(), e.Name()+".json"), raw, 0o640)
			os.Remove(filepath.Join(l.Inbox(), e.Name()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunCopiesAndWaits(t *testing.T) {
	f := &fakeSys{fs: "vfat", stick: map[string]int{"a.mbu": 5, "MusallahBoard/b.mbu": 7, "readme.txt": 1}}
	h, log := newHelper(t, f)
	// A stale result from an earlier insertion must not be taken as this one's.
	os.MkdirAll(h.Layout.InboxResults(), 0o770)
	os.WriteFile(filepath.Join(h.Layout.InboxResults(), "usb-sdb1-1-MusallahBoard_b.mbu.json"), []byte(`{"action":"rejected"}`), 0o640)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fakeDaemon(ctx, h.Layout)
	if err := h.Run(context.Background(), "sdb1"); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*log, "\n")
	for _, want := range []string{"a.mbu: installed", "b.mbu: installed", "all packages from the stick have been handled"} {
		if !strings.Contains(all, want) {
			t.Errorf("log lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "rejected") {
		t.Errorf("stale result was reported:\n%s", all)
	}
	cmds := strings.Join(f.cmds, "\n")
	if !strings.Contains(cmds, "mount -t vfat -o ro,nosuid,nodev,noexec,noatime /dev/sdb1 "+filepath.Join(h.MountRoot, "sdb1")) {
		t.Errorf("mount command wrong:\n%s", cmds)
	}
	if !strings.Contains(cmds, "umount "+filepath.Join(h.MountRoot, "sdb1")) {
		t.Errorf("not unmounted:\n%s", cmds)
	}
	if _, err := os.Stat(filepath.Join(h.MountRoot, "sdb1")); err == nil {
		t.Error("mount point left behind")
	}
}

func TestRunLazyUnmountFallback(t *testing.T) {
	f := &fakeSys{fs: "exfat", stick: map[string]int{}, failU: true}
	h, _ := newHelper(t, f)
	if err := h.Run(context.Background(), "sdc"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.cmds, "\n"), "umount -l") {
		t.Fatalf("no lazy unmount: %q", f.cmds)
	}
}

func TestRunTimesOutWithoutDaemon(t *testing.T) {
	f := &fakeSys{fs: "ext4", stick: map[string]int{"a.mbu": 1}}
	h, log := newHelper(t, f)
	h.WaitTimeout = 50 * time.Millisecond
	if err := h.Run(context.Background(), "sda"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*log, "\n"), "a.mbu: no result after") {
		t.Fatalf("log = %q", *log)
	}
	// The file was queued under a .mbu name, not left as .part.
	queued, _ := filepath.Glob(filepath.Join(h.Layout.Inbox(), "*.mbu"))
	if len(queued) != 1 || filepath.Base(queued[0]) != "usb-sda-1-a.mbu" {
		t.Fatalf("inbox = %v", queued)
	}
	// The board was told a stick is being read; its outcome is the daemon's.
	if n := announced(t, h, "sda"); n.Headline != "Reading USB stick" {
		t.Fatalf("notice = %+v", n)
	}
}

// announced is the notice the helper left for the daemon about dev.
func announced(t *testing.T, h *Helper, dev string) notice.Notice {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.Layout.Inbox(), "usb-"+dev+importer.NoticeSuffix))
	if err != nil {
		t.Fatalf("no notice for %s: %v", dev, err)
	}
	var n notice.Notice
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunTellsTheBoard(t *testing.T) {
	cases := []struct {
		name, fs string
		stick    map[string]int
		tone     notice.Tone
		headline string
	}{
		{"empty stick", "vfat", map[string]int{}, notice.Neutral, "No updates on this USB stick"},
		{"unknown filesystem", "apfs", map[string]int{}, notice.Problem, "Can't read this USB stick"},
		{"no filesystem", "", map[string]int{}, notice.Problem, "Can't read this USB stick"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSys{fs: tc.fs, stick: tc.stick}
			h, _ := newHelper(t, f)
			_ = h.Run(context.Background(), "sdb1")
			if n := announced(t, h, "sdb1"); n.Tone != tc.tone || n.Headline != tc.headline || len(n.Lines) == 0 {
				t.Fatalf("notice = %+v", n)
			}
		})
	}
}

func TestRunRefusals(t *testing.T) {
	cases := []struct {
		name, dev, fs string
		unsupported   bool
		mounted       string
	}{
		{"bad name", "mmcblk0p1", "vfat", false, ""},
		{"path", "../sda1", "vfat", false, ""},
		{"no filesystem", "sda1", "", true, ""},
		{"iso", "sda1", "iso9660", true, ""},
		{"ntfs goes through ntfs3", "sda1", "ntfs", false, "-t ntfs3"},
		{"exfat", "sda1", "exfat", false, "-t exfat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSys{fs: tc.fs, stick: map[string]int{}}
			h, _ := newHelper(t, f)
			err := h.Run(context.Background(), tc.dev)
			cmds := strings.Join(f.cmds, "\n")
			switch {
			case tc.mounted != "":
				if err != nil || !strings.Contains(cmds, tc.mounted) {
					t.Fatalf("err=%v cmds=%s", err, cmds)
				}
			case tc.unsupported:
				if !errors.Is(err, ErrUnsupported) || strings.Contains(cmds, "mount ") {
					t.Fatalf("err=%v cmds=%s", err, cmds)
				}
			default:
				if err == nil || len(f.cmds) != 0 {
					t.Fatalf("err=%v cmds=%s", err, cmds)
				}
			}
		})
	}
}
