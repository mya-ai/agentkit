package agentkit

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// AGENTS.md tells an AI coding agent it may cite an anchor WITHOUT opening the file. That promise is
// only worth what a guard enforces — an unguarded anchor rots on the next refactor, and the reader
// most likely to trust it is the one least likely to check. Same reasoning as the reference's guards.
//
// AGENTS.md uses markdown links, [bind.go:112](bind.go#L112), so BOTH halves are asserted: the
// visible line number and the link target must agree, and the line must declare the named symbol.
func TestAGENTSAnchorsResolve(t *testing.T) {
	src, err := os.ReadFile(agentsDoc)
	if err != nil {
		// NOT a skip: a silent skip is how a drift guard stops guarding.
		t.Fatalf("%s unreadable: %v", agentsDoc, err)
	}

	// [file.go:NN](file.go#LNN) — capture file, visible line, link file, link line.
	link := regexp.MustCompile(`\[([a-z_]+\.go):(\d+)\]\(([a-z_]+\.go)#L(\d+)\)`)

	cache := map[string][]string{}
	lines := func(name string) []string {
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

	checked := 0
	for _, m := range link.FindAllStringSubmatch(string(src), -1) {
		file, shown, target, linked := m[1], m[2], m[3], m[4]

		if file != target || shown != linked {
			t.Errorf("%s: link text %s:%s disagrees with its target %s#L%s — one of them is wrong",
				agentsDoc, file, shown, target, linked)
			continue
		}

		n, _ := strconv.Atoi(shown)
		src := lines(file)
		if src == nil {
			t.Errorf("%s: anchor names %s, which does not exist", agentsDoc, file)
			continue
		}
		if n < 1 || n > len(src) {
			t.Errorf("%s: %s:%d is past end of file (%d lines)", agentsDoc, file, n, len(src))
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(src[n-1]), "func ") &&
			!strings.HasPrefix(strings.TrimSpace(src[n-1]), "type ") {
			t.Errorf("%s: %s:%d should declare a symbol, got: %s", agentsDoc, file, n, src[n-1])
		}
		checked++
	}

	if checked == 0 {
		t.Fatalf("%s: no anchors found — the guard is not guarding anything (did the link format change?)",
			agentsDoc)
	}
	t.Logf("checked %d anchors in %s", checked, agentsDoc)
}

// The constructor table is the decision AGENTS.md says is easiest to get wrong. If a constructor is
// renamed or removed, the table must not keep naming it.
func TestAGENTSNamesRealConstructors(t *testing.T) {
	src, err := os.ReadFile(agentsDoc)
	if err != nil {
		t.Fatalf("%s unreadable: %v", agentsDoc, err)
	}
	for _, name := range []string{"Bind", "BindMixed", "NewActionSet", "NewToolSet"} {
		if !strings.Contains(string(src), name) {
			t.Errorf("%s no longer mentions %s — the constructor table has drifted", agentsDoc, name)
		}
	}
}
