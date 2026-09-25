package importer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/notice"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/trust"
)

const dev = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"

var (
	contentKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	releaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
	img        = []byte("\x89PNG pretend")
)

func imgPath() string {
	s := sha256.Sum256(img)
	return "media/" + hex.EncodeToString(s[:]) + ".png"
}

type fakeEvents struct {
	content []int64
	app     []string
	notices []notice.Notice
}

func (f *fakeEvents) ContentChanged(s int64)  { f.content = append(f.content, s) }
func (f *fakeEvents) AppChanged(v string)     { f.app = append(f.app, v) }
func (f *fakeEvents) Notice(n notice.Notice)  { f.notices = append(f.notices, n) }
func (f *fakeEvents) last() (n notice.Notice) { return f.notices[len(f.notices)-1] }

type fakeScreen struct {
	begun, restarting int
	active            bool
	finished          []notice.Notice
}

func (f *fakeScreen) Active() bool                    { return f.active }
func (f *fakeScreen) Begin(context.Context)           { f.begun++; f.active = true }
func (f *fakeScreen) Caption(context.Context, string) {}
func (f *fakeScreen) Restarting(context.Context)      { f.restarting++ }
func (f *fakeScreen) Finish(_ context.Context, n notice.Notice) {
	f.finished = append(f.finished, n)
	f.active = false
}

func setup(t *testing.T) (*Importer, store.Layout, *fakeEvents, *fakeScreen) {
	t.Helper()
	l := store.Layout{Root: t.TempDir()}
	ev, sc := &fakeEvents{}, &fakeScreen{}
	ring := trust.NewRing([]ed25519.PublicKey{contentKey.Public().(ed25519.PublicKey)},
		[]ed25519.PublicKey{releaseKey.Public().(ed25519.PublicKey)})
	im := New(Deps{
		Layout: l, DeviceID: dev, AgentVersion: "0.2.0",
		Ring:   func() (*trust.Ring, error) { return ring, nil },
		Screen: sc, Events: ev,
		Now: func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	})
	return im, l, ev, sc
}

