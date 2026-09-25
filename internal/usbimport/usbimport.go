// Package usbimport is the root helper that takes update packages off a USB
// stick (docs/architecture.md, section 9.6).
//
// udev starts `musallahboard-agent usb-import <kernel name>` as root, with no
// network, when a USB block device with a filesystem appears. The helper
// treats the stick as hostile: it accepts only a few ordinary filesystems,
// mounts read-only with nosuid, nodev and noexec, follows no symlinks, and
// copies at most a handful of size-capped *.mbu files into the daemon's inbox.
// It verifies nothing itself. The daemon verifies every package exactly as it
// would from any other source, and the helper just waits for its results so
// they end up in the journal next to the insertion.
//
// The person at the board hears about the stick through banners: the helper
// drops a notice (Announce) in the inbox, which the daemon shows over the
// running board. "Reading USB stick" as soon as the stick is seen, then
// either the daemon's own outcome for the packages, or why there are none.
package usbimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/notice"
	"github.com/LensBridge/agent/internal/store"
)

const (
	// MaxFiles is the most packages taken from one stick.
	MaxFiles = 16
	// DefaultMountRoot holds one mount point per device.
	DefaultMountRoot = "/run/musallahboard/usb"
	// WaitTimeout bounds how long the helper waits for the daemon. An agent
	// update restarts the daemon, and a large content package takes a while
	// on a Pi's SD card.
	WaitTimeout = 15 * time.Minute
	// FolderName is the optional folder on the stick, matched ignoring case.
	FolderName = "MusallahBoard"
	// MountOptions are fixed: nothing on the stick is executed, no device
	// node or setuid bit on it means anything, and it is never written to.
	MountOptions = "ro,nosuid,nodev,noexec,noatime"
)

// NameRE is what a USB disk or partition's kernel name looks like. Anything
// else (an SD card, a loop device, a path) is refused before it reaches a
// command line.
var NameRE = regexp.MustCompile(`^sd[a-z]+[0-9]*$`)

// Helper does one stick. Every outside effect is a field so tests can run it
// in a temp directory.
type Helper struct {
	Layout    store.Layout
	MountRoot string
	DevDir    string
	// Exec runs a command and returns its combined output.
	Exec func(ctx context.Context, name string, args ...string) (string, error)
	// Chown gives a copied file the inbox directory's owner, so the
	// unprivileged daemon can read it and delete it once processed.
	Chown func(path, dir string)
	// Logf writes one plain line; journald captures stdout.
	Logf        func(format string, args ...any)
	WaitTimeout time.Duration
	Poll        time.Duration
}

// New returns a Helper with the production paths and commands.
func New() *Helper {
	h := &Helper{
		Layout:      store.Default(),
		MountRoot:   DefaultMountRoot,
		DevDir:      "/dev",
		Exec:        runCommand,
		Chown:       chownToDirOwner,
		Logf:        func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
		WaitTimeout: WaitTimeout,
		Poll:        time.Second,
	}
	return h
}

// ErrUnsupported means the device was left alone on purpose (unknown
// filesystem). It is not a failure worth a failed unit.
var ErrUnsupported = errors.New("not a supported USB filesystem")

