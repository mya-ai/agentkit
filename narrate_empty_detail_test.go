package agentkit

import "testing"

// ⭐ AN ENTRY WITH NO DETAIL IS A ROW THAT SAYS NOTHING.
//
// The narration document is rendered as a stepwise activity stream — one line per entry. An entry
// carrying an action but an empty detail renders as a BLANK ROW: the user sees the list grow while
// being told nothing, which reads as the agent stuttering rather than working.
//
// Observed in a real compose run: three entries, of which two were {action:"once", detail:""}.
//
// ⛔ SUPPRESSED AT THE SOURCE, DELIBERATELY, rather than filtered per client. The SAME document
// feeds the composer, /today's todos and the project surfaces. Filtering in one client leaves the
// blank row rendering in the others — and then two surfaces disagree about what an entry means.
// One document, one rule.
func TestSettledEmptyDetailEntriesAreNotAppended(t *testing.T) {
	n := &Narrator{}
	st := &narratorState{}

	// A SETTLED entry with no detail will never be filled in — a permanent blank row.
	n.appendDone(st, "once", "")
	n.appendDone(st, "once", "   ") // whitespace is not detail
	if len(st.doc.Entries) != 0 {
		t.Fatalf("appended %d settled entries with no detail — each is a permanent blank row",
			len(st.doc.Entries))
	}

	// ⚠️ AN ACTIVE ENTRY IS DIFFERENT, and this distinction is the whole correctness of the guard.
	// turn_start legitimately appends an active entry with NO detail, which the pacer or summarizer
	// then revises in place. Suppressing that would delete the live "the agent is working" row — the thing
	// the narration exists to show. My first version of this guard did exactly that and broke six
	// existing narrator tests, which is how the distinction was found.
	n.appendActive(st, "thinking", "")
	if len(st.doc.Entries) != 1 {
		t.Fatalf("an ACTIVE entry with no detail must still land (the pacer fills it): %d entries",
			len(st.doc.Entries))
	}

	// And a real settled entry must still land — a guard that drops everything would "fix" the blank
	// row by deleting the narration.
	n.appendDone(st, "gathered", "Looked through your email and calendar.")
	if len(st.doc.Entries) != 2 {
		t.Fatalf("a real settled entry did not land (%d entries)", len(st.doc.Entries))
	}
	if st.doc.Entries[1].Detail == "" {
		t.Error("the surviving entry lost its detail")
	}
}