func build(t *testing.T, m mbu.Manifest, src []mbu.Source, k ed25519.PrivateKey) string {
	t.Helper()
	var buf bytes.Buffer
	if err := mbu.Build(&buf, m, src, []ed25519.PrivateKey{k}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "pkg.mbu")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

func content(t *testing.T, seq int64, device, poster string, omitMedia bool) string {
	m := mbu.Manifest{Type: mbu.TypeContent, Sequence: seq, DeviceID: device,
		CreatedAt: "2026-09-24T10:00:00Z",
		Content: &mbu.ContentInfo{Timezone: "America/Toronto", FirstDay: "2026-09-24", LastDay: "2026-09-25",
			Media: []mbu.MediaInfo{{Path: imgPath(), ContentType: "image/png"}}}}
	src := []mbu.Source{
		{Path: "payloads/2026-09-24.json", Data: []byte(`{"n":` + strconv.FormatInt(seq, 10) + `,"frames":[{"frameConfig":{"posterUrl":"` + poster + `"}}]}`)},
		{Path: "payloads/2026-09-25.json", Data: []byte(`{"frames":[]}`)},
		{Path: imgPath(), Data: img, Omit: omitMedia},
	}
	return build(t, m, src, contentKey)
}

func app(t *testing.T, version string, localAPI int) string {
	return build(t, mbu.Manifest{Type: mbu.TypeApp, Version: version, App: &mbu.AppInfo{LocalAPI: localAPI}},
		[]mbu.Source{{Path: "index.html", Data: []byte("<html>" + version)}}, releaseKey)
}

func agent(t *testing.T, version, arch string) string {
	return build(t, mbu.Manifest{Type: mbu.TypeAgent, Version: version, Agent: &mbu.AgentInfo{Arch: arch, Binary: "musallahboard-agent"}},
		[]mbu.Source{{Path: "musallahboard-agent", Data: []byte("#!binary")}}, releaseKey)
}

func one(t *testing.T, b Batch) Result {
	t.Helper()
	if len(b.Results) != 1 {
		t.Fatalf("results = %+v", b.Results)
	}
	return b.Results[0]
}

func TestContentLifecycle(t *testing.T) {
	im, l, ev, _ := setup(t)
	ctx := context.Background()
	poster := "/" + imgPath()

	if r := one(t, im.Import(ctx, SourceUSB, []string{content(t, 100, dev, poster, false)})); r.Action != ActionInstalled {
		t.Fatalf("first install: %+v", r)
	}
	c, err := l.CurrentContent()
	if err != nil || c.Manifest.Sequence != 100 || c.Info.Source != "usb" {
		t.Fatalf("current = %+v, %v", c, err)
	}
	if len(ev.content) != 1 {
		t.Fatalf("content events = %v", ev.content)
	}
	if r := one(t, im.Import(ctx, SourceUSB, []string{content(t, 100, dev, poster, false)})); r.Action != ActionUnchanged {
		t.Fatalf("same sequence: %+v", r)
	}
	if r := one(t, im.Import(ctx, SourceUSB, []string{content(t, 50, dev, poster, false)})); r.Action != ActionOutdated {
		t.Fatalf("older sequence: %+v", r)
	}
	// Delta: media omitted, satisfied from the store.
	if r := one(t, im.Import(ctx, SourceSync, []string{content(t, 200, dev, poster, true)})); r.Action != ActionInstalled {
		t.Fatalf("delta install: %+v", r)
	}
	st, _ := l.State().Load()
	if st.ContentSequence != 200 || st.ClockFloor == 0 {
		t.Fatalf("state = %+v", st)
	}
	// Rollback stays closed even if the bundles are deleted.
	os.RemoveAll(l.BundlesDir())
	if r := one(t, im.Import(ctx, SourceUSB, []string{content(t, 150, dev, poster, false)})); r.Action != ActionOutdated {
		t.Fatalf("rollback after deleting bundles: %+v", r)
	}
}

func TestContentForAnotherBoardIsSkippedQuietly(t *testing.T) {
	im, _, _, sc := setup(t)
	r := one(t, im.Import(context.Background(), SourceUSB,
		[]string{content(t, 100, "11111111-2222-4333-8444-555555555555", "/"+imgPath(), false)}))
	if r.Action != ActionSkipped || sc.begun != 0 {
		t.Fatalf("foreign content: %+v, screen begun %d", r, sc.begun)
	}
}

func TestPosterMustBeLocalMedia(t *testing.T) {
	im, _, _, _ := setup(t)
	r := one(t, im.Import(context.Background(), SourceUSB,
		[]string{content(t, 100, dev, "https://evil.example/x.png", false)}))
	if r.Action != ActionRejected || !strings.Contains(r.Detail, "posterUrl") || strings.Contains(r.Message, "posterUrl") {
		t.Fatalf("remote posterUrl: %+v", r)
	}
}

func TestAppRules(t *testing.T) {
	im, l, ev, _ := setup(t)
	ctx := context.Background()
	if r := one(t, im.Import(ctx, SourceUpload, []string{app(t, "2.1.0", 2)})); r.Action != ActionInstalled {
		t.Fatalf("app: %+v", r)
	}
	if a, err := l.CurrentApp(); err != nil || a.Manifest.Version != "2.1.0" {
		t.Fatalf("current app %+v %v", a, err)
	}
	if len(ev.app) != 1 {
		t.Fatalf("app events %v", ev.app)
	}
	if r := one(t, im.Import(ctx, SourceUpload, []string{app(t, "2.0.9", 2)})); r.Action != ActionOutdated {
		t.Fatalf("older app: %+v", r)
	}
	if r := one(t, im.Import(ctx, SourceUpload, []string{app(t, "3.0.0", LocalAPIVersion+1)})); r.Action != ActionRejected ||
		!strings.Contains(r.Message, "newer agent") {
		t.Fatalf("app needing newer API: %+v", r)
	}
	// An app signed with the content key is not an app this board runs.
	p := build(t, mbu.Manifest{Type: mbu.TypeApp, Version: "9.0.0", App: &mbu.AppInfo{LocalAPI: 2}},
		[]mbu.Source{{Path: "index.html", Data: []byte("x")}}, contentKey)
	if r := one(t, im.Import(ctx, SourceUpload, []string{p})); r.Action != ActionRejected {
		t.Fatalf("app signed by content key: %+v", r)
	}
}

func TestAgentStagingRequeuesTheRest(t *testing.T) {
	im, l, _, sc := setup(t)
	var staged *notice.Notice
	im.d.AgentStaged = func(n notice.Notice) { staged = &n }
	ctx := context.Background()
	if r := one(t, im.Import(ctx, SourceUpload, []string{agent(t, "0.3.0", otherArch())})); r.Action != ActionRejected {
		t.Fatalf("wrong arch: %+v", r)
	}
	if r := one(t, im.Import(ctx, SourceUpload, []string{agent(t, "0.1.0", runtime.GOARCH)})); r.Action != ActionOutdated {
		t.Fatalf("older agent: %+v", r)
	}
	// Content listed first must still wait for the agent: agents go first.
	b := im.Import(ctx, SourceUpload, []string{content(t, 100, dev, "/"+imgPath(), false), agent(t, "0.3.0", runtime.GOARCH)})
	if !b.AgentStaged || len(b.Results) != 2 || b.Results[0].Action != ActionStaged || b.Results[1].Action != ActionQueued {
		t.Fatalf("batch = %+v", b)
	}
	if _, err := os.Stat(l.StagedReady()); err != nil {
		t.Fatal("ready marker missing")
	}
	if files, _ := filepath.Glob(filepath.Join(l.Inbox(), "*.mbu")); len(files) != 1 {
		t.Fatalf("requeued files = %v", files)
	}
	// The new agent finishes the screen; this one only says it restarts.
	if sc.restarting != 1 || len(sc.finished) != 0 {
		t.Fatalf("restarting %d, finished %+v", sc.restarting, sc.finished)
	}
	if staged == nil || staged.Tone != notice.OK || staged.Lines[0] != "Agent 0.3.0" {
		t.Fatalf("notice handed to the next agent: %+v", staged)
	}
}

// Nothing to install: no update screen, a banner instead.
func TestNothingNewIsABanner(t *testing.T) {
	im, _, ev, sc := setup(t)
	ctx := context.Background()
	poster := "/" + imgPath()
	one(t, im.Import(ctx, SourceSync, []string{content(t, 200, dev, poster, false)}))
	ev.notices = nil

	one(t, im.Import(ctx, SourceUSB, []string{content(t, 100, dev, poster, false)}))
	if sc.begun != 0 {
		t.Fatal("the update screen went up for nothing")
	}
	if len(ev.notices) != 2 || ev.notices[0].Tone != notice.Progress {
		t.Fatalf("banners = %+v", ev.notices)
	}
	if n := ev.last(); n.Tone != notice.Neutral || n.Headline != "Already up to date" ||
		n.Lines[0] != "The board already has newer content" {
		t.Fatalf("banner = %+v", n)
	}

	// A package that does not verify: a problem banner, still no screen.
	p := build(t, mbu.Manifest{Type: mbu.TypeApp, Version: "9.0.0", App: &mbu.AppInfo{LocalAPI: 2}},
		[]mbu.Source{{Path: "index.html", Data: []byte("x")}}, contentKey)
	one(t, im.Import(ctx, SourceUpload, []string{p}))
	if n := ev.last(); sc.begun != 0 || n.Tone != notice.Problem || n.Headline != "Update not installed" ||
		n.Lines[0] != "An update is not signed for this board" {
		t.Fatalf("banner = %+v, screen begun %d", n, sc.begun)
	}
}

func TestInstallFinishesTheScreenInPlainWords(t *testing.T) {
	im, _, ev, sc := setup(t)
	one(t, im.Import(context.Background(), SourceUSB, []string{content(t, 100, dev, "/"+imgPath(), false)}))
	if sc.begun != 1 || len(sc.finished) != 1 {
		t.Fatalf("screen begun %d, finished %+v", sc.begun, sc.finished)
	}
	n := sc.finished[0]
	if n.Tone != notice.OK || n.Headline != "Update complete" || len(n.Lines) != 1 ||
		!strings.HasPrefix(n.Lines[0], "New content, through ") || strings.Contains(n.Lines[0], ".mbu") ||
		n.Footer != notice.RemoveStick {
		t.Fatalf("outcome = %+v", n)
	}
	if ev.last().Tone != notice.Progress {
		t.Fatalf("an outcome banner was shown as well as the screen: %+v", ev.notices)
	}
}

func TestSummarize(t *testing.T) {
	r := func(action, msg string) Result { return Result{Action: action, Message: msg} }
	cases := []struct {
		name     string
		results  []Result
		tone     notice.Tone
		headline string
		lines    []string
	}{
		{"installed", []Result{r(ActionInstalled, "Board app 2.1.0"), r(ActionOutdated, "old")},
			notice.OK, "Update complete", []string{"Board app 2.1.0"}},
		{"partial", []Result{r(ActionInstalled, "Board app 2.1.0"), r(ActionRejected, "bad")},
			notice.Problem, "Some updates were not installed", []string{"bad", "Board app 2.1.0"}},
		{"failed", []Result{r(ActionRejected, "a"), r(ActionRejected, "b"), r(ActionRejected, "c"), r(ActionRejected, "d")},
			notice.Problem, "Update not installed", []string{"a", "b", "and 2 more"}},
		{"up to date", []Result{r(ActionUnchanged, "same"), r(ActionOutdated, "older"), r(ActionSkipped, "x")},
			notice.Neutral, "Already up to date", []string{"same", "older"}},
		{"foreign", []Result{r(ActionSkipped, "x"), r(ActionSkipped, "y")},
			notice.Neutral, "Nothing here is for this board", []string{"These updates are for other MusallahBoards"}},
	}
	for _, tc := range cases {
		n := Summarize(Batch{Results: tc.results})
		if n.Tone != tc.tone || n.Headline != tc.headline || strings.Join(n.Lines, "|") != strings.Join(tc.lines, "|") {
			t.Errorf("%s: %+v", tc.name, n)
		}
	}
}

func TestUSBNoticeIsShown(t *testing.T) {
	im, l, ev, _ := setup(t)
	os.MkdirAll(l.Inbox(), 0o770)
	raw, _ := json.Marshal(notice.New(notice.Neutral, "No updates on this USB stick", "Put the .mbu files at the top"))
	os.WriteFile(filepath.Join(l.Inbox(), "usb-sda1"+NoticeSuffix), raw, 0o644)
	im.showNotices(l.Inbox())
	if len(ev.notices) != 1 || ev.notices[0].Headline != "No updates on this USB stick" {
		t.Fatalf("notices = %+v", ev.notices)
	}
	if _, err := os.Stat(filepath.Join(l.Inbox(), "usb-sda1"+NoticeSuffix)); err == nil {
		t.Fatal("notice file left behind")
	}
}

func TestSyncContentIsSilent(t *testing.T) {
	im, _, _, sc := setup(t)
	one(t, im.Import(context.Background(), SourceSync, []string{content(t, 100, dev, "/"+imgPath(), false)}))
	if sc.begun != 0 {
		t.Fatal("background content sync put up the update screen")
	}
}

func TestInboxRunner(t *testing.T) {
	im, l, _, _ := setup(t)
	os.MkdirAll(l.Inbox(), 0o770)
	src := content(t, 100, dev, "/"+imgPath(), false)
	raw, _ := os.ReadFile(src)
	os.WriteFile(filepath.Join(l.Inbox(), "usb-sda1-1-x.mbu"), raw, 0o640)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go im.RunInbox(ctx)
	for ctx.Err() == nil {
		if _, err := os.Stat(filepath.Join(l.InboxResults(), "usb-sda1-1-x.mbu.json")); err == nil {
			c, err := l.CurrentContent()
			if err != nil || c.Info.Source != "usb" {
				t.Fatalf("current %+v %v", c, err)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no inbox result")
}

func otherArch() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}
