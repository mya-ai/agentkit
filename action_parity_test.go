package agentkit

import (
	"context"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// PARITY — the gate on the whole design.
//
// The premise question ("does this act actually change anything?") is normally answered by a
// hand-written gate: one arm per opcode, each arm a small piece of hand-rolled comparison logic.
// ActionSet replaces that switch with a DECLARATION — a Category per action — and DERIVES WillChange
// from it. This test is the proof that the trade is sound: for every category in the vocabulary, and
// for every edge case that a hand-written arm would have had to get right by hand, the generated
// WillChange returns what the hand-written arm returns.
//
// If a category's derivation cannot reproduce an arm, then either the formalism is wrong or that arm
// is a latent bug — and either way it must be resolved before a consumer trusts the derivation.
//
// ⚠️ This test deliberately imports no backend (agentkit imports no infra). It encodes each op's
// EXPECTED behaviour as a case, which makes it a specification test rather than a differential one —
// the trade the boundary forces.
//
// Every case names the behaviour it mirrors, so a reader can check the reasoning rather than the
// transcription.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// deskState is the synthetic support-desk state a hand-written gate would read via storage calls:
// the CURRENT value of every field the op-set can write.
type deskState struct {
	ticketTitle    string
	ticketDesc     string
	ticketDueDate  string
	ticketStatus   string
	ticketAssignee string
	ticketQueue    string

	queueStatus    string
	queueName      string
	queueDesc      string
	queueDeadline  string
	queueStart     string
	queueObjective string

	objectiveDesc      string
	objectiveTimeframe string
	objectiveStatus    string
	objectiveHealth    string
}

// deskActions declares a full support-desk op-set — 24 ops spanning every category in the
// vocabulary. It is the stand-in for a real consumer's action set: if the parity test passes, a
// declaration of this shape can replace both a hand-maintained operand mirror and a hand-written
// premise switch.
func deskActions() *ActionSet[deskState] {
	s := NewActionSet[deskState]()

	// ── create / feedback: no computable post-state → always mint ─────────────────────────────
	s.Add(Action{Opcode: "create_ticket", Description: "Test fixture: create ticket.", Category: ChangeAlwaysMints, Tier: TierReversible,
		Operands: []Operand{{Name: "title", Type: "string", Required: true}}}, nil)
	s.Add(Action{Opcode: "create_objective", Description: "Test fixture: create objective.", Category: ChangeAlwaysMints, Tier: TierReversible,
		Operands: []Operand{{Name: "title", Type: "string", Required: true}}}, nil)
	s.Add(Action{Opcode: "ask_user", Description: "Test fixture: agent question.", Category: ChangeAlwaysMints, Tier: TierReversible,
		Operands: []Operand{{Name: "answer", Type: "string"}}}, nil)

	// ── merge ops: subset-compare not modelled in v1 → always mint ────────────────────────────
	s.Add(Action{Opcode: "set_queue_metadata", Description: "Test fixture: set queue metadata.", Category: ChangeMerge, Tier: TierReversible,
		Operands: []Operand{{Name: "metadata", Type: "string", Required: true}}}, nil)
	s.Add(Action{Opcode: "set_objective_metadata", Description: "Test fixture: set objective metadata.", Category: ChangeMerge, Tier: TierReversible,
		Operands: []Operand{{Name: "metadata", Type: "string", Required: true}}}, nil)

	// ── membership / link: changes iff not already a member ──────────────────────────────────
	s.Add(Action{Opcode: "add_to_queue", Description: "Test fixture: add to queue.", Category: ChangeMembership, Tier: TierReversible, Input: "queue_id",
		Operands: []Operand{{Name: "queue_id", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.ticketQueue, nil })
	s.Add(Action{Opcode: "link_queue_to_objective", Description: "Test fixture: link queue to objective.", Category: ChangeMembership, Tier: TierReversible, Input: "objective_id",
		Operands: []Operand{{Name: "objective_id", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.queueObjective, nil })

	// ── scalar-set: ticket ───────────────────────────────────────────────────────────────────
	s.Add(Action{Opcode: "set_ticket_title", Description: "Test fixture: set ticket title.", Category: ChangeScalarSet, Tier: TierReversible, Input: "title",
		Operands: []Operand{{Name: "title", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.ticketTitle, nil })
	s.Add(Action{Opcode: "set_ticket_description", Description: "Test fixture: set ticket description.", Category: ChangeScalarSet, Tier: TierReversible, Input: "description",
		Operands: []Operand{{Name: "description", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.ticketDesc, nil })
	s.Add(Action{Opcode: "set_ticket_due_date", Description: "Test fixture: set ticket due date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "due_date",
		Operands: []Operand{{Name: "due_date", Type: "date", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.ticketDueDate, nil })
	s.Add(Action{Opcode: "set_ticket_status", Description: "Test fixture: set ticket status.", Category: ChangeScalarSet, Tier: TierReversible, Input: "status",
		Operands: []Operand{{Name: "status", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.ticketStatus, nil })

	// ⚠️ set_ticket_assignee is the one op whose real-world arm is RESOLVE-THEN-COMPARE: only a bare
	// assignee_id is a clean compare; an assignee_name may CREATE a directory row, so it always mints.
	// A plain ScalarSet on assignee_id reproduces the clean half; the create half falls out of the
	// empty-value rule rather than a create-detection rule. See TestParity_Assignee.
	s.Add(Action{Opcode: "set_ticket_assignee", Description: "Test fixture: set ticket assignee.", Category: ChangeScalarSet, Tier: TierReversible, Input: "assignee_id",
		Operands: []Operand{
			{Name: "assignee_id", Type: "string"},
			{Name: "assignee_link_user_id", Type: "string"},
			{Name: "assignee_name", Type: "string"},
			{Name: "assignee_email", Type: "string"},
		}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.ticketAssignee, nil })

	// ── scalar-set: queue ────────────────────────────────────────────────────────────────────
	s.Add(Action{Opcode: "set_queue_status", Description: "Test fixture: set queue status.", Category: ChangeScalarSet, Tier: TierReversible, Input: "status",
		Operands: []Operand{{Name: "status", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.queueStatus, nil })
	s.Add(Action{Opcode: "set_queue_name", Description: "Test fixture: set queue name.", Category: ChangeScalarSet, Tier: TierReversible, Input: "name",
		Operands: []Operand{{Name: "name", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.queueName, nil })
	s.Add(Action{Opcode: "set_queue_description", Description: "Test fixture: set queue description.", Category: ChangeScalarSet, Tier: TierReversible, Input: "description",
		Operands: []Operand{{Name: "description", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.queueDesc, nil })
	s.Add(Action{Opcode: "set_queue_deadline", Description: "Test fixture: set queue deadline.", Category: ChangeScalarSet, Tier: TierReversible, Input: "deadline",
		Operands: []Operand{{Name: "deadline", Type: "date", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.queueDeadline, nil })
	s.Add(Action{Opcode: "set_queue_start_date", Description: "Test fixture: set queue start date.", Category: ChangeScalarSet, Tier: TierReversible, Input: "start_date",
		Operands: []Operand{{Name: "start_date", Type: "date", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.queueStart, nil })

	// ── scalar-set: objective ────────────────────────────────────────────────────────────────
	s.Add(Action{Opcode: "set_objective_description", Description: "Test fixture: set objective description.", Category: ChangeScalarSet, Tier: TierReversible, Input: "description",
		Operands: []Operand{{Name: "description", Type: "string", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.objectiveDesc, nil })
	s.Add(Action{Opcode: "set_objective_timeframe", Description: "Test fixture: set objective timeframe.", Category: ChangeScalarSet, Tier: TierReversible, Input: "timeframe_target",
		Operands: []Operand{{Name: "timeframe_target", Type: "date", Required: true}}},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.objectiveTimeframe, nil })

	// ── multi-field ──────────────────────────────────────────────────────────────────────────
	// ⭐ AddMultiField, and the reader takes the OPERAND NAME — mirroring a hand-written multi-field
	// arm, which compares once PER COLUMN. A single shared reader would compare every field to one
	// value and silently drop a real change.
	s.AddMultiField(Action{Opcode: "update_objective", Description: "Test fixture: update objective.", Category: ChangeMultiField, Tier: TierReversible,
		Operands: []Operand{{Name: "status", Type: "string"}, {Name: "health", Type: "string"}}},
		func(_ context.Context, st *deskState, _ Action, field string) (string, error) {
			if field == "health" {
				return st.objectiveHealth, nil
			}
			return st.objectiveStatus, nil
		})

	// ── terminal / lifecycle ─────────────────────────────────────────────────────────────────
	s.Add(Action{Opcode: "close_ticket", Description: "Test fixture: close ticket.", Category: ChangeTerminal, Ask: func(Action) string { return "Mark this ticket closed?" },
		TerminalStates: []string{"CLOSED", "ARCHIVED"}, Tier: TierReversible},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.ticketStatus, nil })
	s.Add(Action{Opcode: "archive_ticket", Description: "Test fixture: archive ticket.", Category: ChangeTerminal, Ask: func(Action) string { return "Clear this ticket off the board?" },
		TerminalStates: []string{"ARCHIVED"}, Tier: TierIrreversible},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.ticketStatus, nil })
	// ⚠️ A TERMINAL VOCABULARY IS PART OF THE BEHAVIOUR, NOT DECORATION. An adversarial review of an
	// earlier version of this test found several ops declaring an INCOMPLETE terminal set — each
	// omission re-minting an ask the server rejects as a no-op, which is the exact duplicate class the
	// gate exists to kill. The lesson: copying status LITERALS without the comparison SEMANTICS (which
	// states count as over, and how they are compared) reintroduces the bug the declaration removed.
	// The two sets below are deliberately different sizes, so both arities are exercised.
	s.Add(Action{Opcode: "archive_queue", Description: "Test fixture: archive queue.", Category: ChangeTerminal, Ask: func(Action) string { return "Archive this queue?" },
		TerminalStates: []string{"archived", "closed"}, Tier: TierIrreversible}, // queueIsOver: 2 states
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.queueStatus, nil })
	s.Add(Action{Opcode: "archive_objective", Description: "Test fixture: archive objective.", Category: ChangeTerminal, Ask: func(Action) string { return "Archive this objective?" },
		TerminalStates: []string{"completed", "cancelled", "dismissed", "archived"}, // objectiveIsOver: 4 states
		Tier:           TierIrreversible},
		func(_ context.Context, st *deskState, _ Action) (string, error) { return st.objectiveStatus, nil })

	return s
}

// TestParity_TheOpSetDeclaresCleanly is the first half of the gate: every op in a realistic op-set
// can be EXPRESSED in the category vocabulary. If an op needed a category that does not exist, the
// vocabulary would be incomplete and this fails at Validate.
func TestParity_TheOpSetDeclaresCleanly(t *testing.T) {
	set := deskActions()

	if err := set.Validate(); err != nil {
		t.Fatalf("the op-set could not be declared in the category vocabulary — the vocabulary is "+
			"incomplete or a declaration is wrong:\n%v", err)
	}
	VerifyActionSet(t, set)

	if got := len(set.Actions()); got < 24 {
		t.Errorf("declared %d actions, expected at least the 24 ops the op-set covers", got)
	}
}

// TestParity_GeneratedRulesMatchAHandWrittenGate is the gate itself: for each op, the generated
// WillChange must return what a hand-written gate would return for the same state.
//
// Each case names the behaviour it mirrors. A failure means the derivation and the hand-written
// behaviour disagree — STOP and resolve which one is right (one of them is a bug).
func TestParity_GeneratedRulesMatchAHandWrittenGate(t *testing.T) {
	set := deskActions()

	cases := []struct {
		name  string
		arm   string // the hand-written gate behaviour being mirrored
		state deskState
		act   Action
		want  bool
	}{
		// create / feedback — "no computable post-state → always mint"
		{"create_ticket always mints", "no computable post-state", deskState{},
			Action{Opcode: "create_ticket", Args: map[string]string{"title": "x"}}, true},
		{"create_objective always mints", "no computable post-state", deskState{},
			Action{Opcode: "create_objective", Args: map[string]string{"title": "x"}}, true},
		{"ask_user always mints", "no computable post-state", deskState{},
			Action{Opcode: "ask_user", Description: "Test fixture: agent question."}, true},

		// merge — "post-state not modelled → conservatively always-changing"
		{"set_queue_metadata always mints", "merge write, post-state not modelled", deskState{},
			Action{Opcode: "set_queue_metadata", Args: map[string]string{"metadata": "{}"}}, true},

		// membership — "changes iff not yet a member"
		{"add_to_queue: already a member", "membership: already linked",
			deskState{ticketQueue: "queue-1"},
			Action{Opcode: "add_to_queue", Args: map[string]string{"queue_id": "queue-1"}}, false},
		{"add_to_queue: not a member", "membership: not yet linked",
			deskState{ticketQueue: ""},
			Action{Opcode: "add_to_queue", Args: map[string]string{"queue_id": "queue-1"}}, true},
		{"link_queue_to_objective: already linked", "membership: already linked",
			deskState{queueObjective: "obj-1"},
			Action{Opcode: "link_queue_to_objective", Args: map[string]string{"objective_id": "obj-1"}}, false},

		// scalar-set: the compare
		{"set_ticket_title: unchanged", "scalar-set: proposed equals current", deskState{ticketTitle: "Widget import fails"},
			Action{Opcode: "set_ticket_title", Args: map[string]string{"title": "Widget import fails"}}, false},
		{"set_ticket_title: changed", "scalar-set: proposed differs from current", deskState{ticketTitle: "Widget import fails"},
			Action{Opcode: "set_ticket_title", Args: map[string]string{"title": "Widget import times out"}}, true},
		{"set_ticket_due_date: unchanged", "scalar-set: proposed equals current", deskState{ticketDueDate: "2026-08-21"},
			Action{Opcode: "set_ticket_due_date", Args: map[string]string{"due_date": "2026-08-21"}}, false},
		{"set_ticket_status: changed", "scalar-set: proposed differs from current", deskState{ticketStatus: "open"},
			Action{Opcode: "set_ticket_status", Args: map[string]string{"status": "blocked"}}, true},
		{"set_queue_status: unchanged", "scalar-set: proposed equals current", deskState{queueStatus: "active"},
			Action{Opcode: "set_queue_status", Args: map[string]string{"status": "active"}}, false},
		{"set_queue_deadline: changed", "scalar-set: proposed differs from current", deskState{queueDeadline: "2026-09-30"},
			Action{Opcode: "set_queue_deadline", Args: map[string]string{"deadline": "2026-10-31"}}, true},
		{"set_objective_timeframe: unchanged", "scalar-set: proposed equals current", deskState{objectiveTimeframe: "2026-12-31"},
			Action{Opcode: "set_objective_timeframe", Args: map[string]string{"timeframe_target": "2026-12-31"}}, false},

		// ⭐ the EMPTY-VALUE rule — "an empty proposed value is USER-INPUT the agent has not decided
		// → ALWAYS MINT". This is the consequence the derivation gets for free.
		{"scalar-set with EMPTY value always mints", "empty value is user input", deskState{ticketDueDate: "2026-08-21"},
			Action{Opcode: "set_ticket_due_date", Args: map[string]string{"due_date": ""}}, true},
		{"scalar-set with WHITESPACE value always mints", "empty value is user input", deskState{ticketTitle: "Widget import fails"},
			Action{Opcode: "set_ticket_title", Args: map[string]string{"title": "   "}}, true},

		// ⭐ multi-field. NOTE the asymmetry: an ABSENT field means "not being set" and contributes
		// nothing, which is the OPPOSITE of the scalar-set rule above.
		{"update_objective: provided field unchanged → no-op", "multi-field: provided field equals current", deskState{objectiveStatus: "active"},
			Action{Opcode: "update_objective", Args: map[string]string{"status": "active"}}, false},
		{"update_objective: provided field changed", "multi-field: provided field differs from current", deskState{objectiveStatus: "active"},
			Action{Opcode: "update_objective", Args: map[string]string{"status": "completed"}}, true},
		{"update_objective: ABSENT field is ignored, not 'undecided'", "multi-field: absent field contributes nothing",
			deskState{objectiveStatus: "active"},
			Action{Opcode: "update_objective", Args: map[string]string{"status": "active", "health": ""}}, false},

		// terminal
		{"archive_queue: already archived", "terminal state already reached", deskState{queueStatus: "archived"},
			Action{Opcode: "archive_queue", Description: "Test fixture: archive queue."}, false},
		{"archive_queue: active", "terminal state not yet reached", deskState{queueStatus: "active"},
			Action{Opcode: "archive_queue", Description: "Test fixture: archive queue."}, true},
		{"close_ticket: already closed", "terminal state already reached", deskState{ticketStatus: "CLOSED"},
			Action{Opcode: "close_ticket", Description: "Test fixture: close ticket."}, false},
		{"close_ticket: still open", "terminal state not yet reached", deskState{ticketStatus: "OPEN"},
			Action{Opcode: "close_ticket", Description: "Test fixture: close ticket."}, true},
		{"archive_ticket: already archived", "terminal state already reached", deskState{ticketStatus: "ARCHIVED"},
			Action{Opcode: "archive_ticket", Description: "Test fixture: archive ticket."}, false},

		// ⭐ The cases an adversarial review added after finding transcription errors in an earlier
		// version of this test. Each was minting an ask the server rejects as a no-op — the exact
		// duplicate class this gate exists to kill, re-introduced by copying the status LITERALS
		// without the comparison SEMANTICS. A terminal set with more than one state, and a compare
		// that is case- and whitespace-insensitive, are both load-bearing.
		{"archive_queue: already 'closed' (queueIsOver has 2 states)", "terminal set has 2 states",
			deskState{queueStatus: "closed"}, Action{Opcode: "archive_queue", Description: "Test fixture: archive queue."}, false},
		{"archive_objective: already cancelled (objectiveIsOver has 4 states)", "terminal set has 4 states",
			deskState{objectiveStatus: "cancelled"}, Action{Opcode: "archive_objective", Description: "Test fixture: archive objective."}, false},
		{"archive_objective: already dismissed", "terminal set has 4 states",
			deskState{objectiveStatus: "dismissed"}, Action{Opcode: "archive_objective", Description: "Test fixture: archive objective."}, false},
		// The terminal compare is EqualFold+TrimSpace: the same status arrives upper-case from a
		// lifecycle column and lower-case from a workflow-state type. An exact compare silently never
		// fires, so the ask re-mints forever for an object that is already terminal.
		{"close_ticket: lower-case 'closed' still terminal", "terminal compare is case-insensitive",
			deskState{ticketStatus: "closed"}, Action{Opcode: "close_ticket", Description: "Test fixture: close ticket."}, false},
		{"archive_ticket: padded ' ARCHIVED ' still terminal", "terminal compare trims whitespace",
			deskState{ticketStatus: " ARCHIVED "}, Action{Opcode: "archive_ticket", Description: "Test fixture: archive ticket."}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := set.WillChange(context.Background(), &c.state, c.act)
			if err != nil {
				t.Fatalf("WillChange errored: %v", err)
			}
			if got != c.want {
				t.Errorf("⛔ PARITY BREAK: generated WillChange = %v, but %q returns %v.\n"+
					"The derivation and the hand-written arm DISAGREE — resolve which is correct "+
					"(one of them is a bug).", got, c.arm, c.want)
			}
		})
	}
}

// TestParity_Assignee documents the ONE op whose hand-written arm is not a pure category.
//
// Assigning a ticket is resolve-then-compare: only a bare assignee_id is a clean compare; an
// assignee_name may CREATE-or-resolve a directory row, so it always mints. The kit's ScalarSet
// reproduces the clean half exactly; the create half needs the consumer to route it, since "these
// operands mean create" is domain meaning, not mechanism.
//
// ⚠️ This is a KNOWN, BOUNDED gap — recorded rather than hidden, and it does not block adoption: the
// conservative behaviour (mint) is what a consumer gets by declaring the create-shaped variant as its
// own AlwaysMints action.
func TestParity_Assignee(t *testing.T) {
	set := deskActions()
	st := deskState{ticketAssignee: "assignee-1"}

	// The clean half: a bare assignee_id compares exactly like the hand-written arm.
	got, _ := set.WillChange(context.Background(), &st,
		Action{Opcode: "set_ticket_assignee", Args: map[string]string{"assignee_id": "assignee-1"}})
	if got {
		t.Error("a bare assignee_id equal to the current assignee must be a no-op")
	}

	got, _ = set.WillChange(context.Background(), &st,
		Action{Opcode: "set_ticket_assignee", Args: map[string]string{"assignee_id": "assignee-2"}})
	if !got {
		t.Error("a different assignee_id changes state")
	}

	// The create half: assignee_name with no assignee_id. The hand-written arm always mints because
	// the row may not exist yet. ScalarSet sees an EMPTY Input and also mints — the same answer,
	// reached by the empty-value derivation rather than by a create-detection rule.
	got, _ = set.WillChange(context.Background(), &st,
		Action{Opcode: "set_ticket_assignee", Args: map[string]string{"assignee_name": "Assignee One"}})
	if !got {
		t.Error("an assignee_name (resolve-or-create) must always mint — the row may " +
			"not exist yet, so there is no post-state to compare")
	}
}
