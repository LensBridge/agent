package offline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractBundleValidation(t *testing.T) {
	const first = "2026-09-24"
	cases := []struct {
		name    string
		mutate  func(b *bundleSpec)
		wantErr string // "" means the bundle must be accepted
	}{
		{name: "valid", mutate: func(*bundleSpec) {}},
		{name: "directory entries for the allowed dirs are fine", mutate: func(b *bundleSpec) {
			b.extra = append(b.extra, zipEntry{"payloads/", nil}, zipEntry{"media/", nil})
		}},

		// Zip-slip and friends: none of these may be joined onto a path.
		{name: "parent traversal", mutate: addEntry("../evil.json"), wantErr: "not allowed"},
		{name: "deep traversal", mutate: addEntry("../../../../etc/cron.d/x"), wantErr: "not allowed"},
		{name: "absolute path", mutate: addEntry("/etc/passwd"), wantErr: "not allowed"},
		{name: "traversal inside payloads", mutate: addEntry("payloads/../../x.json"), wantErr: "not allowed"},
		{name: "traversal inside media", mutate: addEntry("media/../../" + sha(testImage) + ".jpg"), wantErr: "not allowed"},
		{name: "backslash separator", mutate: addEntry(`payloads\2026-09-24.json`), wantErr: "not allowed"},
		{name: "windows drive", mutate: addEntry(`C:/x.json`), wantErr: "not allowed"},
		{name: "unknown top-level dir", mutate: addEntry("assets/app.js"), wantErr: "not allowed"},
		{name: "nested payload dir", mutate: addEntry("payloads/x/2026-09-24.json"), wantErr: "not allowed"},
		{name: "wrong manifest case", mutate: addEntry("Manifest.json"), wantErr: "not allowed"},
		{name: "media name not a sha", mutate: addEntry("media/poster.jpg"), wantErr: "not allowed"},
		{name: "uppercase sha", mutate: addEntry("media/" + strings.ToUpper(sha(testImage)) + ".jpg"), wantErr: "not allowed"},
		{name: "duplicate payload", mutate: func(b *bundleSpec) {
			b.extra = append(b.extra, zipEntry{"payloads/" + first + ".json", b.payloads[first]})
		}, wantErr: "twice"},

		{name: "no manifest", mutate: func(b *bundleSpec) { b.noManifest = true }, wantErr: "no manifest.json"},
		{name: "unknown formatVersion", mutate: func(b *bundleSpec) { b.manifest.FormatVersion = 2 }, wantErr: "formatVersion 2"},
		{name: "missing formatVersion", mutate: func(b *bundleSpec) { b.manifest.FormatVersion = 0 }, wantErr: "formatVersion 0"},
		{name: "wrong device", mutate: func(b *bundleSpec) { b.manifest.DeviceID = "someone-else" }, wantErr: "for device someone-else"},
		{name: "unknown timezone", mutate: func(b *bundleSpec) { b.manifest.Timezone = "Mars/Olympus_Mons" }, wantErr: "timezone"},
		{name: "empty timezone", mutate: func(b *bundleSpec) { b.manifest.Timezone = "" }, wantErr: "timezone"},
		{name: "Local is not a timezone", mutate: func(b *bundleSpec) { b.manifest.Timezone = "Local" }, wantErr: "timezone"},
		{name: "bad generatedAt", mutate: func(b *bundleSpec) { b.manifest.GeneratedAt = "yesterday" }, wantErr: "generatedAt"},
		{name: "lastDay before firstDay", mutate: func(b *bundleSpec) { b.manifest.LastDay = "2026-09-01" }, wantErr: "before firstDay"},

		{name: "missing day", mutate: func(b *bundleSpec) { delete(b.payloads, "2026-09-26") }, wantErr: "missing payloads/2026-09-26.json"},
		{name: "extra day after range", mutate: func(b *bundleSpec) {
			b.payloads["2026-09-28"] = payloadFor("2026-09-28", "")
		}, wantErr: "payloads/2026-09-28.json, outside its range"},
		{name: "extra day before range", mutate: func(b *bundleSpec) {
			b.payloads["2026-09-23"] = payloadFor("2026-09-23", "")
		}, wantErr: "outside its range"},
		{name: "impossible date", mutate: func(b *bundleSpec) {
			b.payloads["2026-13-45"] = []byte(`{}`)
		}, wantErr: "outside its range"},
		{name: "payload is an array", mutate: func(b *bundleSpec) { b.payloads[first] = []byte(`[]`) }, wantErr: "not a JSON object"},
		{name: "payload is not JSON", mutate: func(b *bundleSpec) { b.payloads[first] = []byte(`{"frames":`) }, wantErr: "not valid JSON"},
		{name: "payload has trailing data", mutate: func(b *bundleSpec) { b.payloads[first] = []byte(`{} {}`) }, wantErr: "trailing"},

		{name: "hash mismatch", mutate: func(b *bundleSpec) {
			// Same size, different bytes: only the hash can catch it.
			bad := append([]byte(nil), testImage...)
			bad[len(bad)-1] ^= 0xff
			b.media["media/"+sha(testImage)+".jpg"] = bad
		}, wantErr: "corrupt"},
		{name: "size mismatch", mutate: func(b *bundleSpec) { b.manifest.Media[0].Bytes++ }, wantErr: "the manifest says"},
		{name: "manifest media missing from zip", mutate: func(b *bundleSpec) {
			b.media = map[string][]byte{}
		}, wantErr: "does not contain it"},
		{name: "media not in manifest", mutate: func(b *bundleSpec) {
			other := []byte("other")
			b.media["media/"+sha(other)+".png"] = other
		}, wantErr: "does not list"},
		{name: "manifest sha disagrees with file name", mutate: func(b *bundleSpec) {
			b.manifest.Media[0].SHA256 = sha([]byte("x"))
		}, wantErr: "does not match its file name"},
		{name: "posterUrl names missing media", mutate: func(b *bundleSpec) {
			b.payloads["2026-09-25"] = payloadFor("2026-09-25", "/media/"+sha([]byte("gone"))+".jpg")
		}, wantErr: "not in the bundle"},
		{name: "posterUrl still on the internet", mutate: func(b *bundleSpec) {
			b.payloads["2026-09-25"] = payloadFor("2026-09-25", "https://cdn.example.com/p.jpg")
		}, wantErr: "not a /media/ path"},
		{name: "posterUrl traversal", mutate: func(b *bundleSpec) {
			b.payloads["2026-09-25"] = payloadFor("2026-09-25", "/media/../manifest.json")
		}, wantErr: "not in the bundle"},
		{name: "empty posterUrl is allowed", mutate: func(b *bundleSpec) {
			b.payloads["2026-09-25"] = payloadFor("2026-09-25", "")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBundle(first, 3, "2026-09-24T14:02:11Z")
			tc.mutate(b)
			zipPath := b.write(t, t.TempDir())

			stage := t.TempDir()
			m, err := extractBundle(zipPath, stage, testDevice)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected a valid bundle: %v", err)
				}
				if m.FirstDay != first || len(m.Days()) != 3 {
					t.Errorf("manifest = %+v", m)
				}
				got := listDir(t, filepath.Join(stage, "payloads"))
				if len(got) != 3 {
					t.Errorf("payloads extracted = %v", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted a bad bundle; want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want containing %q", err, tc.wantErr)
			}
			// Whatever happened, nothing may have been written outside stage.
			assertOnlyUnder(t, filepath.Dir(zipPath), filepath.Base(zipPath))
		})
	}
}

