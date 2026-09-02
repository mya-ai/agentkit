package agentkit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ACTIONSET — the tests ARE the formal contract.
//
// Each category below is a DERIVATION of one law, not a rule of thumb:
//
//	WillChange(op, s)  ≡  ¬fixpoint(op, s)  ≡  ¬( Add ⊆ s ∧ Del ∩ s = ∅ )
//
// So each test names the reduction it pins. If a category's generated rule stops matching its
// reduction, the failure is a proof error, not a preference.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// testState stands in for a consumer's gathered state. The kit never sees a type like this — it only
// ever receives the string a reader returns.
type testState struct {
	dueDate   string
	status    string
	health    string
	projectID string
	readErr   error
}

func dueDateReader(_ context.Context, s *testState, _ Action) (string, error) {
	return s.dueDate, s.readErr
}

// setDueDate is the canonical ScalarSet action, reused across the laws below.
func setDueDate() Action {
	return Action{
		Opcode: "set_task_due_date", Category: ChangeScalarSet, Input: "due_date",
		Tier:        TierReversible,
		Description: "Set the task's due date.",
		Operands:    []Operand{{Name: "due_date", Type: "date", Required: true}},
	}
}

// TestScalarSet_FixpointLaw is the core reduction: for Add = {X = v}, the state is a fixpoint exactly
// when s.X == v, so WillChange ≡ (s.X != v). Nothing about this is a heuristic.
func TestScalarSet_FixpointLaw(t *testing.T) {
	set := NewActionSet[testState]().Add(setDueDate(), dueDateReader)

	st := &testState{dueDate: "2026-08-21"}

	cases := []struct {
		name     string
		proposed string
		want     bool
		why      string
	}{
		{"effect already holds", "2026-08-21", false,
			"the state IS a fixpoint of the effect — this ask was never warranted"},
		{"effect does not hold", "2026-09-01", true,
			"the value differs, so applying it changes state"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := set.WillChange(context.Background(), st,
				Action{Opcode: "set_task_due_date", Args: map[string]string{"due_date": c.proposed}})
			if err != nil {
				t.Fatalf("WillChange errored: %v", err)
			}
			if got != c.want {
				t.Errorf("WillChange = %v, want %v — %s", got, c.want, c.why)
			}
		})
	}
}

// TestScalarSet_EmptyInputMints pins the consequence that is easiest to forget when hand-writing a
// comparator — and which a DERIVED rule cannot omit.
//
// An empty proposed value means the Add effect is UNDETERMINED (the user fills the slot on accept),
// so the current state cannot be a fixpoint of it → always mint. This is why "set the due date — you
// pick" is always shown while "set it to Aug 21" is skipped when it is already Aug 21.
func TestScalarSet_EmptyInputMints(t *testing.T) {
	set := NewActionSet[testState]().Add(setDueDate(), dueDateReader)

	// The state EQUALS nothing meaningful here; what matters is that the proposed value is undecided.
	for _, empty := range []string{"", "   "} {
		got, err := set.WillChange(context.Background(), &testState{dueDate: "2026-08-21"},
			Action{Opcode: "set_task_due_date", Args: map[string]string{"due_date": empty}})
		if err != nil {
			t.Fatalf("WillChange errored: %v", err)
		}
		if !got {
			t.Errorf("an EMPTY proposed value (%q) was treated as a no-op — but the user fills it on "+
				"accept, so there is no post-state to compare and the ask must always be shown", empty)
		}
	}
}

// TestWillChange_FailsOpen pins the asymmetry with VerifyAction: a state read that fails must KEEP
// the turn. An ask wrongly shown is recoverable; an ask wrongly dropped is silent.
func TestWillChange_FailsOpen(t *testing.T) {
	set := NewActionSet[testState]().Add(setDueDate(), dueDateReader)

	st := &testState{dueDate: "2026-08-21", readErr: errors.New("catalog unavailable")}
	got, err := set.WillChange(context.Background(), st,
		Action{Opcode: "set_task_due_date", Args: map[string]string{"due_date": "2026-08-21"}})

	if err != nil {
		t.Fatalf("a reader error must NOT surface as an error — it must fail open: %v", err)
	}
	if !got {
		t.Error("⛔ a failed state read DROPPED the turn — fail-open is the contract: an ask wrongly " +
			"dropped is silent, while one wrongly shown is recoverable")
	}
}

// TestAlwaysMints_NeedsNoStateLogic pins the categories whose post-state is not computable: a create
// has no prior object to compare, and feedback (a question) has no state effect at all. Add ⊆ s is
// undefined, so there is no fixpoint → always mint, and the user writes NO state logic.
func TestAlwaysMints_NeedsNoStateLogic(t *testing.T) {
	set := NewActionSet[testState]().
		Add(Action{Opcode: "create_goal", Description: "Test fixture: create goal.", Category: ChangeAlwaysMints, Tier: TierReversible,
			Operands: []Operand{{Name: "title", Type: "string", Required: true}}}, nil)

	if err := set.Validate(); err != nil {
		t.Fatalf("an AlwaysMints action with a nil reader must be VALID — there is nothing to read: %v", err)
	}

	got, err := set.WillChange(context.Background(), &testState{},
		Action{Opcode: "create_goal", Args: map[string]string{"title": "Ship v2"}})
	if err != nil {
		t.Fatalf("WillChange errored: %v", err)
	}
	if !got {
		t.Error("a create/feedback op must always mint — its post-state is not computable")
	}
}

