// Package core_test holds the one rule that keeps the board offline-first:
// nothing under internal/core imports anything under internal/online. The
// online layer may use the core; the core never needs the online layer, so a
// board without it loses no board function.
package core_test

import (
	"os/exec"
	"strings"
	"testing"
)

const online = "github.com/LensBridge/agent/internal/online"

func TestCoreDoesNotImportOnline(t *testing.T) {
	out, err := exec.Command("go", "list", "-f",
		`{{.ImportPath}} {{join .Deps " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`,
		"github.com/LensBridge/agent/internal/core/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 10 {
		t.Fatalf("go list found only %d core packages:\n%s", len(lines), out)
	}
	for _, line := range lines {
		pkg, deps, _ := strings.Cut(line, " ")
		for _, d := range strings.Fields(deps) {
			if d == online || strings.HasPrefix(d, online+"/") {
				t.Errorf("%s imports %s: the core must not depend on the online layer", pkg, d)
			}
		}
	}
}