func addEntry(name string) func(*bundleSpec) {
	return func(b *bundleSpec) { b.extra = append(b.extra, zipEntry{name, []byte(`{}`)}) }
}

// assertOnlyUnder checks dir holds exactly the named entries.
func assertOnlyUnder(t *testing.T, dir string, want ...string) {
	t.Helper()
	got := listDir(t, dir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s holds %v, want %v", dir, got, want)
	}
}

func TestExtractBundleRejectsNonZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.zip")
	if err := os.WriteFile(p, []byte("definitely not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBundle(p, t.TempDir(), testDevice); err == nil || !strings.Contains(err.Error(), "not a readable zip") {
		t.Errorf("err = %v", err)
	}
}

func TestExtractBundleWindowLimit(t *testing.T) {
	b := newBundle("2026-09-01", 32, "2026-09-01T00:00:00Z")
	if _, err := extractBundle(b.write(t, t.TempDir()), t.TempDir(), testDevice); err == nil || !strings.Contains(err.Error(), "at most 31") {
		t.Errorf("err = %v", err)
	}
	b = newBundle("2026-09-01", 31, "2026-09-01T00:00:00Z")
	if _, err := extractBundle(b.write(t, t.TempDir()), t.TempDir(), testDevice); err != nil {
		t.Errorf("31 days rejected: %v", err)
	}
}