// TestMembership_FixpointLaw: Add = {linked(a,b)}, so the state is a fixpoint exactly when the link
// already exists → WillChange ≡ ¬linked.
func TestMembership_FixpointLaw(t *testing.T) {
	set := NewActionSet[testState]().
		Add(Action{Opcode: "add_to_project", Description: "Test fixture: add to project.", Category: ChangeMembership, Tier: TierReversible, Input: "project_id",
			Operands: []Operand{{Name: "project_id", Type: "string", Required: true}}},
			func(_ context.Context, s *testState, _ Action) (string, error) { return s.projectID, nil })

	st := &testState{projectID: "proj-1"}

	if got, _ := set.WillChange(context.Background(), st,
		Action{Opcode: "add_to_project", Args: map[string]string{"project_id": "proj-1"}}); got {
		t.Error("already a member — the link exists, so the state is a fixpoint and nothing changes")
	}
	if got, _ := set.WillChange(context.Background(), st,
		Action{Opcode: "add_to_project", Args: map[string]string{"project_id": "proj-2"}}); !got {
		t.Error("not a member of proj-2 — adding it changes state")
	}
}

// TestTerminal_FixpointLaw: Add = {status = terminal}, so an object already in a terminal state is a
// fixpoint. This is why "archive this" on an archived project is not an ask.
func TestTerminal_FixpointLaw(t *testing.T) {
	set := NewActionSet[testState]().
		Add(Action{Opcode: "archive_project", Description: "Test fixture: archive project.", Category: ChangeTerminal,
			TerminalStates: []string{"archived", "completed"}, Tier: TierIrreversible},
			func(_ context.Context, s *testState, _ Action) (string, error) { return s.status, nil })

	if got, _ := set.WillChange(context.Background(), &testState{status: "archived"},
		Action{Opcode: "archive_project", Description: "Test fixture: archive project."}); got {
		t.Error("already archived — the terminal state holds, so archiving again changes nothing")
	}
	if got, _ := set.WillChange(context.Background(), &testState{status: "active"},
		Action{Opcode: "archive_project", Description: "Test fixture: archive project."}); !got {
		t.Error("an active project — archiving it changes state")
	}
}

// TestMultiField_IsNotScalarSet pins the asymmetry the formalism EXISTS to catch
// for a SINGLE-field set-op an empty value means "user-input → mint"; for
// a MULTI-field op an empty field means "not being set" and is IGNORED — the op still changes state
// via its other provided fields, or is a genuine no-op if none of them change.
//
// ⭐ Same symbol, OPPOSITE meaning, because the Add set differs. A design that forced update_goal into
// ScalarSet would be provably wrong, so this test is the guard on that.
func TestMultiField_IsNotScalarSet(t *testing.T) {
	set := NewActionSet[testState]().
		AddMultiField(Action{Opcode: "update_goal", Description: "Test fixture: update goal.", Category: ChangeMultiField, Tier: TierReversible,
			Operands: []Operand{{Name: "status", Type: "string"}, {Name: "health", Type: "string"}}},
			func(_ context.Context, s *testState, _ Action, field string) (string, error) {
				if field == "health" {
					return s.health, nil
				}
				return s.status, nil
			})

	st := &testState{status: "on_track", health: "on_track"}

	// Every PROVIDED field already holds → a genuine no-op.
	if got, _ := set.WillChange(context.Background(), st,
		Action{Opcode: "update_goal", Args: map[string]string{"status": "on_track"}}); got {
		t.Error("every provided field already holds — this is a genuine no-op")
	}

	// ⭐ An ABSENT field is NOT "undecided user input" here — it is simply not being set. With no
	// provided field changing, the op is still a no-op. Under ScalarSet's rule this would MINT.
	if got, _ := set.WillChange(context.Background(), st,
		Action{Opcode: "update_goal", Args: map[string]string{"status": "on_track", "health": ""}}); got {
		t.Error("⛔ an ABSENT multi-field operand was treated as ScalarSet's 'undecided user input'. " +
			"For a multi-field op an empty field means NOT BEING SET and contributes nothing")
	}

	// A provided field that differs → changes.
	if got, _ := set.WillChange(context.Background(), st,
		Action{Opcode: "update_goal", Args: map[string]string{"status": "at_risk"}}); !got {
		t.Error("a provided field differs — the op changes state")
	}
}

// TestMultiField_ComparesEachFieldAgainstItsOwnValue is the regression guard for a SILENT DROP found
// by an adversarial review — the worst class of bug this file can carry, because it fails CLOSED on a
// check whose entire contract is fail-OPEN.
//
// The original implementation called ONE reader for N operands and compared every field against that
// single string. So an op changing `health` at_risk→on_track on a goal whose `status` was already
// "on_track" reported WillChange=FALSE, and the proposal was silently discarded:
//
//	goal is status="on_track" health="at_risk"; the agent proposes health="on_track"
//	→ health genuinely changes at_risk → on_track, but WillChange = FALSE
//
// ⚠️ The old test could never have caught it: it used `status` as BOTH the tested operand AND the
// reader's return, so single-reader and per-field semantics were indistinguishable. This test pins
// the distinguishing case — the fields must hold DIFFERENT values.
//
// The hand-written gate it claims parity with was always correct: it compares each column
// against its OWN current value, one call per column.
func TestMultiField_ComparesEachFieldAgainstItsOwnValue(t *testing.T) {
	set := NewActionSet[testState]().
		AddMultiField(Action{Opcode: "update_goal", Description: "Test fixture: update goal.", Category: ChangeMultiField, Tier: TierReversible,
			Operands: []Operand{{Name: "status", Type: "string"}, {Name: "health", Type: "string"}}},
			func(_ context.Context, s *testState, _ Action, field string) (string, error) {
				if field == "health" {
					return s.health, nil
				}
				return s.status, nil
			})

	// ⭐ The two fields hold DIFFERENT values — this is what makes the case discriminating.
	st := &testState{status: "on_track", health: "at_risk"}

	got, err := set.WillChange(context.Background(), st,
		Action{Opcode: "update_goal", Args: map[string]string{"health": "on_track"}})
	if err != nil {
		t.Fatalf("WillChange errored: %v", err)
	}
	if !got {
		t.Error("⛔ SILENT DROP: health genuinely changes at_risk → on_track, but WillChange reported " +
			"'no change'. The reader must be asked for EACH FIELD's own value — comparing every operand " +
			"against one shared string discards real changes, and does so in the fail-CLOSED direction " +
			"on a check contracted to fail OPEN.")
	}

	// The mirror case: a field equal to ITS OWN current value is genuinely a no-op.
	got, _ = set.WillChange(context.Background(), st,
		Action{Opcode: "update_goal", Args: map[string]string{"health": "at_risk"}})
	if got {
		t.Error("health already equals at_risk — that field is a fixpoint, so this is a real no-op")
	}
}

