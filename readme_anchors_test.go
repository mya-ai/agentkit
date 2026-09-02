package agentkit

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestREADMEAnchorsResolve is the guard on the doc rule this package states about itself:
// "Every anchor is file:line in pkg/agentkit/, verified against source."
//
// ⚠️ THAT CLAIM WAS FALSE. A sweep found 12 of 100 anchors pointing at the wrong line — drifted by
// later edits to the very files they index. Density signals authority, so a coding agent copies a
// dense table CONFIDENTLY: a wrong file:line is worse than no file:line, because it sends the reader
// somewhere plausible and wrong.
//
// This asserts each anchor lands on something that could BE a declaration (a type/func/const, a doc
// comment, or a struct field) rather than mid-expression. It cannot check that the anchor names the
// RIGHT symbol — but it catches the whole-file drift that actually happened, every time, in CI.
func TestREADMEAnchorsResolve(t *testing.T) {
	src, err := os.ReadFile(referenceDoc)
	if err != nil {
		// NOT a skip: a silent skip is how a drift guard stops guarding.
		t.Fatalf("%s unreadable: %v", referenceDoc, err)
	}

	// A declaration line, a doc comment, an indented exported field / enum member — or an `if` guard,
	// because some anchors deliberately point at the LINE THAT IMPLEMENTS a trap (e.g. the early
	// return that makes the object-naming guarantee no-op on an empty Label). Pointing at the code
	// rather than its doc comment is correct there; the reader wants to see the condition.
	decl := regexp.MustCompile(`^(type|func|const|var|//|\s+// |\s+if |\s+[A-Z][A-Za-z]*)`)
	anchor := regexp.MustCompile(`([a-z_]+\.go):(\d+)`)

	files := map[string][]string{}
	readLines := func(name string) []string {
		if l, ok := files[name]; ok {
			return l
		}
		f, err := os.Open(name)
		if err != nil {
			files[name] = nil
			return nil
		}
		defer func() { _ = f.Close() }()
		var out []string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			out = append(out, sc.Text())
		}
		files[name] = out
		return out
	}

	checked, broken := 0, 0
	for _, m := range anchor.FindAllStringSubmatch(string(src), -1) {
		lines := readLines(m[1])
		if lines == nil {
			continue // an anchor into another package; not ours to check
		}
		n, _ := strconv.Atoi(m[2])
		checked++
		if n < 1 || n > len(lines) {
			broken++
			t.Errorf("%s:%d is past the end of the file (%d lines)", m[1], n, len(lines))
			continue
		}
		if line := lines[n-1]; !decl.MatchString(line) {
			broken++
			t.Errorf("%s:%d does not look like a declaration — the anchor has DRIFTED:\n    %s",
				m[1], n, strings.TrimSpace(line))
		}
	}

	if checked == 0 {
		t.Fatal("no anchors found — the regex or the README changed shape; this guard is now vacuous")
	}
	t.Logf("checked %d anchors, %d broken", checked, broken)
}
