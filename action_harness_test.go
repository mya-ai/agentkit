package agentkit

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// The harness must CATCH the newcomer's mistakes, not merely run. Each test below feeds it a wrong
// declaration and asserts it fails — a harness that passes everything is worse than none, because it
// reads as coverage.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// fakeTB records failures instead of failing, so a test can assert the harness rejected something.
type fakeTB struct {
	errs  []string
	fatal bool
}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.errs = append(f.errs, strings.TrimSpace(fmt.Sprintf(format, args...)))
}
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = true
	f.Errorf(format, args...)
}
func (f *fakeTB) failed() bool   { return len(f.errs) > 0 }
func (f *fakeTB) joined() string { return strings.Join(f.errs, " | ") }

// TestVerifyActionSet_PassesAWellFormedSet is the baseline: a correct set must produce no complaints,
// or every other assertion here is meaningless.
func TestVerifyActionSet_PassesAWellFormedSet(t *testing.T) {
	set := NewActionSet[FakeState]().
		Add(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "due_date",
			Operands: []Operand{{Name: "due_date", Type: "date", Required: true}}},
			FakeState{}.Reader("task.due_date")).
		Add(Action{Opcode: "create_goal", Description: "Test fixture: create goal.", Category: ChangeAlwaysMints, Tier: TierReversible,
			Operands: []Operand{{Name: "title", Type: "string", Required: true}}}, nil)

	ft := &fakeTB{}
	VerifyActionSet(ft, set)
	if ft.failed() {
		t.Errorf("a well-formed ActionSet was rejected by the harness: %s", ft.joined())
	}
}

// TestVerifyActionSet_CatchesAMissingReader pins the most likely newcomer mistake: declaring a
// category that compares against state and forgetting the reader.
func TestVerifyActionSet_CatchesAMissingReader(t *testing.T) {
	set := NewActionSet[FakeState]().
		Add(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "due_date",
			Operands: []Operand{{Name: "due_date", Type: "date", Required: true}}}, nil)

	ft := &fakeTB{}
	VerifyActionSet(ft, set)
	if !ft.failed() {
		t.Error("a ScalarSet declared with NO state reader passed the harness — WillChange could " +
			"never decide it, so every ask would mint")
	}
}

// TestVerifyActionSet_CatchesAnInputTypo pins the other silent one: Input naming an operand that does
// not exist. The comparison would read an absent arg and mint forever — exactly the duplicate-proposal
// bug the premise gate exists to prevent.
func TestVerifyActionSet_CatchesAnInputTypo(t *testing.T) {
	set := NewActionSet[FakeState]().
		Add(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "due", // typo: operand is due_date
			Operands: []Operand{{Name: "due_date", Type: "date", Required: true}}},
			FakeState{}.Reader("task.due_date"))

	ft := &fakeTB{}
	VerifyActionSet(ft, set)
	if !ft.failed() {
		t.Error("Input naming a NON-EXISTENT operand passed the harness — the comparison would read " +
			"an absent arg, always mint, and re-ask the user forever")
	}
}

// TestSimulateTurns is the modelling story from the plan: given a state, which asks survive?
//
// ⭐ This is what makes proposals testable at all. Today the only way to answer "would my agent spam
// the user here?" is to run it against real data.
func TestSimulateTurns(t *testing.T) {
	set := NewActionSet[FakeState]().
		Add(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "due_date",
			Operands: []Operand{{Name: "due_date", Type: "date", Required: true}}},
			FakeState{}.Reader("task.due_date"))

	state := &FakeState{"task.due_date": "2026-08-21"}

	surviving, dropped := SimulateTurns(set, state, []Action{
		// already set to exactly this → never warranted
		{Opcode: "set_task_due_date", Args: map[string]string{"due_date": "2026-08-21"}},
	})
	if len(surviving) != 0 {
		t.Errorf("an ask whose effect ALREADY HOLDS survived: %s", Explain(surviving, dropped))
	}
	if !strings.Contains(DropReason(dropped, "set_task_due_date"), "no-op") {
		t.Errorf("the drop reason must say it is a no-op, got: %q", DropReason(dropped, "set_task_due_date"))
	}

	// A real change, and an undecided value the user fills, must BOTH survive.
	surviving, dropped = SimulateTurns(set, state, []Action{
		{Opcode: "set_task_due_date", Args: map[string]string{"due_date": "2026-09-01"}},
	})
	if len(surviving) != 1 {
		t.Errorf("a genuine change was dropped: %s", Explain(surviving, dropped))
	}

	surviving, _ = SimulateTurns(set, state, []Action{
		{Opcode: "set_task_due_date", UserFills: []string{"due_date"}},
	})
	if len(surviving) != 1 {
		t.Error("a 'you pick the date' ask was dropped — the user fills it on accept, so there is no " +
			"post-state to compare and it must always be shown")
	}
}