// TestMultiField_RejectsASharedReader pins the guard that makes the bug above unrepeatable: a
// multi-field action registered with Add (one shared reader) is rejected at WIRING time, in both
// directions, so nobody can re-introduce the mispairing by copying the wrong Add call.
func TestMultiField_RejectsASharedReader(t *testing.T) {
	shared := func(_ context.Context, s *testState, _ Action) (string, error) { return s.status, nil }
	perField := func(_ context.Context, s *testState, _ Action, _ string) (string, error) { return s.status, nil }
	multi := Action{Opcode: "update_goal", Description: "Test fixture: update goal.", Category: ChangeMultiField, Tier: TierReversible,
		Operands: []Operand{{Name: "status", Type: "string"}, {Name: "health", Type: "string"}}}

	t.Run("multi-field registered with Add is rejected", func(t *testing.T) {
		err := NewActionSet[testState]().Add(multi, shared).Validate()
		if err == nil {
			t.Fatal("a multi-field action with ONE shared reader passed Validate — that is the silent-drop bug")
		}
		if !strings.Contains(err.Error(), "AddMultiField") {
			t.Errorf("the error must name the fix, got: %v", err)
		}
	})

	t.Run("a scalar registered with AddMultiField is rejected", func(t *testing.T) {
		if err := NewActionSet[testState]().AddMultiField(setDueDate(), perField).Validate(); err == nil {
			t.Error("a ScalarSet registered with AddMultiField passed Validate — only multi_field " +
				"compares per operand")
		}
	})
}

// TestScalarSet_ComparesByTypeNotByString is the regression guard for a re-mint-forever bug an
// adversarial review demonstrated: the generated comparison was a raw `proposed != current`, so any
// value with more than one spelling never reached a fixpoint.
//
//	"2026-08-21" vs "2026-08-21T00:00:00Z"  → same DATE,   compared unequal → re-minted every run
//	"5000"       vs "5000.00"               → same NUMBER,  compared unequal → re-minted every run
//
// ⭐ This is the kit's bug, not the consumer's: the kit declares the Type, hard-codes the comparison,
// and offers no normalization seam — so a reader returning what the DB holds cannot avoid it.
func TestScalarSet_ComparesByTypeNotByString(t *testing.T) {
	set := NewActionSet[testState]().
		Add(setDueDate(), dueDateReader).
		Add(Action{Opcode: "set_budget", Description: "Test fixture: set budget.", Category: ChangeScalarSet, Tier: TierReversible, Input: "amount",
			Operands: []Operand{{Name: "amount", Type: "number", Required: true}}},
			func(_ context.Context, s *testState, _ Action) (string, error) { return s.status, nil })

	t.Run("the same date spelled two ways is a fixpoint", func(t *testing.T) {
		st := &testState{dueDate: "2026-08-21T00:00:00Z"}
		got, _ := set.WillChange(context.Background(), st,
			Action{Opcode: "set_task_due_date", Args: map[string]string{"due_date": "2026-08-21"}})
		if got {
			t.Error("⛔ RE-MINT FOREVER: '2026-08-21' and '2026-08-21T00:00:00Z' are the SAME DATE, so " +
				"the state IS a fixpoint — comparing them as strings re-proposes on every single run")
		}
	})

	t.Run("a genuinely different date still changes", func(t *testing.T) {
		st := &testState{dueDate: "2026-08-21T00:00:00Z"}
		got, _ := set.WillChange(context.Background(), st,
			Action{Opcode: "set_task_due_date", Args: map[string]string{"due_date": "2026-09-01"}})
		if !got {
			t.Error("a different date must still report a change — normalization must not flatten real ones")
		}
	})

	t.Run("the same number spelled two ways is a fixpoint", func(t *testing.T) {
		st := &testState{status: "5000"}
		got, _ := set.WillChange(context.Background(), st,
			Action{Opcode: "set_budget", Args: map[string]string{"amount": "5000.00"}})
		if got {
			t.Error("'5000' and '5000.00' are the same NUMBER — the state is a fixpoint")
		}
	})
}

