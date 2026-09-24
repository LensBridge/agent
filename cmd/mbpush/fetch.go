package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"
)

// channel is a release channel file (docs/architecture.md, 9.4): a pointer to
// the latest package. It is only a pointer. What makes a package safe is its
// signature, which the board checks; the sha256 here just catches a broken
// download before it is carried to a board.
type channel struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}

var (
	versionRE  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fileNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*\.mbu$`)
)

func newDownloadClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// The laptop is online here, so its proxy settings apply.
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout: 15 * time.Second,
		},
		Timeout: 30 * time.Minute,
	}
}

// runFetch downloads the latest board app and agent packages into o.dir, so
// they can be carried to boards without internet. Both are tried even if one
// fails.
func runFetch(hc *http.Client, o options, out io.Writer) error {
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return err
	}
	step(out, "Downloading the latest packages into %s", o.dir)
	var failed int
	for _, ch := range []struct{ what, url string }{
		{"Board app", o.appChannel},
		{"Agent (" + o.arch + ")", o.agentChannel},
	} {
		msg, err := fetchChannel(hc, ch.url, o.dir)
		if err != nil {
			failed++
			fmt.Fprintf(out, "    %-14s FAILED: %v\n", ch.what, err)
			continue
		}
		fmt.Fprintf(out, "    %-14s %s\n", ch.what, msg)
	}
	if failed > 0 {
		return fmt.Errorf("%d download(s) failed (see above)", failed)
	}
	fmt.Fprintln(out, "\nDone. Send them with `mbpush <file.mbu>...`, or copy them to a USB stick.")
	return nil
}

// fetchChannel reads one channel file and downloads the package it points
// at, unless an identical file is already there. It returns what it did.
func fetchChannel(hc *http.Client, channelURL, dir string) (string, error) {
	base, err := url.Parse(channelURL)
	if err != nil {
		return "", fmt.Errorf("bad channel URL %q: %v", channelURL, err)
	}
	resp, err := hc.Get(channelURL)
	if err != nil {
		return "", fmt.Errorf("could not read the release channel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("could not read the release channel %s: %s", channelURL, resp.Status)
	}
	var ch channel
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ch); err != nil {
		return "", fmt.Errorf("the release channel is not valid: %v", err)
	}
	switch {
	case !versionRE.MatchString(ch.Version):
		return "", fmt.Errorf("the release channel has an invalid version %q", ch.Version)
	case !sha256RE.MatchString(ch.SHA256):
		return "", errors.New("the release channel has an invalid sha256")
	case ch.Bytes <= 0 || ch.Bytes > maxPackageBytes:
		return "", fmt.Errorf("the release channel gives an impossible size (%d bytes)", ch.Bytes)
	}
	pkgURL, err := base.Parse(ch.URL) // relative URLs resolve against the channel
	if err != nil || (pkgURL.Scheme != "https" && pkgURL.Scheme != "http") {
		return "", fmt.Errorf("the release channel has an invalid package URL %q", ch.URL)
	}
	name := path.Base(pkgURL.Path)
	if !fileNameRE.MatchString(name) {
		return "", fmt.Errorf("the release channel points at %q, which is not a .mbu file", name)
	}
	dest := filepath.Join(dir, name)

	if ok, _ := fileMatches(dest, ch.Bytes, ch.SHA256); ok {
		return fmt.Sprintf("%s already downloaded (%s)", name, ch.Version), nil
	}
	if err := download(hc, pkgURL.String(), dest, ch.Bytes, ch.SHA256); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (%s, %s)", name, ch.Version, humanBytes(ch.Bytes)), nil
}

// download saves url to dest via a temporary file, and only renames it into
// place once its size and sha256 match.
func download(hc *http.Client, url, dest string, size int64, sum string) error {
	resp, err := hc.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.part")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	h := sha256.New()
	// One byte over the declared size is enough to know it is wrong.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, size+1))
	if err != nil {
		return fmt.Errorf("download failed: %v", err)
	}
	if n != size {
		return fmt.Errorf("download is %d bytes, the release channel says %d; not saved", n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return errors.New("download does not match the release channel's sha256; not saved")
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return err
	}
	keep = true
	return nil
}

// fileMatches reports whether p exists with exactly this size and sha256.
func fileMatches(p string, size int64, sum string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() != size {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == sum, nil
}
