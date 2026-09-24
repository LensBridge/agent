package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Limits the board enforces (docs/architecture.md, 9.5). Checked here too, so
// a file that could never be accepted is not sent across first.
const (
	maxPackageBytes = 512 << 20
	maxUploadBytes  = 1 << 30
)

// boardClient talks to the upload server on the service port.
type boardClient struct {
	base string // http://10.77.0.1
	hc   *http.Client
	now  func() time.Time
}

func newBoardClient(host string) *boardClient {
	base := host
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return &boardClient{
		base: strings.TrimRight(base, "/"),
		hc: &http.Client{
			Transport: &http.Transport{
				// Never through a proxy: the board is at the other end of a
				// cable, and a laptop's corporate proxy settings would send
				// 10.77.0.1 somewhere else entirely.
				Proxy:       nil,
				DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			},
			// A full 1 GiB upload over a slow adapter, then the board
			// verifying and installing it, all inside one request.
			Timeout: 60 * time.Minute,
		},
		now: time.Now,
	}
}

// ── Import ───────────────────────────────────────────────────────────────────

// importResult is one package's outcome (docs/architecture.md, section 8).
type importResult struct {
	File    string `json:"file"`
	Type    string `json:"type"`
	Version string `json:"version"`
	Action  string `json:"action"`
	Message string `json:"message"`
}

type importResponse struct {
	Results []importResult `json:"results"`
	Clock   *struct {
		// Board minus uploader, before any correction: positive means the
		// board was ahead.
		DriftSeconds int64  `json:"driftSeconds"`
		Adjusted     bool   `json:"adjusted"`
		Note         string `json:"note"`
	} `json:"clock"`
}

// runPush uploads files as one batch and prints what the board did with each.
// It fails if the upload failed or any package was rejected.
func runPush(c *boardClient, files []string, out io.Writer) error {
	var total int64
	sizes := make([]int64, len(files))
	for i, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			return fmt.Errorf("cannot read %s: %v", f, err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a file", f)
		}
		if fi.Size() > maxPackageBytes {
			return fmt.Errorf("%s is %s; the board accepts packages up to %s", f, humanBytes(fi.Size()), humanBytes(maxPackageBytes))
		}
		sizes[i] = fi.Size()
		total += fi.Size()
	}
	if total > maxUploadBytes {
		return fmt.Errorf("these packages add up to %s; the board accepts up to %s at a time. Send them in two goes", humanBytes(total), humanBytes(maxUploadBytes))
	}

	step(out, "Sending %d package(s), %s, to the board at %s", len(files), humanBytes(total), c.hostName())

	// Streamed: a 500 MB package is read from disk as it is sent, never held
	// in memory.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	writeErr := make(chan error, 1)
	go func() {
		err := writeParts(mw, files, sizes, out)
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err) // nil closes normally
		writeErr <- err
	}()

	req, err := http.NewRequest(http.MethodPost, c.base+"/api/import", pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// The board sets its clock from this, but only once a package in this
	// request has verified, and only within limits (section 10).
	req.Header.Set("X-MB-Client-Time", strconv.FormatInt(c.now().Unix(), 10))

	resp, err := c.hc.Do(req)
	pr.Close() // unblocks the writer if the board answered before reading it all
	werr := <-writeErr
	if err != nil {
		if werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
			return fmt.Errorf("could not read a package: %v\nNothing was installed", werr)
		}
		return c.connectError(err, true)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.statusError(resp)
	}
	var ir importResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ir); err != nil {
		return fmt.Errorf("the board sent an answer mbpush does not understand (%v).\nRun `mbpush status` to see what is installed", err)
	}
	return printImport(out, ir)
}