// TestVerifyAction_ValidatesDeclaredTypes closes the gap an adversarial review drove an ask through:
// only `enum` was validated, so `amount="not-a-number"` and `date="tomorrow-ish"` reached a rendered
// proposal — a malformed ask in front of a human, exactly what fail-CLOSED exists to prevent.
//
// ⚠️ An UNRECOGNIZED type must still pass: Type is the consumer's vocabulary and the kit must not
// close a set it does not own.
func TestVerifyAction_ValidatesDeclaredTypes(t *testing.T) {
	set := NewActionSet[testState]().
		Add(setDueDate(), dueDateReader).
		Add(Action{Opcode: "set_budget", Description: "Test fixture: set budget.", Category: ChangeScalarSet, Tier: TierReversible, Input: "amount",
			Operands: []Operand{{Name: "amount", Type: "number", Required: true}}},
			func(_ context.Context, s *testState, _ Action) (string, error) { return s.status, nil }).
		Add(Action{Opcode: "set_note", Description: "Test fixture: set note.", Category: ChangeScalarSet, Tier: TierReversible, Input: "body",
			Operands: []Operand{{Name: "body", Type: "markdown", Required: true}}}, // kit doesn't own it
			func(_ context.Context, s *testState, _ Action) (string, error) { return s.status, nil })

	bad := []struct{ name, opcode, field, val string }{
		{"prose date", "set_task_due_date", "due_date", "tomorrow-ish"},
		{"impossible date", "set_task_due_date", "due_date", "31/02/2026"},
		{"non-numeric amount", "set_budget", "amount", "not-a-number"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			err := set.VerifyAction(Action{Opcode: c.opcode, Args: map[string]string{c.field: c.val}})
			if err == nil {
				t.Errorf("%q passed verification for a %s operand — it would reach a human as a "+
					"malformed ask", c.val, c.field)
			}
		})
	}

	t.Run("valid values pass", func(t *testing.T) {
		for _, ok := range []struct{ opcode, field, val string }{
			{"set_task_due_date", "due_date", "2026-08-21"},
			{"set_task_due_date", "due_date", "2026-08-21T00:00:00Z"},
			{"set_budget", "amount", "5000.00"},
			{"set_budget", "amount", "-500"}, // negative is a DOMAIN rule, not a type rule
		} {
			if err := set.VerifyAction(Action{Opcode: ok.opcode,
				Args: map[string]string{ok.field: ok.val}}); err != nil {
				t.Errorf("%q is a valid %s but was rejected: %v", ok.val, ok.field, err)
			}
		}
	})

	t.Run("an unrecognized type is accepted", func(t *testing.T) {
		if err := set.VerifyAction(Action{Opcode: "set_note", Description: "Test fixture: set note.",
			Args: map[string]string{"body": "## anything at all"}}); err != nil {
			t.Errorf("the kit must not validate a type it does not own (%q): %v", "markdown", err)
		}
	})
}

// TestFillsSlot_AnsweredQuestionIsNotReAsked is the category that makes "deduplication of duplicate
// DIALOGUE" expressible at all. Without it, an agent re-asks a question the user already answered.
//
// ⭐ THE RULE COLLISION IT RESOLVES. Two rules, each correct for its own kind, contradict:
//
//	PROPOSAL ("set the due date to Aug 21"):   empty value ⇒ the user picks ⇒ ALWAYS MINT
//	QUESTION ("what due date should I use?"):  the value EXISTS ⇒ already answered ⇒ NEVER ASK
//
// A question asking for a value carries an EMPTY Input by construction — that is what makes it a
// question. So under ChangeScalarSet the empty-value rule fires first and returns true
// unconditionally, and the fixpoint check below it is UNREACHABLE for exactly the shape most in need
// of de-duplication. Demonstrated before this category existed:
//
//	premise resolved (due="2026-08-21"), question re-asked? true
//
// The difference is WHAT THE PREMISE IS. For a proposal the premise is its own effect (does the field
// already equal MY value?). For a question the premise is whether the slot it fills is still EMPTY —
// the proposed value is irrelevant, because the user supplies it.
func TestFillsSlot_AnsweredQuestionIsNotReAsked(t *testing.T) {
	set := NewActionSet[testState]().
		Add(Action{Opcode: "ask_due_date", Category: ChangeFillsSlot, Tier: TierReversible, Input: "due_date",
			Description: "Ask the user which due date to use.",
			Operands:    []Operand{{Name: "due_date", Type: "date"}}},
			dueDateReader)

	if err := set.Validate(); err != nil {
		t.Fatalf("a well-formed FillsSlot action was rejected: %v", err)
	}

	t.Run("the slot is EMPTY — ask it", func(t *testing.T) {
		got, err := set.WillChange(context.Background(), &testState{dueDate: ""},
			Action{Opcode: "ask_due_date", Args: map[string]string{"due_date": ""}})
		if err != nil {
			t.Fatalf("WillChange errored: %v", err)
		}
		if !got {
			t.Error("the slot is unfilled, so the question is still open and MUST be asked")
		}
	})

	t.Run("the slot is FILLED — the user already answered", func(t *testing.T) {
		got, err := set.WillChange(context.Background(), &testState{dueDate: "2026-08-21"},
			Action{Opcode: "ask_due_date", Args: map[string]string{"due_date": ""}})
		if err != nil {
			t.Fatalf("WillChange errored: %v", err)
		}
		if got {
			t.Error("⛔ the agent will RE-ASK a question the user already answered. The premise of a " +
				"question is whether the slot it fills is still empty — not what value the ask carries.")
		}
	})

	t.Run("the proposed value is IRRELEVANT — the user supplies it", func(t *testing.T) {
		// Even if the agent drafts a suggestion, an already-filled slot means the question is closed.
		got, _ := set.WillChange(context.Background(), &testState{dueDate: "2026-08-21"},
			Action{Opcode: "ask_due_date", Args: map[string]string{"due_date": "2026-09-01"}})
		if got {
			t.Error("a FillsSlot premise must ignore the carried value — only 'is the slot empty?' " +
				"decides whether the question is still open")
		}
	})

	t.Run("a reader error still fails OPEN", func(t *testing.T) {
		st := &testState{dueDate: "2026-08-21", readErr: errors.New("catalog unavailable")}
		got, _ := set.WillChange(context.Background(), st,
			Action{Opcode: "ask_due_date", Args: map[string]string{"due_date": ""}})
		if !got {
			t.Error("a failed state read must KEEP the question — dropping one is silent")
		}
	})
}

