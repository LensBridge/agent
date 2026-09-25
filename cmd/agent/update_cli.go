package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/updates"
)

// updateWait is how long `update now` waits: a download on a slow link, then
// an install.
const updateWait = 15 * time.Minute

// runUpdate is `update now` (docs/architecture.md, section 9.4): check the
// release channels and install a new app or agent at once instead of in the
// board's quiet window. The daemon does the work; this asks it to by
// dropping a request file, as `import` does with the inbox, and prints its
// answer.
func runUpdate(args []string) {
	if len(args) != 1 || args[0] != "now" {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent update now")
		os.Exit(2)
	}
	requireRoot("update now")
	l := store.Default()
	if err := os.MkdirAll(l.Updates(), 0o750); err != nil {
		fail("could not create %s: %v", l.Updates(), err)
	}
	chownLike(l.Updates(), l.Root)
	if err := os.Remove(l.UpdateRequestResult()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fail("%v", err)
	}
	if err := fsutil.WriteAtomic(l.UpdateRequest(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o640); err != nil {
		fail("could not ask the agent: %v", err)
	}
	chownLike(l.UpdateRequest(), l.Root)
	fmt.Println("Asked the agent to check for updates and install them now ...")

	deadline := time.Now().Add(updateWait)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(l.UpdateRequestResult())
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		var out updates.Outcome
		if err := json.Unmarshal(raw, &out); err != nil {
			time.Sleep(time.Second)
			continue
		}
		printUpdateOutcome(out)
		return
	}
	fail("no answer after %s. Is musallahboard-agent running? (systemctl status musallahboard-agent)", updateWait)
}

func printUpdateOutcome(out updates.Outcome) {
	if len(out.Results) == 0 {
		if out.CheckError != "" {
			// Nothing was installed, and there is no telling whether that is
			// because nothing is newer: do not say "up to date".
			fmt.Printf("  Could not check for updates: %s\n", out.CheckError)
			fmt.Println("  Nothing was installed. Check the board's internet connection and try again.")
			os.Exit(1)
		}
		fmt.Println("  Nothing to install: the board app and agent are up to date.")
		return
	}
	if out.CheckError != "" {
		fmt.Printf("  Could not check for updates: %s\n", out.CheckError)
		fmt.Println("  Installed what was already downloaded:")
	}
	rejected := false
	for _, r := range out.Results {
		if r.Action == importer.ActionRejected {
			rejected = true
		}
		printResult(r)
	}
	if rejected {
		os.Exit(1)
	}
}
