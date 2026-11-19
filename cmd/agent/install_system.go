package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/LensBridge/agent/internal/core/fsutil"
	"github.com/LensBridge/agent/packaging"
)

// runInstallSystem is `install-system [--rtc]`: write the system files
// embedded in this binary (packaging/): the systemd units, udev rules, the
// sudoers allow-list, the modules-load list, the kiosk launcher and the
// kiosk's local page. setup.sh runs it once the binary is in place. It is
// idempotent: a file that already has the right bytes and mode is left
// alone, and running it again changes nothing.
func runInstallSystem(args []string) {
	fs := flag.NewFlagSet("install-system", flag.ExitOnError)
	rtc := fs.Bool("rtc", false, "also install the units that read a DS3231 RTC at boot")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: sudo musallahboard-agent install-system [--rtc]")
		os.Exit(2)
	}
	requireRoot("install-system")
	files := packaging.System
	if *rtc {
		files = append(append([]packaging.File{}, files...), packaging.RTC...)
	}
	changed, err := installSystem("/", files, packaging.Obsolete, runVisudo)
	for _, c := range changed {
		fmt.Println("  " + c)
	}
	if err != nil {
		fail("%v", err)
	}
	if len(changed) == 0 {
		fmt.Println("System files are up to date.")
	}

	// Each step is idempotent; failures are reported but do not stop the
	// rest (a VM has no exfat module, say).
	for _, cmd := range [][]string{
		{"systemctl", "daemon-reload"},
		append([]string{"systemctl", "enable", "--quiet"}, packaging.Units...),
		{"udevadm", "control", "--reload"},
		// Apply the rtc0 permissions now. Not the block subsystem: that
		// would start an import from a stick that happens to be plugged in.
		{"udevadm", "trigger", "--subsystem-match=rtc", "--action=change"},
		// The USB helper's sandbox (ProtectKernelModules=yes) cannot load
		// filesystem drivers; modules-load.d loads them at boot, this now.
		{"modprobe", "-a", "exfat", "ntfs3"},
	} {
		if out, err := runCmd(cmd[0], cmd[1:]...); err != nil {
			warnf("%s: %v %s", strings.Join(cmd, " "), err, firstLine(out))
		}
	}
}

// installSystem writes files under root (normally "/"), removes the obsolete
// paths, and returns a line for everything it changed. check validates a
// sudoers file before it is put in place.
func installSystem(root string, files []packaging.File, obsolete []string, check func(path string) error) ([]string, error) {
	var changed []string
	for _, f := range files {
		data, err := packaging.Read(f.Name)
		if err != nil {
			return changed, err
		}
		dst := filepath.Join(root, f.Path)
		if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
			if fi, err := os.Stat(dst); err == nil && fi.Mode().Perm() == f.Mode {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return changed, err
		}
		if strings.HasPrefix(f.Path, "/etc/sudoers.d/") {
			// A broken sudoers file breaks sudo for everyone: check it
			// first, under a name sudo ignores (it has a dot).
			tmp := dst + ".new"
			if err := fsutil.WriteAtomic(tmp, data, f.Mode); err != nil {
				return changed, err
			}
			if err := check(tmp); err != nil {
				os.Remove(tmp)
				return changed, fmt.Errorf("%s failed validation: %w", f.Path, err)
			}
			if err := os.Rename(tmp, dst); err != nil {
				return changed, err
			}
		} else if err := fsutil.WriteAtomic(dst, data, f.Mode); err != nil {
			return changed, fmt.Errorf("write %s: %w", f.Path, err)
		}
		// WriteAtomic's mode is subject to the umask.
		if err := os.Chmod(dst, f.Mode); err != nil {
			return changed, err
		}
		changed = append(changed, "wrote "+f.Path)
	}
	for _, p := range obsolete {
		dst := filepath.Join(root, p)
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if root == "/" && strings.HasPrefix(p, "/lib/systemd/system/") {
			_, _ = runCmd("systemctl", "disable", "--now", "--quiet", filepath.Base(p))
		}
		if err := os.Remove(dst); err != nil {
			return changed, err
		}
		changed = append(changed, "removed "+p)
	}
	return changed, nil
}

func runVisudo(path string) error {
	if out, err := runCmd("visudo", "-c", "-f", path); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
