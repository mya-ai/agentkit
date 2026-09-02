package agentkit_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestREADMESampleCompiles extracts the `package main` sample from README.md and
// actually builds it against this module.
//
// ⚠️ THE FAILURE THIS PREVENTS, WHICH SHIPPED. The README's flagship sample — the
// first code any reader sees — referenced two undefined symbols (`myCaller`,
// `escalate`) and omitted its import block. Pasted verbatim it did not compile.
// Every other sample in the repo was CI-guarded; this one was not, so nothing
// caught it.
//
// ⭐ That gap matters most for the reader who cannot ask a follow-up question. A
// human reads `myCaller` as "your thing goes here"; a coding agent resolves an
// undefined symbol by INVENTING a definition, then fails again somewhere less
// obvious. The first sample has to be the one that runs.
//
// The test builds rather than runs: compilation is the contract, and a sample
// that needs a live model would not be a good sample.
func TestREADMESampleCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a temp module; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	src, err := os.ReadFile("README.md")
	if err != nil {
		// NOT a skip: a silent skip is how a guard stops guarding.
		t.Fatalf("README.md unreadable: %v", err)
	}

	block := regexp.MustCompile("(?s)```go\\n(package main\\n.*?)```").FindSubmatch(src)
	if block == nil {
		t.Fatal("no ```go package main``` block in README.md — the sample is gone, or the fence changed")
	}
	sample := string(block[1])

	// The module under test, so the sample builds against THIS source tree.
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("main.go", sample)
	write("go.mod", "module readmesample\n\ngo 1.25\n\nrequire github.com/mya-ai/agentkit v0.0.0\n\nreplace github.com/mya-ai/agentkit => "+root+"\n")

	// GOFLAGS=-mod=mod so the temp module can resolve; the sample must need no
	// dependency beyond the kit itself.
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("⛔ the README sample does not compile — a reader pasting it gets these errors:\n%s\n\nsample was:\n%s",
			strings.TrimSpace(string(out)), sample)
	}
}
