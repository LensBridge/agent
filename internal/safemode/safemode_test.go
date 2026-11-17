package safemode

import "testing"

func TestSafeModeAfterRepeatedStarts(t *testing.T) {
	m := New(t.TempDir())
	for i := 1; i <= safeModeThreshold; i++ {
		if m.OnStartup() {
			t.Fatalf("safe mode after %d starts", i)
		}
	}
	if !m.OnStartup() {
		t.Fatal("no safe mode after repeated starts")
	}
	m.OnSuccessfulRun()
	if m.OnStartup() {
		t.Fatal("safe mode after a stable run")
	}
}
