package agentkit

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ⭐ AN ANCHOR MUST POINT AT THE RIGHT SYMBOL — not merely at SOME declaration.
//
// TestREADMEAnchorsResolve checks a line's SHAPE ("does this look like a declaration?"). That passes
// any anchor landing on any declaration, so a row can point at the wrong symbol and be reported green.
//
// ⚠️ IT HAPPENED, AND IT REACHED THE READER. 49 anchors pointed at the wrong symbol while the shape
// guard said "0 broken" — and the harness rows printed `TB`, a type renamed to TestingT, in five
// signature cells. Copy-pasting a documented signature was a COMPILE ERROR.
//
// This test closes that: when a row NAMES a symbol in its first cell, the anchored line must declare
// that symbol. Rows naming no symbol (prose, or a deliberate anchor at a trap's implementation line)
// are skipped rather than forced.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

var (
	anchorRE = regexp.MustCompile(`([a-z_]+\.go):(\d+)`)
	// A row documenting two enum members at once ("`RungAsk` / `RungAuto`") anchors at the block.
	twoSymbols = regexp.MustCompile("` / `")
)

func TestREADMEAnchorsPointAtTheNamedSymbol(t *testing.T) {
	src, err := os.ReadFile(referenceDoc)
	if err != nil {
		// NOT a skip: a silent skip is how a drift guard stops guarding.
		t.Fatalf("%s unreadable: %v", referenceDoc, err)
	}

	cache := map[string][]string{}
	readLines := func(name string) []string {
		if l, ok := cache[name]; ok {
			return l
		}
		b, err := os.ReadFile(name)
		if err != nil {
			cache[name] = nil
			return nil
		}
		l := strings.Split(string(b), "\n")
		cache[name] = l
		return l
	}

	checked, wrong := 0, 0
	for _, row := range strings.Split(string(src), "\n") {
		m := anchorRE.FindStringSubmatch(row)
		if m == nil {
			continue
		}
		sym := rowSymbol(row)
		if sym == "" || twoSymbols.MatchString(rowFirstCell(row)) {
			continue // prose row, or one documenting a pair — nothing single to verify
		}
		// A row may deliberately anchor at the LINE THAT IMPLEMENTS a trap rather
		// than at a declaration — "a verifier error discards the verdict" wants the
		// discard, not func Pair. Such a row marks itself by bolding the behaviour;
		// requiring a declaration there would force a WORSE anchor.
		if strings.Contains(row, "**") && !strings.HasPrefix(rowFirstCell(row), "`") {
			continue
		}
		lines := readLines(m[1])
		n, _ := strconv.Atoi(m[2])
		if lines == nil || n < 1 || n > len(lines) {
			continue // the shape guard already reports out-of-range anchors
		}
		checked++
		line := lines[n-1]
		// An enum member may anchor at its doc comment, which is what the reader wants — so accept the
		// name appearing anywhere on the line. A WRONG symbol still fails: the name will not be there.
		if !declaresSymbol(line, sym) && !strings.Contains(line, sym) {
			wrong++
			t.Errorf("⛔ %s:%d does not declare %q — the anchor points at the WRONG SYMBOL:\n    %s",
				m[1], n, sym, strings.TrimSpace(line))
		}
	}
	if checked == 0 {
		t.Fatal("no symbol-bearing anchors found — the row parser is broken, not the README")
	}
	t.Logf("checked %d symbol-bearing anchors, %d wrong", checked, wrong)
}

// rowFirstCell returns a markdown row's first cell, trimmed of decoration.
func rowFirstCell(row string) string {
	cells := strings.Split(row, "|")
	if len(cells) < 2 {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cells[1]), "⭐"))
}

// rowSymbol extracts the Go symbol a row names — `Foo`, `Foo.Bar` or `Foo[S]` → "Foo"/"Bar"/"Foo".
// Returns "" when the row names none.
//
// ⚠️ IT MUST LOOK PAST THE FIRST CELL. An earlier version required the FIRST cell
// to be a backticked symbol, so any row whose first cell is prose was skipped
// entirely — and that silently exempted the most important rows in the document:
// the loop table, whose first cell describes the JOB ("draft→critique→revise")
// and whose second names `Pair`. 15 of 115 anchors sat in that blind spot,
// including RunOnce, RunOnceText, Pair and RunToolLoop. Repointing the `Pair` row
// at an unrelated doc comment passed both guards.
func rowSymbol(row string) string {
	first := rowFirstCell(row)
	if !strings.HasPrefix(first, "`") {
		// Fall back to the first backticked identifier in any LATER cell, skipping
		// the anchor cell itself (`loop.go:105`) — a path, not a symbol.
		cells := strings.Split(row, "|")
		if len(cells) < 3 {
			return "" // prose, not a table row
		}
		for _, cell := range cells[2:] {
			for _, m := range backtickIdent.FindAllStringSubmatch(cell, -1) {
				if strings.HasSuffix(m[1], ".go") {
					continue
				}
				return normalizeSymbol(m[1])
			}
		}
		return ""
	}
	sym := strings.Trim(first, "`⭐ ")
	if i := strings.LastIndex(sym, "."); i >= 0 {
		sym = sym[i+1:]
	}
	if i := strings.Index(sym, "["); i >= 0 {
		sym = sym[:i]
	}
	return sym
}

// backtickIdent matches a backticked Go identifier (optionally qualified or
// generic), so a symbol can be recovered from any cell of a row.
var backtickIdent = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_.]*)(?:\\[[^`]*\\])?(?:\\([^`]*\\))?`")

// normalizeSymbol reduces Foo.Bar → Bar and Foo[S] → Foo, matching rowSymbol.
func normalizeSymbol(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, "["); i >= 0 {
		s = s[:i]
	}
	return s
}

// declaresSymbol reports whether a line declares the named symbol, in the forms Go actually uses.
func declaresSymbol(line, name string) bool {
	t := strings.TrimSpace(line)
	for _, p := range []string{
		"func " + name + "(", "func " + name + "[",
		"type " + name + " ", "type " + name + "[",
		"const " + name + " ", "var " + name + " ",
		name + " ", name + "(", name + ",",
	} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	if strings.HasPrefix(t, "func (") { // a method: func (r T) Name(
		if i := strings.Index(t, ") "); i > 0 {
			rest := t[i+2:]
			return strings.HasPrefix(rest, name+"(") || strings.HasPrefix(rest, name+"[")
		}
	}
	return false
}