// TestStateReader_ReceivesTheAct pins the signature fix a consumer's design review found BLOCKING.
//
// A reader used to be func(ctx, *S) — it could only consult ambient state. But a real run touches
// SEVERAL objects: the PM plans across many tasks, and "set THIS task's due date" needs THIS task's
// current value. With no way to know which, an agent had to thread a mutable "current target" field
// through its state and set it before every check.
//
// ⭐ Forget to set it, and the comparison reads the WRONG object — which fails CLOSED: the ask looks
// like a no-op and is silently dropped. That is the exact failure mode fail-open exists to prevent,
// re-introduced through the back door by the reader's own signature.
//
// The act carries the target id in its args, so the reader resolves per call and the mutable field
// disappears.
func TestStateReader_ReceivesTheAct(t *testing.T) {
	// A state holding MANY tasks — the shape that broke the old signature.
	type multi struct{ dueByTask map[string]string }

	set := NewActionSet[multi]().
		Add(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "due_date",
			Operands: []Operand{
				{Name: "task_id", Type: "string", Required: true},
				{Name: "due_date", Type: "date", Required: true},
			}},
			func(_ context.Context, s *multi, a Action) (string, error) {
				return s.dueByTask[a.Arg("task_id")], nil // ⭐ resolves the RIGHT object, per call
			})

	st := &multi{dueByTask: map[string]string{"t-1": "2026-08-21", "t-2": "2026-12-01"}}

	// t-1 is already due 2026-08-21 → a no-op.
	if got, _ := set.WillChange(context.Background(), st, Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.",
		Args: map[string]string{"task_id": "t-1", "due_date": "2026-08-21"}}); got {
		t.Error("t-1 already has this date — the ask is a genuine no-op")
	}

	// ⭐ The SAME date against t-2 is a REAL change. Under the old signature a reader with a stale
	// target would have compared t-1's value here and dropped this ask silently.
	if got, _ := set.WillChange(context.Background(), st, Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.",
		Args: map[string]string{"task_id": "t-2", "due_date": "2026-08-21"}}); !got {
		t.Error("⛔ t-2 is due 2026-12-01, so moving it to 2026-08-21 CHANGES state — reading the wrong " +
			"object drops a legitimate ask, and does it in the fail-CLOSED direction")
	}
}

// TestVerifyAction_FailsClosed is the OTHER check, with the OPPOSITE default: a malformed action is
// dropped. A malformed ask reaching a human is the class this guards.
func TestVerifyAction_FailsClosed(t *testing.T) {
	set := NewActionSet[testState]().Add(setDueDate(), dueDateReader)

	cases := []struct {
		name string
		act  Action
		want string
	}{
		{"invented opcode", Action{Opcode: "delete_everything", Description: "Test fixture: delete everything."}, "delete_everything"},
		{"invented operand", Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.",
			Args: map[string]string{"due_date": "2026-08-21", "colour": "red"}}, "colour"},
		{"missing required operand", Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date."}, "due_date"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := set.VerifyAction(c.act)
			if err == nil {
				t.Fatalf("a malformed action PASSED verification — it would reach a human malformed")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error must name %q so it is correctable, got: %v", c.want, err)
			}
		})
	}

	if err := set.VerifyAction(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.",
		Args: map[string]string{"due_date": "2026-08-21"}}); err != nil {
		t.Errorf("a well-formed action was rejected: %v", err)
	}
}

