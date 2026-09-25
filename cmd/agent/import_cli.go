package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/store"
)

// importWait is how long `import` waits for the daemon's verdict. An agent
// update restarts the daemon mid-batch, and a large content package takes a
// while to hash on a Pi.
const importWait = 15 * time.Minute

// runImport is `import <file.mbu>... | -` (docs/architecture.md, section 9.7).
// It copies the packages into the inbox and prints the daemon's results. The
// daemon does the verifying and installing, exactly as for a USB stick or an
// upload; this command only delivers.
func runImport(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent import <file.mbu>... | -   (- reads one package from stdin)")
		os.Exit(2)
	}
	requireRoot("import")
	loadConfigCLI()
	l := store.Default()
	inbox := l.Inbox()
	if err := os.MkdirAll(inbox, 0o770); err != nil {
		fail("could not create %s: %v", inbox, err)
	}

	stamp := time.Now().UTC().Format("20060102T150405")
	var names []string
	for i, a := range args {
		base := "stdin.mbu"
		var r io.Reader = os.Stdin
		if a != "-" {
			f, err := os.Open(a)
			if err != nil {
				fail("%v", err)
			}
			defer f.Close()
			r, base = f, filepath.Base(a)
		}
		base = strings.TrimSuffix(base, filepath.Ext(base))
		name := fmt.Sprintf("cli-%s-%d-%s.mbu", stamp, i+1, safeName(base))
		part := filepath.Join(inbox, "."+name+".part")
		n, err := fsutil.CopyFileSync(part, r, mbu.MaxPackageBytes+1, 0o640)
		if err != nil {
			os.Remove(part)
			fail("could not copy %s into the inbox: %v", a, err)
		}
		if n > mbu.MaxPackageBytes {
			os.Remove(part)
			fail("%s is larger than the %d MiB limit", a, mbu.MaxPackageBytes>>20)
		}
		chownLike(part, inbox)
		if err := os.Rename(part, filepath.Join(inbox, name)); err != nil {
			fail("%v", err)
		}
		names = append(names, name)
	}
	fmt.Printf("Handed %d package(s) to the agent; waiting for it to check and install them ...\n", len(names))

	rejected := false
	deadline := time.Now().Add(importWait)
	hinted := false
	for _, name := range names {
		res, ok := waitResult(l.InboxResults(), name, deadline, &hinted)
		if !ok {
			fail("no result for %s after %s. Is musallahboard-agent running? (systemctl status musallahboard-agent)", name, importWait)
		}
		if res.Action == importer.ActionRejected {
			rejected = true
		}
		printResult(res.Result)
	}
	if rejected {
		fmt.Println("Anything refused was not installed; the board keeps what it had.")
		os.Exit(1)
	}
}

// printResult prints one package's outcome, with the technical detail of a
// refusal under it.
func printResult(r importer.Result) {
	mark := "OK "
	switch r.Action {
	case importer.ActionRejected:
		mark = "NO "
	case importer.ActionUnchanged, importer.ActionOutdated, importer.ActionSkipped:
		mark = " - "
	}
	fmt.Printf("  %s %s: %s\n", mark, r.File, r.Message)
	if r.Detail != "" {
		fmt.Printf("        (%s)\n", r.Detail)
	}
}

func waitResult(dir, name string, deadline time.Time, hinted *bool) (importer.InboxResult, bool) {
	path := filepath.Join(dir, name+".json")
	start := time.Now()
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			var r importer.InboxResult
			if json.Unmarshal(raw, &r) == nil {
				return r, true
			}
		}
		if !*hinted && time.Since(start) > 30*time.Second {
			fmt.Println("  (still waiting; an agent update restarts the agent, which takes up to two minutes)")
			*hinted = true
		}
		time.Sleep(time.Second)
	}
	return importer.InboxResult{}, false
}

func safeName(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "package"
	}
	return b.String()
}

// chownLike gives path the owner of dir, so the unprivileged daemon can
// delete the file once it has processed it.
func chownLike(path, dir string) {
	if fi, err := os.Stat(dir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(path, int(st.Uid), int(st.Gid))
		}
	}
}
