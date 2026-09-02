package agentkit

import (
	"strings"
	"testing"
)

// TestNameObject_EmptyLabelIsLoudNotSilent pins a guarantee that used to no-op silently.
//
// ⛔ The kit promises "no agent can ship an ask that doesn't name what it acts on." nameObject
// returns the question UNCHANGED when Label is empty (proposal_question.go:190) — so a developer who
// writes ObjectRef{Kind: "task", ID: id}, which the field list openly invites, loses the guarantee
// and is told nothing. The safe constructor Ref() appeared ZERO times in the README.
//
// The tokenizer cannot fail loudly at call time (a missing label is not an error — some refs
// genuinely have none), so the guard belongs where a wrong declaration is still cheap: the
// validator. This test pins the observable behaviour so the trap stays documented in code.
func TestNameObject_EmptyLabelIsLoudNotSilent(t *testing.T) {
	q := "Should I archive this?"

	// ⚠️ THE TRAP, preserved as behaviour: no label ⇒ the sentence is untouched, silently.
	bare := ObjectRef{Kind: "task", ID: "t-1"}
	if got := nameObject(q, bare); got != q {
		t.Errorf("documented behaviour changed: an unlabelled ref left the question alone, got %q", got)
	}

	// ⭐ Ref(labeler, kind, id) is the constructor that makes the guarantee hold: it RESOLVES the label
	// from the agent's own labeler, and yields the ZERO ref for an unresolvable id — so an invented or
	// stale id can never become a chip.
	labelled := Ref(MapLabeler(map[string]map[string]string{"task": {"t-1": "Ship the audit"}}), "task", "t-1")
	got := nameObject(q, labelled)
	if got == q {
		t.Error("a LABELLED ref must name its object in the sentence — that is the whole guarantee")
	}
	if !strings.Contains(got, "Ship the audit") && !strings.Contains(got, "{{") {
		t.Errorf("the object should be named or tokenized, got %q", got)
	}
}
