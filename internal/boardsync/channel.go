package boardsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/LensBridge/agent/internal/mbu"
)

// Channel is a release channel pointer file (section 9.4). It is only a
// pointer: the package it names is verified against the release keys and the
// monotonic rules like any other, so a tampered channel can at worst make the
// board download a file and refuse it.
type Channel struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}

var sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Syncer) channelLoop(ctx context.Context) {
	if !sleep(ctx, s.channelFirstDelay) {
		return
	}
	for {
		if err := s.CheckChannels(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("release channel check failed", "err", err)
		}
		if !sleep(ctx, s.channelInterval) {
			return
		}
	}
}

// CheckChannels checks the agent channel, then the app channel, and hands
// anything newer to the update scheduler, which installs it in the board's
// quiet window. The agent goes first because an app release may need a newer
// local API than the running agent serves: the scheduler only holds such an
// app when a new enough agent is waiting with it.
func (s *Syncer) CheckChannels(ctx context.Context) error {
	var errs []error
	if u := s.d.Cfg.AgentChannelURL(runtime.GOARCH); u != "" {
		if err := s.checkChannel(ctx, mbu.TypeAgent, u); err != nil {
			errs = append(errs, fmt.Errorf("agent channel: %w", err))
		}
	}
	if u := s.d.Cfg.AppChannelURL(); u != "" {
		if err := s.checkChannel(ctx, mbu.TypeApp, u); err != nil {
			errs = append(errs, fmt.Errorf("app channel: %w", err))
		}
	}
	return errors.Join(errs...)
}

// checkChannel fetches one channel and, if it points at something newer than
// what is installed or already waiting, downloads it and offers it to the
// update scheduler.
func (s *Syncer) checkChannel(ctx context.Context, kind mbu.Type, channelURL string) error {
	ch, err := s.fetchChannel(ctx, channelURL)
	if err != nil {
		return err
	}

	var installed string
	switch kind {
	case mbu.TypeApp:
		st, err := s.d.Layout.State().Load()
		if err != nil {
			return err
		}
		installed = st.AppVersion
	case mbu.TypeAgent:
		installed = s.d.AgentVersion
		if !mbu.ValidVersion(installed) {
			// A developer build ("dev") sorts before every release, so it
			// would be replaced by the first channel check. Someone running
			// a dev build on a board wants to keep it.
			s.log.Info("not following the agent channel: this is a development build", "version", installed)
			return nil
		}
		st, err := s.d.Layout.State().Load()
		if err != nil {
			return err
		}
		if st.AgentRejected(ch.Version) {
			s.log.Info("agent channel offers a version that failed on this board before; skipping", "version", ch.Version)
			return nil
		}
	}
	if mbu.CompareVersions(ch.Version, installed) <= 0 {
		s.log.Debug("release channel: up to date", "type", kind, "installed", installed, "channel", ch.Version)
		return nil
	}
	if waiting := s.d.Updates.Pending(kind); waiting != "" && mbu.CompareVersions(ch.Version, waiting) <= 0 {
		s.log.Debug("release channel: already waiting to install", "type", kind, "version", waiting)
		return nil
	}
	s.log.Info("release channel offers an update", "type", kind, "installed", installed, "version", ch.Version)

	pkgURL, err := resolve(channelURL, ch.URL)
	if err != nil {
		return err
	}
	dir, err := s.downloadDir()
	if err != nil {
		return err
	}
	defer removeAll(dir)
	file := filepath.Join(dir, fmt.Sprintf("musallahboard-%s-%s.mbu", kind, ch.Version))
	if err := s.download(ctx, pkgURL, file, ch); err != nil {
		return err
	}
	return s.d.Updates.Offer(file)
}

func (s *Syncer) fetchChannel(ctx context.Context, channelURL string) (*Channel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, channelURL, nil)
	if err != nil {
		return nil, fmt.Errorf("bad channel URL: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("channel returned %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxChannelBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxChannelBytes {
		return nil, fmt.Errorf("channel file is larger than %d KiB", maxChannelBytes>>10)
	}
	var ch Channel
	if err := json.Unmarshal(raw, &ch); err != nil {
		return nil, fmt.Errorf("channel file is not valid JSON: %w", err)
	}
	ch.SHA256 = strings.ToLower(ch.SHA256)
	switch {
	case !mbu.ValidVersion(ch.Version):
		return nil, fmt.Errorf("channel version %q is not MAJOR.MINOR.PATCH", ch.Version)
	case ch.URL == "":
		return nil, fmt.Errorf("channel has no url")
	case !sha256HexRE.MatchString(ch.SHA256):
		return nil, fmt.Errorf("channel sha256 is malformed")
	case ch.Bytes <= 0 || ch.Bytes > maxPackage:
		return nil, fmt.Errorf("channel bytes %d is not a valid package size", ch.Bytes)
	}
	return &ch, nil
}

// download fetches the package and checks its size and hash against the
// channel before anything else looks at it.
func (s *Syncer) download(ctx context.Context, pkgURL, file string, ch *Channel) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pkgURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("package download returned %d", resp.StatusCode)
	}
	h := sha256.New()
	n, err := saveCapped(file, resp.Body, ch.Bytes, h)
	if err != nil {
		if err == errTooLarge {
			return fmt.Errorf("package is larger than the %d bytes the channel promised", ch.Bytes)
		}
		return fmt.Errorf("package download failed: %w", err)
	}
	if n != ch.Bytes {
		removeAll(file)
		return fmt.Errorf("package is %d bytes but the channel says %d", n, ch.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != ch.SHA256 {
		removeAll(file)
		return fmt.Errorf("package SHA-256 does not match the channel (got %s)", got)
	}
	return nil
}

// resolve lets a channel give its url relative to the channel file.
func resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("channel url is not a URL: %w", err)
	}
	u := b.ResolveReference(r)
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("channel url scheme %q is not http(s)", u.Scheme)
	}
	return u.String(), nil
}
