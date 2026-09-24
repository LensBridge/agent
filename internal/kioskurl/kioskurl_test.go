package kioskurl

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteLocal(t *testing.T) {
	out := filepath.Join(t.TempDir(), "kiosk-url")
	if err := WriteLocal(out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != LocalURL+"\n" {
		t.Fatalf("kiosk-url = %q", got)
	}
	fi, _ := os.Stat(out)
	// An identical rewrite must not touch the file: the kiosk .path watcher
	// would restart the browser.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(out, old, old)
	if err := WriteLocal(out); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(out)
	if !fi2.ModTime().Equal(old) || fi.Mode().Perm() != 0o644 {
		t.Fatalf("rewrote an unchanged kiosk-url, or wrong mode %v", fi.Mode())
	}
}

func TestWriteLocalRejectsEmpty(t *testing.T) {
	if err := WriteLocal(""); err == nil {
		t.Fatal("expected an error")
	}
}