// Run imports from the device with kernel name name.
func (h *Helper) Run(ctx context.Context, name string) error {
	if !NameRE.MatchString(name) {
		return fmt.Errorf("%q is not a USB disk name (want sdX or sdXN)", name)
	}
	h.Announce(name, notice.New(notice.Progress, "Reading USB stick"))
	dev := filepath.Join(h.DevDir, name)
	fsType, err := h.Exec(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
	fsType = strings.TrimSpace(fsType)
	if err != nil || fsType == "" {
		h.Logf("%s: no filesystem found (%v); ignoring it", name, err)
		h.Announce(name, notice.New(notice.Problem, "Can't read this USB stick",
			"Format it as FAT32 or exFAT, then copy the updates onto it again"))
		return ErrUnsupported
	}
	mountType, ok := h.mountType(fsType)
	if !ok {
		h.Logf("%s: filesystem %q is not accepted (use FAT32, exFAT, NTFS or ext4); ignoring it", name, fsType)
		h.Announce(name, notice.New(notice.Problem, "Can't read this USB stick",
			"It is formatted as "+strings.ToUpper(fsType)+", which the board does not read",
			"Format it as FAT32 or exFAT, then copy the updates onto it again"))
		return ErrUnsupported
	}

	mp := filepath.Join(h.MountRoot, name)
	if err := os.MkdirAll(mp, 0o700); err != nil {
		h.Announce(name, notice.New(notice.Problem, "Can't read this USB stick", "The board could not open it"))
		return fmt.Errorf("cannot create mount point %s: %w", mp, err)
	}
	if out, err := h.Exec(ctx, "mount", "-t", mountType, "-o", MountOptions, dev, mp); err != nil {
		os.Remove(mp)
		h.Announce(name, notice.New(notice.Problem, "Can't read this USB stick",
			"The board could not open it. Try formatting it as FAT32 or exFAT"))
		return fmt.Errorf("could not mount %s (%s): %v %s", name, fsType, err, strings.TrimSpace(out))
	}
	h.Logf("%s: mounted read-only (%s)", name, fsType)

	files, notes := Scan(mp)
	for _, n := range notes {
		h.Logf("%s: %s", name, n)
	}
	var parts []part
	if len(files) == 0 {
		h.Logf("%s: no .mbu packages found (put them at the top of the stick or in a %s folder)", name, FolderName)
	} else {
		parts = h.copyAll(name, files)
	}
	// Unmount before the daemon sees anything, so that by the time the board
	// says what it makes of the stick ("You can remove the USB stick" with
	// every outcome), the stick really can be pulled.
	h.unmount(ctx, name, mp)
	queued := h.queue(name, parts)

	switch {
	case len(files) == 0:
		h.Announce(name, notice.New(notice.Neutral, "No updates on this USB stick",
			"Put the .mbu files at the top of the stick, or in a "+FolderName+" folder"))
		return nil
	case len(queued) == 0:
		h.Announce(name, notice.New(notice.Problem, "Can't read this USB stick",
			"Copying the updates off it failed. Try copying them again, or another stick"))
		return nil
	}
	h.Logf("%s: copied %d package(s); waiting for the board to check and install them", name, len(queued))
	h.wait(ctx, queued)
	return nil
}

// mountType maps blkid's type to the mount -t value, and whether it is
// accepted at all. NTFS goes through the kernel's ntfs3 driver; the FUSE
// ntfs-3g driver is not used, it runs a userspace process over the stick's
// contents. The exfat and ntfs3 modules are loaded at boot
// (/etc/modules-load.d/musallahboard.conf): this helper's sandbox
// (ProtectKernelModules=yes) cannot load them itself.
func (h *Helper) mountType(fsType string) (string, bool) {
	switch fsType {
	case "vfat", "exfat", "ext4", "ntfs3":
		return fsType, true
	case "ntfs":
		return "ntfs3", true
	}
	return "", false
}

func (h *Helper) unmount(ctx context.Context, name, mp string) {
	if _, err := h.Exec(ctx, "umount", mp); err != nil {
		// Something still holds it (it should not); detach it anyway so a
		// pulled stick leaves nothing behind.
		if out, err := h.Exec(ctx, "umount", "-l", mp); err != nil {
			h.Logf("%s: could not unmount %s: %v %s", name, mp, err, strings.TrimSpace(out))
			return
		}
	}
	os.Remove(mp)
	h.Logf("%s: unmounted; the stick can be removed", name)
}

// Announce puts n on the board as a banner, through the daemon's inbox
// (inbox/usb-<dev>.notice.json). A later notice for the same device replaces
// an earlier one the daemon has not shown yet. Best effort: a stick is read
// whether or not anyone is told.
func (h *Helper) Announce(dev string, n notice.Notice) {
	// Every outcome is announced once the stick is no longer in use; only
	// "Reading USB stick" is not an outcome.
	if n.Tone != notice.Progress {
		n.Footer = notice.RemoveStick
	}
	inbox := h.Layout.Inbox()
	raw, err := json.Marshal(n)
	if err == nil {
		err = os.MkdirAll(inbox, 0o770)
	}
	path := filepath.Join(inbox, "usb-"+dev+importer.NoticeSuffix)
	if err == nil {
		err = fsutil.WriteAtomic(path, raw, 0o644)
	}
	if err != nil {
		h.Logf("%s: could not tell the board: %v", dev, err)
		return
	}
	if h.Chown != nil {
		h.Chown(path, inbox)
	}
}

// Candidate is a package file found on the stick.
type Candidate struct {
	Path string
	Size int64
}

// Scan lists the packages to take from the stick mounted at root: regular
// files (never symlinks) named *.mbu in any case, at the top level and in a
// top-level folder named MusallahBoard in any case, at most MaxFiles, each at
// most mbu.MaxPackageBytes. notes explains anything skipped.
func Scan(root string) (files []Candidate, notes []string) {
	dirs := []string{root}
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if strings.EqualFold(e.Name(), FolderName) && e.Type().IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	var all []Candidate
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			notes = append(notes, fmt.Sprintf("cannot read %s: %v", rel(root, d), err))
			continue
		}
		for _, e := range entries {
			n := e.Name()
			// "._name.mbu" is macOS metadata written beside every file on
			// a FAT stick; it is not a package.
			if strings.HasPrefix(n, ".") || !strings.HasSuffix(strings.ToLower(n), ".mbu") {
				continue
			}
			p := filepath.Join(d, n)
			fi, err := os.Lstat(p)
			if err != nil {
				notes = append(notes, fmt.Sprintf("cannot read %s: %v", rel(root, p), err))
				continue
			}
			if !fi.Mode().IsRegular() {
				notes = append(notes, fmt.Sprintf("skipping %s: not a regular file", rel(root, p)))
				continue
			}
			if fi.Size() > mbu.MaxPackageBytes {
				notes = append(notes, fmt.Sprintf("skipping %s: %d MiB is larger than the %d MiB a package may be",
					rel(root, p), fi.Size()>>20, mbu.MaxPackageBytes>>20))
				continue
			}
			all = append(all, Candidate{Path: p, Size: fi.Size()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
	if len(all) > MaxFiles {
		for _, c := range all[MaxFiles:] {
			notes = append(notes, fmt.Sprintf("skipping %s: at most %d packages are taken from one stick", rel(root, c.Path), MaxFiles))
		}
		all = all[:MaxFiles]
	}
	return all, notes
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

type queuedFile struct {
	orig  string // name on the stick, for the log
	inbox string // base name in the inbox
}

// part is a package copied into the inbox under a .part name, which the
// daemon ignores until queue renames it.
type part struct {
	q    queuedFile
	path string
}

// copyAll copies every candidate to a .part name first; queue renames them
// to .mbu once all are copied and the stick is unmounted, so the daemon's
// next inbox scan sees the whole stick as one batch (one update screen,
// agent first).
func (h *Helper) copyAll(dev string, files []Candidate) []part {
	inbox := h.Layout.Inbox()
	if err := os.MkdirAll(inbox, 0o770); err != nil {
		h.Logf("%s: cannot use the inbox %s: %v", dev, inbox, err)
		return nil
	}
	var parts []part
	for i, c := range files {
		base := inboxName(dev, i+1, filepath.Base(c.Path))
		// A result left from an earlier insertion of the same stick would
		// otherwise be read as this one's.
		os.Remove(filepath.Join(h.Layout.InboxResults(), base+".json"))
		tmp := filepath.Join(inbox, strings.TrimSuffix(base, ".mbu")+".part")
		if err := copyCapped(c.Path, tmp); err != nil {
			os.Remove(tmp)
			h.Logf("%s: could not copy %s: %v", dev, filepath.Base(c.Path), err)
			continue
		}
		if h.Chown != nil {
			h.Chown(tmp, inbox)
		}
		parts = append(parts, part{queuedFile{orig: filepath.Base(c.Path), inbox: base}, tmp})
	}
	return parts
}

// queue hands copied packages to the daemon.
func (h *Helper) queue(dev string, parts []part) []queuedFile {
	inbox := h.Layout.Inbox()
	var out []queuedFile
	for _, p := range parts {
		if err := os.Rename(p.path, filepath.Join(inbox, p.q.inbox)); err != nil {
			os.Remove(p.path)
			h.Logf("%s: could not queue %s: %v", dev, p.q.orig, err)
			continue
		}
		out = append(out, p.q)
	}
	return out
}

// inboxName is usb-<dev>-<n>-<stem>.mbu. The "usb-" prefix is how the inbox
// runner knows the source; the stem is reduced to safe characters because it
// came off a stick.
func inboxName(dev string, n int, orig string) string {
	stem := orig[:len(orig)-len(filepath.Ext(orig))]
	var b strings.Builder
	for _, c := range stem {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		s = "package"
	}
	return fmt.Sprintf("usb-%s-%d-%s.mbu", dev, n, s)
}

// copyCapped copies src (on the stick) to a new file dst, refusing a symlink
// swapped in after the scan and anything over the package size limit.
func copyCapped(src, dst string) error {
	in, err := openRegularNoFollow(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, mbu.MaxPackageBytes+1))
	if err == nil && n > mbu.MaxPackageBytes {
		err = fmt.Errorf("larger than %d MiB", mbu.MaxPackageBytes>>20)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// wait logs each queued file's result as the daemon writes it.
func (h *Helper) wait(ctx context.Context, queued []queuedFile) {
	ctx, cancel := context.WithTimeout(ctx, h.WaitTimeout)
	defer cancel()
	pending := append([]queuedFile(nil), queued...)
	for len(pending) > 0 {
		var still []queuedFile
		for _, q := range pending {
			raw, err := os.ReadFile(filepath.Join(h.Layout.InboxResults(), q.inbox+".json"))
			if err != nil {
				still = append(still, q)
				continue
			}
			var r importer.InboxResult
			if err := json.Unmarshal(raw, &r); err != nil {
				// Written atomically, so this is a genuinely bad file.
				h.Logf("%s: unreadable result: %v", q.orig, err)
				continue
			}
			h.Logf("%s: %s: %s", q.orig, r.Action, r.Message)
		}
		pending = still
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			for _, q := range pending {
				h.Logf("%s: no result after %s; the board may still be working on it (see `journalctl -u musallahboard-agent`)",
					q.orig, h.WaitTimeout)
			}
			return
		case <-time.After(h.Poll):
		}
	}
	h.Logf("all packages from the stick have been handled")
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