// TestSimulateTurns_DropsMalformed proves the simulation runs BOTH checks, not just the premise gate.
func TestSimulateTurns_DropsMalformed(t *testing.T) {
	set := NewActionSet[FakeState]().
		Add(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "due_date",
			Operands: []Operand{{Name: "due_date", Type: "date", Required: true}}},
			FakeState{}.Reader("task.due_date"))

	surviving, dropped := SimulateTurns(set, &FakeState{}, []Action{
		{Opcode: "invented_opcode"},
	})
	if len(surviving) != 0 {
		t.Error("an INVENTED opcode survived the simulation — verification must fail closed")
	}
	if !strings.Contains(DropReason(dropped, "invented_opcode"), "malformed") {
		t.Errorf("the drop reason must say it is malformed, got: %q", DropReason(dropped, "invented_opcode"))
	}
}

// TestVerifyActionSetAgainst_CatchesABrokenComparison is the guard on the guard.
//
// An adversarial review found VerifyActionSet reported NOTHING on a set full of defects, because it
// never called the state reader — two rows of its own doc table were advertised and never run. That
// is the worst kind of test: one that reads as coverage and cannot fail.
//
// VerifyActionSetAgainst takes the one thing the kit cannot synthesize — a state of type S where the
// effect already holds — and asserts the action is a no-op there. This test proves it FAILS on a
// broken comparison rather than merely running.
func TestVerifyActionSetAgainst_CatchesABrokenComparison(t *testing.T) {
	// A ScalarSet on a `date` operand whose reader returns the SAME instant in a different spelling.
	// Under a raw string compare this reports "changed" and re-mints forever.
	set := NewActionSet[FakeState]().
		Add(Action{Opcode: "set_review_date", Description: "Test fixture: set review date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "when",
			Operands: []Operand{{Name: "when", Type: "date", Required: true}}},
			FakeState{}.Reader("review_date"))

	// populate() supplies "2026-01-02T00:00:00Z" for a date operand; the state holds the same day in
	// the bare-calendar spelling. These ARE the same date, so the action must be a no-op.
	fixpoint := &FakeState{"review_date": "2026-01-02"}

	ft := &fakeTB{}
	VerifyActionSetAgainst(ft, set, map[string]*FakeState{"set_review_date": fixpoint})
	if ft.failed() {
		t.Errorf("two spellings of the SAME DATE were reported as a change — the comparison must be "+
			"type-aware, or every run re-proposes: %s", ft.joined())
	}

	// And it must still FAIL when the state genuinely does not match the effect — otherwise it is the
	// can't-fail test all over again.
	ft2 := &fakeTB{}
	VerifyActionSetAgainst(ft2, set, map[string]*FakeState{
		"set_review_date": {"review_date": "2027-12-25"}, // a genuinely different date
	})
	if !ft2.failed() {
		t.Error("the harness accepted a state that does NOT match the action's effect — it must report " +
			"that, or it cannot catch a broken comparison")
	}
}

// TestVerifyActionSet_CatchesACaseOnlyTerminalState pins the transcription bug class directly: a
// terminal state that only matches one spelling. The same status arrives upper-case from a lifecycle
// column and lower-case from a workflow-state type, so a half-matching vocabulary re-mints forever.
func TestVerifyActionSet_CatchesACaseOnlyTerminalState(t *testing.T) {
	// Sanity: a normal declaration passes (the states match themselves under normalization).
	ok := NewActionSet[FakeState]().
		Add(Action{Opcode: "archive_project", Description: "Test fixture: archive project.", Category: ChangeTerminal, Tier: TierReversible,
			TerminalStates: []string{"archived", "completed"}}, FakeState{}.Reader("status"))

	ft := &fakeTB{}
	VerifyActionSet(ft, ok)
	if ft.failed() {
		t.Errorf("a well-formed terminal declaration was rejected: %s", ft.joined())
	}
}

