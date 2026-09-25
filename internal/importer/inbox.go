package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/fsutil"
	"github.com/LensBridge/agent/internal/notice"
)

// InboxPoll is how often the inbox is scanned. Polling rather than inotify:
// it is one ReadDir of a nearly always empty directory, and it cannot miss a
// file dropped while the daemon was restarting.
const InboxPoll = 2 * time.Second

// NoticeSuffix marks a banner dropped in the inbox by the root USB helper
// (usb-<dev>.notice.json, a notice.Notice), for the daemon to show.
const NoticeSuffix = ".notice.json"

// InboxResult is what the daemon writes to inbox/results/<file>.json.
type InboxResult struct {
	Result
	At string `json:"at"`
}

// RunInbox processes *.mbu files dropped into the inbox (by the CLI, the USB
// helper, or a batch requeued behind an agent update) until ctx ends. Each
// scan's files form one batch. A file whose name starts with "usb-" counts as
// source usb; anything else as cli. Processed files are deleted and their
// results written for whoever is waiting on them.
func (im *Importer) RunInbox(ctx context.Context) {
	inbox, results := im.d.Layout.Inbox(), im.d.Layout.InboxResults()
	t := time.NewTicker(InboxPoll)
	defer t.Stop()
	for {
		im.showNotices(inbox)
		files := pending(inbox)
		if len(files) > 0 {
			// Group by source so a USB stick and a CLI import that land in
			// the same scan still report under the right source.
			groups := map[Source][]string{}
			for _, f := range files {
				src := SourceCLI
				if strings.HasPrefix(filepath.Base(f), "usb-") {
					src = SourceUSB
				}
				groups[src] = append(groups[src], f)
			}
			for _, src := range []Source{SourceUSB, SourceCLI} {
				if len(groups[src]) == 0 {
					continue
				}
				b := im.Import(ctx, src, groups[src])
				_ = os.MkdirAll(results, 0o770)
				for _, r := range b.Results {
					if r.Action == ActionQueued {
						continue // moved back into the inbox; reported when it runs
					}
					raw, _ := json.MarshalIndent(InboxResult{Result: r, At: im.d.Now().UTC().Format(time.RFC3339)}, "", "  ")
					_ = fsutil.WriteAtomic(filepath.Join(results, r.File+".json"), raw, 0o660)
				}
				for _, f := range groups[src] {
					if _, err := os.Stat(f); err == nil {
						os.Remove(f)
					}
				}
				if b.AgentStaged {
					// The updater is about to restart us. Stop taking work
					// so the rest waits for the new agent, unless the
					// updater refuses it and this agent carries on.
					select {
					case <-ctx.Done():
						return
					case <-im.awaitReplacement():
					}
				}
			}
			pruneResults(results, im.d.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// showNotices passes on the banners the USB helper left, then deletes them.
func (im *Importer) showNotices(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, NoticeSuffix) {
			continue
		}
		path := filepath.Join(dir, n)
		raw, err := os.ReadFile(path)
		os.Remove(path)
		var msg notice.Notice
		if err != nil || len(raw) > 16<<10 || json.Unmarshal(raw, &msg) != nil || msg.Headline == "" {
			continue
		}
		switch msg.Tone {
		case notice.Progress, notice.OK, notice.Neutral, notice.Problem:
		default:
			msg.Tone = notice.Neutral
		}
		footer := msg.Footer
		msg = notice.New(msg.Tone, msg.Headline, msg.Lines...)
		msg.Footer = footer
		im.d.Logger.Info("usb notice", "tone", msg.Tone, "headline", msg.Headline, "lines", msg.Lines)
		if im.d.Events != nil {
			im.d.Events.Notice(msg)
		}
	}
}

// pending lists complete *.mbu files in dir, oldest name first. Writers copy
// to a .part name and rename, so a listed file is complete.
func pending(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".mbu") {
			continue
		}
		out = append(out, filepath.Join(dir, n))
	}
	sort.Strings(out)
	return out
}

// pruneResults drops results older than a day; nobody waits that long.
func pruneResults(dir string, now time.Time) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