// TestVerifyAction_WaivesUserFilledOperands: a required operand the USER fills on accept is legitimately
// absent when the action is frozen. Mirrors a backend op kernel's user-slot waiver — without it, every "you pick
// the date" proposal would fail verification.
func TestVerifyAction_WaivesUserFilledOperands(t *testing.T) {
	set := NewActionSet[testState]().Add(setDueDate(), dueDateReader)

	err := set.VerifyAction(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.", UserFills: []string{"due_date"}})
	if err != nil {
		t.Errorf("a required operand declared as user-filled must be WAIVED (the user supplies it on "+
			"accept), got: %v", err)
	}

	if err := set.VerifyAction(Action{Opcode: "set_task_due_date", Description: "Test fixture: set task due date.", UserFills: []string{"nonexistent"}}); err == nil {
		t.Error("a user-fill naming an operand the action does not have must be rejected")
	}
}

// TestActionSet_Validate catches declaration mistakes at WIRING time — the point of a declarative
// design is that a wrong declaration cannot reach a run.
func TestActionSet_Validate(t *testing.T) {
	reader := func(_ context.Context, _ *testState, _ Action) (string, error) { return "", nil }

	t.Run("category needing state has no reader", func(t *testing.T) {
		set := NewActionSet[testState]().Add(setDueDate(), nil)
		if err := set.Validate(); err == nil {
			t.Error("a ScalarSet with NO state reader cannot compare anything — Validate must reject it")
		}
	})

	t.Run("scalar set with no Input", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(Action{Opcode: "x", Description: "Test fixture: x.", Category: ChangeScalarSet, Tier: TierReversible,
				Operands: []Operand{{Name: "v", Type: "string"}}}, reader)
		if err := set.Validate(); err == nil {
			t.Error("a ScalarSet must name WHICH operand it compares — without Input there is no rule")
		}
	})

	t.Run("Input names an undeclared operand", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(Action{Opcode: "x", Description: "Test fixture: x.", Category: ChangeScalarSet, Tier: TierReversible, Input: "ghost",
				Operands: []Operand{{Name: "v", Type: "string"}}}, reader)
		if err := set.Validate(); err == nil {
			t.Error("Input must name a REAL operand — otherwise the comparison reads an absent arg")
		}
	})

	t.Run("terminal with no states", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(Action{Opcode: "x", Description: "Test fixture: x.", Category: ChangeTerminal, Tier: TierReversible}, reader)
		if err := set.Validate(); err == nil {
			t.Error("a Terminal action must declare WHICH states are terminal")
		}
	})

	t.Run("duplicate opcode", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(Action{Opcode: "dup", Description: "Test fixture: dup.", Category: ChangeAlwaysMints, Tier: TierReversible}, nil).
			Add(Action{Opcode: "dup", Description: "Test fixture: dup.", Category: ChangeAlwaysMints, Tier: TierReversible}, nil)
		if err := set.Validate(); err == nil {
			t.Error("a duplicate opcode makes one declaration unreachable")
		}
	})

	t.Run("empty opcode", func(t *testing.T) {
		set := NewActionSet[testState]().Add(Action{Category: ChangeAlwaysMints}, nil)
		if err := set.Validate(); err == nil {
			t.Error("an action with no opcode can never be emitted")
		}
	})

	t.Run("a valid set passes", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(setDueDate(), dueDateReader).
			Add(Action{Opcode: "create_goal", Description: "Test fixture: create goal.", Category: ChangeAlwaysMints, Tier: TierReversible}, nil)
		if err := set.Validate(); err != nil {
			t.Errorf("a well-formed set was rejected: %v", err)
		}
	})
}

// TestActionSet_UnknownActionInWillChange: WillChange must not silently mint for an action nobody
// declared — that would hide the invented-opcode case behind fail-open. Verification catches it, and
// WillChange reports it rather than pretending.
func TestActionSet_UnknownActionInWillChange(t *testing.T) {
	set := NewActionSet[testState]().Add(setDueDate(), dueDateReader)

	_, err := set.WillChange(context.Background(), &testState{}, Action{Opcode: "never_declared", Description: "Test fixture: never declared."})
	if err == nil {
		t.Error("WillChange on an UNDECLARED action must ERROR — silently minting would hide the " +
			"invented-opcode case that VerifyAction exists to catch")
	}
}

// TestActionSet_FeedsThePrompt closes the loop, exactly as ToolSet does: the SAME declaration the
// state logic hangs off is what the model is told about. There is no second list to drift.
func TestActionSet_FeedsThePrompt(t *testing.T) {
	set := NewActionSet[testState]().Add(setDueDate(), dueDateReader)

	rendered := RenderActions(set.Actions())
	for _, want := range []string{"set_task_due_date", "due_date"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the declared action did not reach the prompt (missing %q):\n%s", want, rendered)
		}
	}
}

// TestValuesDiffer_TypeSpellings is the regression guard for a LIVE silent bug found by building
// five agents against the kit: the two operand types documented DIFFERENT vocabularies for the same
// concept, and only one spelling got numeric comparison.
//
//	CapabilityArg.Type: "string" | "enum" | "int"    | "bool" | "date"   (capability.go)
//	Operand.Type:       "string" | "enum" | "number"          | "date"   (action.go)
//
// A developer who wrote the spelling documented on the SIBLING type got:
//
//	Type="number" current=40.0 proposed=40 -> WillChange=false   ← correct
//	Type="int"    current=40.0 proposed=40 -> WillChange=true    ← re-proposes forever
//
// ⭐ That is precisely the failure WillChange exists to prevent, caused by the framework's own
// documentation, with every other check green. Nothing validated Type anywhere.
//
// The fix accepts the synonyms both types document. ⚠️ It deliberately does NOT close the set:
// Type is the consumer's vocabulary (an unrecognized type falls back to an exact compare), so the
// guarantee is "the spellings the kit itself documents all work", not "only these exist".
func TestValuesDiffer_TypeSpellings(t *testing.T) {
	numeric := []string{"number", "int", "integer", "float"}
	for _, typ := range numeric {
		t.Run(typ+" normalizes", func(t *testing.T) {
			if valuesDiffer(typ, "40", "40.0") {
				t.Errorf("Type=%q: 40 and 40.0 are the SAME NUMBER — comparing them as strings "+
					"re-proposes an act that changes nothing, on every single run", typ)
			}
			if !valuesDiffer(typ, "41", "40.0") {
				t.Errorf("Type=%q: 41 and 40.0 differ — normalization must not flatten a real change", typ)
			}
		})
	}

	t.Run("date spellings", func(t *testing.T) {
		for _, typ := range []string{"date", "datetime", "timestamp"} {
			if valuesDiffer(typ, "2026-08-21", "2026-08-21T00:00:00Z") {
				t.Errorf("Type=%q: the same instant spelled two ways must compare equal", typ)
			}
		}
	})

	t.Run("an unrecognized type falls back to an exact compare", func(t *testing.T) {
		// Type is the consumer's vocabulary — the kit must not close a set it does not own.
		if !valuesDiffer("markdown", "40", "40.0") {
			t.Error("an unknown type must compare exactly, not guess at numeric semantics")
		}
	})
}