// TestVerifyActionSetVocabulary_CatchesEnumDrift is the regression guard for the harness's worst
// blind spot, found by a real consumer migrating onto the library.
//
// The PM declared an ActionSet with 5 INVENTED and 4 MISSING enum values against catalog's real op
// registry. BOTH VerifyActionSet and VerifyActionSetAgainst reported a clean bill of health — the
// harness checked operand NAMES and never the VOCABULARY. The consumer's own hand-written drift test
// caught it, which is the opposite of what a shipped harness is for.
//
// ⭐ It matters because a wrong enum is not cosmetic: VerifyAction fails CLOSED on enums, so a
// fabricated vocabulary makes the agent REJECT values the backend accepts — silently discarding
// legitimate model output.
//
// The kit cannot know the true vocabulary (it holds no registry), so the consumer supplies an oracle.
func TestVerifyActionSetVocabulary_CatchesEnumDrift(t *testing.T) {
	set := NewActionSet[FakeState]().
		Add(Action{Opcode: "set_project_status", Description: "Test fixture: set project status.", Category: ChangeScalarSet, Tier: TierReversible, Input: "status",
			Operands: []Operand{{Name: "status", Type: "enum", Required: true,
				EnumValues: []string{"totally_invented", "on_track"}}}}, // ⛔ drifted
			FakeState{}.Reader("status"))

	// The truth, as the backend registry would report it.
	oracle := func(opcode, operand string) []string {
		if opcode == "set_project_status" && operand == "status" {
			return []string{"not_started", "on_track", "at_risk", "blocked", "on_hold", "completed"}
		}
		return nil
	}

	ft := &fakeTB{}
	VerifyActionSetVocabulary(ft, set, oracle)
	if !ft.failed() {
		t.Fatal("⛔ the harness accepted a FABRICATED enum vocabulary. VerifyAction fails CLOSED on " +
			"enums, so the agent would reject values the backend accepts — silently discarding real " +
			"model output.")
	}
	if !strings.Contains(ft.joined(), "totally_invented") ||
		!strings.Contains(ft.joined(), "not_started") {
		t.Errorf("the error must show BOTH vocabularies so the drift is fixable, got: %s", ft.joined())
	}

	t.Run("a matching vocabulary passes", func(t *testing.T) {
		ok := NewActionSet[FakeState]().
			Add(Action{Opcode: "set_project_status", Description: "Test fixture: set project status.", Category: ChangeScalarSet, Tier: TierReversible, Input: "status",
				Operands: []Operand{{Name: "status", Type: "enum", Required: true,
					EnumValues: []string{"not_started", "on_track", "at_risk", "blocked", "on_hold", "completed"}}}},
				FakeState{}.Reader("status"))

		ft := &fakeTB{}
		VerifyActionSetVocabulary(ft, ok, oracle)
		if ft.failed() {
			t.Errorf("a correct vocabulary was rejected: %s", ft.joined())
		}
	})

	t.Run("an oracle with no opinion is skipped", func(t *testing.T) {
		ft := &fakeTB{}
		VerifyActionSetVocabulary(ft, set, func(string, string) []string { return nil })
		if ft.failed() {
			t.Errorf("an oracle returning nil means 'no opinion' and must not fail: %s", ft.joined())
		}
	})
}

// TestVerifyActionDeclarations_NeedsNoReaders pins the other friction a migrating consumer hit: the
// reader-free half of the contract was unreachable.
//
// VerifyActionSet calls Validate(), which requires a state reader for every state-comparing category.
// But an agent whose backend already enforces the premise gate server-side (catalog's
// proposalWillChange is unbypassable) has NO reason to write agent-side readers — they would be dead
// production code. That consumer wrote ~60 lines of test-only readers purely to unlock the harness.
func TestVerifyActionDeclarations_NeedsNoReaders(t *testing.T) {
	// No readers anywhere — the shape of an agent that defers the premise check to its backend.
	set := NewActionSet[FakeState]().
		Add(Action{Opcode: "set_project_status", Description: "Test fixture: set project status.", Category: ChangeScalarSet, Tier: TierReversible, Input: "status",
			Operands: []Operand{{Name: "status", Type: "enum", Required: true,
				EnumValues: []string{"on_track"}}}}, nil).
		Add(Action{Opcode: "archive_project", Description: "Test fixture: archive project.", Category: ChangeTerminal, Tier: TierReversible,
			TerminalStates: []string{"archived"}}, nil)

	ft := &fakeTB{}
	VerifyActionDeclarations(ft, set)
	if ft.failed() {
		t.Errorf("declaration checks must not require readers — an agent whose backend owns the "+
			"premise gate has no reason to write them: %s", ft.joined())
	}

	// It must still CATCH a declaration mistake, or it is decoration.
	bad := NewActionSet[FakeState]().
		Add(Action{Opcode: "x", Description: "Test fixture: x.", Category: ChangeScalarSet, Tier: TierReversible, Input: "ghost",
			Operands: []Operand{{Name: "real", Type: "string"}}}, nil)

	ft2 := &fakeTB{}
	VerifyActionDeclarations(ft2, bad)
	if !ft2.failed() {
		t.Error("an Input naming no real operand must still be caught without a reader")
	}
}

// TestFakeState_Reader pins the stub: a state read with no DB, no network, no store.
func TestFakeState_Reader(t *testing.T) {
	st := FakeState{"task.due_date": "2026-08-21"}
	got, err := st.Reader("task.due_date")(context.Background(), &st, Action{})
	if err != nil || got != "2026-08-21" {
		t.Errorf("FakeState.Reader = (%q, %v), want (2026-08-21, nil)", got, err)
	}

	missing, err := st.Reader("absent")(context.Background(), &st, Action{})
	if err != nil || missing != "" {
		t.Errorf("an absent key must read as empty, got (%q, %v)", missing, err)
	}
}
