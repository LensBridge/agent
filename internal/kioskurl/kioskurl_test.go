package kioskurl

import (
	"os"
	"path/filepath"
	"testing"
)

const id = "3f2a1b4c-0000-4000-8000-000000000001"

func TestCompose(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://board.example.com", "https://board.example.com?deviceId=" + id},
		{"https://board.example.com/", "https://board.example.com/?deviceId=" + id},
		{"https://board.example.com/?theme=dark", "https://board.example.com/?theme=dark&deviceId=" + id},
	}
	for _, tc := range cases {
		if got := Compose(tc.base, id); got != tc.want {
			t.Errorf("Compose(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestOfflineURL(t *testing.T) {
	want := "http://127.0.0.1:8080/?deviceId=" + id + "&mode=offline"
	if got := OfflineURL(id); got != want {
		t.Errorf("OfflineURL = %q, want %q", got, want)
	}
	// Not a real device id, but the query must stay well-formed regardless.
	if got := OfflineURL("a&b c"); got != "http://127.0.0.1:8080/?deviceId=a%26b+c&mode=offline" {
		t.Errorf("OfflineURL did not escape: %q", got)
	}
}

func TestWriteAndWriteOffline(t *testing.T) {
	dir := t.TempDir()
	board := filepath.Join(dir, "board-url")
	out := filepath.Join(dir, "kiosk-url")
	if err := os.WriteFile(board, []byte("  https://board.example.com \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Write(board, out, id); err != nil {
		t.Fatal(err)
	}
	assertFile(t, out, "https://board.example.com?deviceId="+id+"\n")

	if err := WriteOffline(out, id); err != nil {
		t.Fatal(err)
	}
	assertFile(t, out, "http://127.0.0.1:8080/?deviceId="+id+"&mode=offline\n")

	// Switching back needs nothing restored: board-url was never touched.
	if err := Write(board, out, id); err != nil {
		t.Fatal(err)
	}
	assertFile(t, out, "https://board.example.com?deviceId="+id+"\n")
}

func TestWriteRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "kiosk-url")
	if err := WriteOffline(out, ""); err == nil {
		t.Error("WriteOffline accepted an empty device id")
	}
	board := filepath.Join(dir, "board-url")
	_ = os.WriteFile(board, []byte("\n"), 0o644)
	if err := Write(board, out, id); err == nil {
		t.Error("Write accepted an empty board-url")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("kiosk-url written despite errors")
	}
}

func assertFile(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(p), got, want)
	}
}