// TestActionSet_NilIsASupportedState closes a panic waiting on a path a production consumer
// DELIBERATELY produces.
//
// ⛔ Bind fails CLOSED, which is right for an agent whose whole job is proposing. But the PM's
// proposals are only PART of a review — its planning and status work are unaffected by a contract
// blip — so a consumer's bound-action builder returns a NIL set and logs, rather than killing
// the run. Nil then means "skip agent-side validation; the engine still verifies at accept."
//
// Every ActionSet method dereferenced the receiver immediately:
//
//	*** Lookup on a nil ActionSet PANICS: runtime error: invalid memory address ***
//
// So the consumer hand-wrote a guard funnel (lookupAct, proposals_apply.go) to convert nil into an
// empty answer for its two call sites — and one future direct call would panic in production.
//
// ⭐ Nil is now a SUPPORTED state meaning "the empty set": no actions declared, so nothing is
// verifiable and nothing is warranted-checkable. That is exactly the degraded semantics the consumer
// wanted, and it removes the need for anyone to write the guard again.
func TestActionSet_NilIsASupportedState(t *testing.T) {
	var set *ActionSet[testState]

	t.Run("Lookup", func(t *testing.T) {
		if _, ok := set.Lookup("anything"); ok {
			t.Error("a nil set declares nothing, so nothing can be found in it")
		}
	})
	t.Run("Actions", func(t *testing.T) {
		if len(set.Actions()) != 0 {
			t.Error("a nil set has no actions")
		}
	})
	t.Run("Validate", func(t *testing.T) {
		if err := set.Validate(); err != nil {
			t.Errorf("a nil set has no declarations to be wrong: %v", err)
		}
	})
	t.Run("VerifyAction ERRORS rather than passing", func(t *testing.T) {
		// ⚠️ Fails CLOSED even when degraded: an undeclared opcode is still not verifiable.
		if err := set.VerifyAction(Action{Opcode: "x", Description: "Test fixture: x."}); err == nil {
			t.Error("a nil set cannot VERIFY anything — it must error, not silently approve")
		}
	})
	t.Run("WillChange errors rather than deciding", func(t *testing.T) {
		if _, err := set.WillChange(context.Background(), &testState{}, Action{Opcode: "x", Description: "Test fixture: x."}); err == nil {
			t.Error("a nil set cannot evaluate a premise — it must say so")
		}
	})
}

// TestActionSet_ValidateIsAsStrictAsBind closes the gap that unified four independent reviews: the
// SAME declaration mistake was caught on one path and silent on another.
//
// ⛔ THE ASYMMETRY. Bind rejects an understated tier and a withheld-act-with-no-Ask at wiring time
// (bind.go). ToolSet.Validate rejects a forgotten Tier with the kit's best error message. ActionSet —
// the path the reference offers as an EQUAL alternative for anyone without a contract source — checked
// NEITHER. Measured before this fix:
//
//	ActionSet.Validate() = <nil>          ← an irreversible act with no Ask, AND a forgotten tier
//	VerifyActionSet failures = false      ← the shipped harness agreed
//
// ⭐ THE LESSON, which is the session's recurring one: a guard is only worth what its WEAKEST
// reachable path enforces. The kit diagnosed the zero-Tier trap, wrote the guard, and left two of
// three entry points without it — exactly as it earlier put the guard in Validate while GateProgram,
// the function consumers actually call, skipped it.
func TestActionSet_ValidateIsAsStrictAsBind(t *testing.T) {
	t.Run("a forgotten Tier is rejected", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(Action{Opcode: "delete_all", Category: ChangeAlwaysMints, Description: "destroys"}, nil) // Tier DELIBERATELY omitted
		err := set.Validate()
		if err == nil {
			t.Fatal("⛔ a forgotten Tier passed — the zero value is read-only, which AUTO-RUNS")
		}
		if !strings.Contains(err.Error(), "Tier") {
			t.Errorf("the error must name the missing field, got: %v", err)
		}
	})

	t.Run("a withheld act with no Ask is rejected", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(Action{Opcode: "wire_funds", Category: ChangeAlwaysMints, Tier: TierIrreversible,
				Description: "sends money"}, nil)
		err := set.Validate()
		if err == nil {
			t.Fatal("⛔ an act the tier ALWAYS withholds, with no Ask, passed — the gate withholds it " +
				"and the user is never asked, so it vanishes silently. Bind rejects this; ActionSet must too")
		}
		if !strings.Contains(err.Error(), "Ask") {
			t.Errorf("the error must name the missing field, got: %v", err)
		}
	})

	t.Run("a well-formed withheld act passes", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(Action{Opcode: "wire_funds", Category: ChangeAlwaysMints, Tier: TierIrreversible,
				Description: "sends money", Ask: func(Action) string { return "Send the wire?" }}, nil)
		if err := set.Validate(); err != nil {
			t.Errorf("a correctly declared withheld act must pass: %v", err)
		}
	})

	t.Run("an explicit read-only act passes", func(t *testing.T) {
		set := NewActionSet[testState]().
			Add(Action{Opcode: "search", Category: ChangeAlwaysMints, Tier: TierReadOnlyExplicit,
				Description: "looks up"}, nil)
		if err := set.Validate(); err != nil {
			t.Errorf("an explicitly read-only act must pass: %v", err)
		}
	})
}

