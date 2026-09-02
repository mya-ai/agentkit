package agentkit

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestGuardedDocPathsExist asserts that every documentation path a guard reads is
// spelled EXACTLY as git records it.
//
// ⚠️ WHY EXACTLY, AND NOT JUST "os.Stat succeeds". macOS is case-insensitive, so
// after docs/REFERENCE.md was renamed to docs/reference.md the old constant kept
// resolving to the new file and every guard stayed green — locally. CI runs on
// Linux, which is case-sensitive, where the stale spelling is a missing file and
// the guard fails with "unreadable" rather than with anything about the rename.
//
// ⭐ A green suite on a case-insensitive filesystem is not evidence the paths are
// right. This compares against `git ls-files`, which stores one canonical spelling
// regardless of the filesystem underneath.
func TestGuardedDocPathsExist(t *testing.T) {
	// Every tracked path, not just docs/ — a guarded doc may live at the repository root
	// (AGENTS.md does). Scoping this to "docs" made a root-level path unfindable and so
	// permanently untracked-looking, which is a guard that fails for the wrong reason.
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		t.Skipf("git unavailable (%v) — cannot check canonical spelling", err)
	}
	tracked := make(map[string]bool)
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		tracked[p] = true
	}
	if len(tracked) == 0 {
		t.Skip("no tracked files — nothing to check")
	}

	for _, p := range docPaths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("⛔ a guard reads %q, which does not exist: %v", p, err)
			continue
		}
		if !tracked[p] {
			t.Errorf("⛔ a guard reads %q, but git tracks no such path. On a case-insensitive\n"+
				"    filesystem this still opens the right file, so the suite is green here and\n"+
				"    RED on a case-sensitive CI runner. Match git's spelling exactly.", p)
		}
	}
}