func writeParts(mw *multipart.Writer, files []string, sizes []int64, out io.Writer) error {
	for i, f := range files {
		fmt.Fprintf(out, "    %s (%s)\n", filepath.Base(f), humanBytes(sizes[i]))
		src, err := os.Open(f)
		if err != nil {
			return err
		}
		part, err := mw.CreateFormFile("package", filepath.Base(f))
		if err == nil {
			_, err = io.Copy(part, src)
		}
		src.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// actionLabel puts a result's action in words for the results table.
func actionLabel(action string) string {
	switch action {
	case "installed":
		return "Installed"
	case "unchanged":
		return "Already installed"
	case "staged":
		return "Installing"
	case "skipped":
		return "Skipped"
	case "rejected":
		return "NOT INSTALLED"
	default:
		return action
	}
}

func printImport(out io.Writer, ir importResponse) error {
	step(out, "Results")
	rejected, staged := 0, 0
	if len(ir.Results) == 0 {
		fmt.Fprintln(out, "    The board reported no results.")
	}
	for _, r := range ir.Results {
		fmt.Fprintf(out, "    %-18s %s\n", actionLabel(r.Action), r.File)
		if r.Message != "" {
			fmt.Fprintf(out, "    %-18s %s\n", "", r.Message)
		}
		switch r.Action {
		case "rejected":
			rejected++
		case "staged":
			staged++
		}
	}

	if ir.Clock != nil {
		step(out, "Clock")
		fmt.Fprintf(out, "    %s\n", describeClockReport(ir.Clock.DriftSeconds, ir.Clock.Adjusted))
		if ir.Clock.Note != "" {
			fmt.Fprintf(out, "    (%s)\n", ir.Clock.Note)
		}
	}

	fmt.Fprintln(out)
	if staged > 0 {
		fmt.Fprintln(out, "The board is updating its own software and restarts it by itself. Leave it powered")
		fmt.Fprintln(out, "on; any other packages in this upload are installed after the restart.")
	}
	if rejected > 0 {
		return fmt.Errorf("%d package(s) were not installed (see above). The board keeps what it had", rejected)
	}
	fmt.Fprintln(out, "Done. Wait for the board's screen to say \"Update complete\", then unplug the cable.")
	return nil
}

// describeClockReport puts the board's clock report in words.
func describeClockReport(driftSeconds int64, adjusted bool) string {
	drift := time.Duration(driftSeconds) * time.Second
	switch {
	case adjusted:
		return fmt.Sprintf("The board's clock was %s. It has been set to this laptop's time.", describeDrift(drift))
	case drift.Abs() <= clockTolerance:
		return "The board's clock is " + describeDrift(drift) + "."
	default:
		return fmt.Sprintf("The board's clock is %s, and was not changed.", describeDrift(drift))
	}
}

// ── Status ───────────────────────────────────────────────────────────────────

type boardStatus struct {
	DeviceID     string `json:"deviceId"`
	AgentVersion string `json:"agentVersion"`
	App          *struct {
		Version string `json:"version"`
	} `json:"app"`
	Content *struct {
		FirstDay    string `json:"firstDay"`
		LastDay     string `json:"lastDay"`
		Timezone    string `json:"timezone"`
		Source      string `json:"source"`
		InstalledAt string `json:"installedAt"`
	} `json:"content"`
	Today         string `json:"today"`
	DaysRemaining *int   `json:"daysRemaining"`
	StaleDays     *int   `json:"staleDays"`
	Clock         struct {
		Unix     int64  `json:"unix"`
		Timezone string `json:"timezone"`
	} `json:"clock"`
	RTC    bool `json:"rtc"`
	Update struct {
		Active bool `json:"active"`
	} `json:"update"`
}

func runStatus(c *boardClient, out io.Writer) error {
	sent := c.now()
	resp, err := c.hc.Get(c.base + "/api/status")
	got := c.now()
	if err != nil {
		return c.connectError(err, false)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.statusError(resp)
	}
	var st boardStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return fmt.Errorf("the board sent a status mbpush does not understand: %v", err)
	}
	printStatus(out, c.hostName(), st, sent, got)
	return nil
}

func printStatus(out io.Writer, host string, st boardStatus, sent, got time.Time) {
	row := func(label, format string, args ...any) {
		fmt.Fprintf(out, "    %-11s %s\n", label, fmt.Sprintf(format, args...))
	}
	step(out, "Board at %s", host)
	row("Board", "%s", orUnknown(st.DeviceID))
	row("Agent", "%s", orUnknown(st.AgentVersion))
	if st.App != nil {
		row("Board app", "%s", st.App.Version)
	} else {
		row("Board app", "not installed (send musallahboard-app-<version>.mbu)")
	}

	if st.Content == nil {
		row("Content", "none installed (send the board's content package from LensBridge)")
	} else {
		c := st.Content
		row("Content", "%s to %s (%s), %s", c.FirstDay, c.LastDay, c.Timezone, describeSource(c.Source))
		switch {
		case st.StaleDays != nil && *st.StaleDays > 0:
			row("", "OUT OF DATE: it ran out %s ago, so the board is repeating %s.", plural(*st.StaleDays, "day"), c.LastDay)
		case st.DaysRemaining != nil && *st.DaysRemaining == 0:
			row("", "Today (%s) is its last day.", st.Today)
		case st.DaysRemaining != nil:
			row("", "Today is %s: %s of content left after today.", st.Today, plural(*st.DaysRemaining, "day"))
		}
	}

	if st.Clock.Unix > 0 {
		cc := compareClocks(sent, got, st.Clock.Unix)
		zone := ""
		if st.Clock.Timezone != "" {
			zone = " (time zone " + st.Clock.Timezone + ")"
		}
		row("Clock", "%s%s", describeDrift(cc.Drift), zone)
		if cc.NeedsSet {
			row("", "Sending a package with mbpush sets it to this laptop's time.")
		}
	}
	if st.RTC {
		row("RTC", "fitted: the board keeps time while unplugged")
	} else {
		row("RTC", "none: the board loses time while unplugged")
	}
	if st.Update.Active {
		row("Update", "in progress")
	}
}

func describeSource(s string) string {
	switch s {
	case "sync":
		return "synced from LensBridge"
	case "usb":
		return "from a USB stick"
	case "upload":
		return "uploaded from a laptop or phone"
	case "cli":
		return "installed by an admin"
	case "":
		return "source unknown"
	default:
		return "from " + s
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// ── Errors ───────────────────────────────────────────────────────────────────

func (c *boardClient) hostName() string {
	if i := strings.Index(c.base, "://"); i >= 0 {
		return c.base[i+3:]
	}
	return c.base
}

// connectError explains a failed request. duringUpload says whether the board
// may have received part of an upload.
func (c *boardClient) connectError(err error, duringUpload bool) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return fmt.Errorf("could not reach the board at %s (%v).\n"+
			"  - Is the cable plugged into the board's ethernet port?\n"+
			"  - Is this laptop's wired adapter set to get an address automatically (DHCP)?\n"+
			"    It gets a 10.77.0.x address within about a minute of plugging in.\n"+
			"  - Is the board's service port on? (on the board: sudo musallahboard-agent service-port on)\n"+
			"Nothing on the board has changed", c.hostName(), op.Err)
	}
	if duringUpload {
		return fmt.Errorf("lost the connection to the board during the upload (%v).\n"+
			"Run `mbpush status` to see what the board has installed, then send the packages again", err)
	}
	return fmt.Errorf("lost the connection to the board (%v)", err)
}

// statusError turns a non-200 answer into plain English.
func (c *boardClient) statusError(resp *http.Response) error {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	switch resp.StatusCode {
	case http.StatusConflict:
		return errors.New("the board is already installing another upload (from a browser, a phone or another mbpush).\n" +
			"Wait until its screen says \"Update complete\", then try again. Nothing from this upload was installed")
	case http.StatusMisdirectedRequest:
		return fmt.Errorf("the board refused a request addressed to %q. It only answers to %s or musallahboard.local,\n"+
			"so leave out --host, or use one of those", c.hostName(), defaultHost)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("the board says the upload is too large: %s", orDefault(body.Message, resp.Status))
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return fmt.Errorf("%s answered %s. Is that a MusallahBoard on the service port, with agent v2 or later?", c.hostName(), resp.Status)
	default:
		return fmt.Errorf("the board answered %s: %s", resp.Status, orDefault(body.Message, "no details"))
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