// TestVerifyAction_EnforcesTypeSynonyms is the regression for the kit's headline fail-CLOSED
// guarantee holding for only HALF the spellings it documents.
//
// ⚠️ checkOperandType switched on the RAW Operand.Type, so "int"/"datetime"/"integer"/… fell through
// to the accept-unknown default and VerifyAction passed amount="not-a-number". normalizeTypeName was
// added precisely to fold these, and this one switch never called it.
//
// ⭐ Every synonym is exercised, not just the two that were reported — the defect is the MISSING
// NORMALIZE, so any spelling the folder knows must be enforced or the fix is partial.
func TestVerifyAction_EnforcesTypeSynonyms(t *testing.T) {
	numberSpellings := []string{"number", "int", "integer", "float", "float64", "decimal"}
	dateSpellings := []string{"date", "datetime", "timestamp", "time"}

	build := func(typeName string) *ActionSet[struct{ v string }] {
		return NewActionSet[struct{ v string }]().Add(Action{
			Opcode: "op", Description: "Test fixture: op.", Category: ChangeScalarSet, Input: "field", Tier: TierReversible,
			Operands: []Operand{{Name: "field", Type: typeName, Required: true}},
		}, func(_ context.Context, s *struct{ v string }, _ Action) (string, error) { return s.v, nil })
	}

	for _, spelling := range numberSpellings {
		set := build(spelling)
		if err := set.VerifyAction(Action{Opcode: "op", Args: map[string]string{"field": "not-a-number"}}); err == nil {
			t.Errorf("⛔ Type:%q accepted a non-numeric value — the fail-CLOSED guarantee does not hold "+
				"for this spelling, so a model emitting garbage reaches the backend", spelling)
		}
		if err := set.VerifyAction(Action{Opcode: "op", Args: map[string]string{"field": "42"}}); err != nil {
			t.Errorf("Type:%q rejected a valid number: %v", spelling, err)
		}
	}

	for _, spelling := range dateSpellings {
		set := build(spelling)
		if err := set.VerifyAction(Action{Opcode: "op", Args: map[string]string{"field": "tomorrow-ish"}}); err == nil {
			t.Errorf("⛔ Type:%q accepted an unparseable date — a malformed date reaching the backend is "+
				"the exact class the PM's lossy operand mirror could not catch", spelling)
		}
		if err := set.VerifyAction(Action{Opcode: "op", Args: map[string]string{"field": "2026-09-30"}}); err != nil {
			t.Errorf("Type:%q rejected a valid date: %v", spelling, err)
		}
	}

	// The accept-unknown contract must SURVIVE the fix: Type is the consumer's vocabulary and the kit
	// must not close a set it does not own.
	custom := build("employee_id")
	if err := custom.VerifyAction(Action{Opcode: "op", Args: map[string]string{"field": "anything"}}); err != nil {
		t.Errorf("an unrecognized Type must still be ACCEPTED (the kit does not own that vocabulary): %v", err)
	}
}

// TestValidate_RejectsAForgottenCategory is the regression for the kit's worst default.
//
// ⚠️ ChangeCategory's zero value USED TO BE ChangeAlwaysMints, so omitting the field silently declared
// "no computable post-state ⇒ always mint" — and the ask RE-MINTED ON EVERY REVIEW, which is exactly
// the defect ActionSet exists to close. Bind, Validate, VerifyActionSet and VerifyActionSetAgainst
// all passed it.
//
// ⭐ Same argument the kit already makes for Tier (tier.go:51): "this always asks" must be a
// STATEMENT, never an OMISSION. Unlike Tier — pinned position-for-position against a backend kernel — this
// enum is the kit's own, so the zero value itself could be made invalid.
func TestValidate_RejectsAForgottenCategory(t *testing.T) {
	read := func(_ context.Context, s *struct{ v string }, _ Action) (string, error) { return s.v, nil }

	forgotten := NewActionSet[struct{ v string }]().Add(Action{
		Opcode: "set_due", Description: "Test fixture: set due.", Input: "due", Tier: TierReversible, // Category DELIBERATELY omitted
		Operands: []Operand{{Name: "due", Type: "date", Required: true}},
	}, read)

	err := forgotten.Validate()
	if err == nil {
		t.Fatal("⛔ a forgotten Category must be REJECTED — as the zero value it classified the act as " +
			"always-mint, so the ask re-proposed itself forever and every guard passed")
	}
	if !strings.Contains(err.Error(), "Category") {
		t.Errorf("the error must name the missing field, got: %v", err)
	}

	// And a STATED category still passes — the guard must not reject correct declarations.
	stated := NewActionSet[struct{ v string }]().Add(Action{
		Opcode: "set_due", Description: "Test fixture: set due.", Category: ChangeScalarSet, Input: "due", Tier: TierReversible,
		Operands: []Operand{{Name: "due", Type: "date", Required: true}},
	}, read)
	if err := stated.Validate(); err != nil {
		t.Errorf("a stated Category must pass: %v", err)
	}

	// ⭐ AND THE REASON IT MATTERS: with the category stated, an ask whose effect already holds is
	// correctly suppressed. That is the behaviour the zero value was silently destroying.
	act := Action{Opcode: "set_due", Args: map[string]string{"due": "2026-09-01"}}
	changes, err := stated.WillChange(context.Background(), &struct{ v string }{v: "2026-09-01"}, act)
	if err != nil {
		t.Fatalf("WillChange: %v", err)
	}
	if changes {
		t.Error("⛔ an ask whose effect ALREADY HOLDS must not mint — this is what a forgotten Category " +
			"turned off")
	}
}
