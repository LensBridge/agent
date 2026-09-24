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

func TestWriteForBoardFallsBackToHostedUntilAppInstalled(t *testing.T) {
	dir := t.TempDir()
	out, board := filepath.Join(dir, "kiosk-url"), filepath.Join(dir, "board-url")
	os.WriteFile(board, []byte("https://board.example\n"), 0o644)

	u, err := WriteForBoard(out, board, "abc", false)
	if err != nil || u != "https://board.example?deviceId=abc" {
		t.Fatalf("no app: %q %v", u, err)
	}
	if u, _ := WriteForBoard(out, board, "abc", true); u != LocalURL {
		t.Fatalf("app installed: %q", u)
	}
	os.Remove(board)
	if u, _ := WriteForBoard(out, board, "abc", false); u != LocalURL {
		t.Fatalf("no board-url: %q", u)
	}
}
