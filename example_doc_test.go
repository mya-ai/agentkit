package agentkit_test

// Compiled documentation. The reference's marked samples live here as runnable Examples, so a
// sample can never drift from the API: if a signature changes, `go test ././` fails.
//
// This is the "samples COMPILE or are marked illustrative" rule (CLAUDE.md, Docs are LLM-NATIVE).
// A doc block that cannot rot is worth more than a longer one that can — a coding agent copies a
// stale sample confidently.

import (
	"context"
	"fmt"

	"github.com/mya-ai/agentkit"
)

// docCaps is the capability set the samples below share: one reversible tool (may run live) and
// one irreversible tool (structurally cannot).
func docCaps() []agentkit.Capability {
	return []agentkit.Capability{{
		Name:        "set_due_date",
		Description: "Set a task's due date. Use when the user states a deadline.",
		Tier:        agentkit.TierReversible,
		Args: []agentkit.CapabilityArg{
			{Name: "due", Type: "date", Required: true, Description: "ISO-8601 date"},
		},
	}, {
		Name:        "send_email",
		Description: "Email the task owner. Only when explicitly asked.",
		Tier:        agentkit.TierIrreversible,
		Args: []agentkit.CapabilityArg{
			{Name: "to", Type: "string", Required: true, Description: "recipient"},
		},
	}}
}

// ExampleGateProgram is THE core pattern: verify the model's whole program, then split it by
// reversibility. An irreversible call is captured into Plan.Propose and is physically unable to
// reach Plan.Apply — that is the safety guarantee, and it holds however the model phrases itself.
func ExampleGateProgram() {
	program := []agentkit.ToolCall{
		{Name: "set_due_date", Args: map[string]any{"due": "2026-08-21"}},
		{Name: "send_email", Args: map[string]any{"to": "dana@example.com"}},
	}

	plan, err := agentkit.GateProgram(docCaps(), program, agentkit.RungAuto)
	if err != nil {
		// The model invented an opcode/operand/enum. The WHOLE program is rejected so a multi-op
		// write is never half-applied.
		fmt.Println("rejected:", err)
		return
	}

	for _, c := range plan.Apply {
		fmt.Println("APPLY  ", c.Name, c.ArgString("due"))
	}
	for _, c := range plan.Propose {
		fmt.Println("PROPOSE", c.Name, c.ArgString("to"))
	}
	// Output:
	// APPLY   set_due_date 2026-08-21
	// PROPOSE send_email dana@example.com
}

// ExampleVerifyCall shows the model inventing a capability that was never declared.
func ExampleVerifyCall() {
	err := agentkit.VerifyCall(docCaps(), agentkit.ToolCall{Name: "delete_everything"})
	fmt.Println(err != nil)
	// Output: true
}

// ExampleVerifyCall_missingRequiredArg shows a declared capability called without its required
// operand — also rejected, before any handler runs.
func ExampleVerifyCall_missingRequiredArg() {
	err := agentkit.VerifyCall(docCaps(), agentkit.ToolCall{
		Name: "set_due_date",
		Args: map[string]any{}, // "due" is Required
	})
	fmt.Println(err != nil)
	// Output: true
}

// ExampleRenderCapabilities generates the tool list for the prompt. Never hand-write this list:
// a hand-written one drifts from the specs silently, and if the prompt is runtime-overridable no
// Go test can catch the drift.
func ExampleRenderCapabilities() {
	rendered := agentkit.RenderCapabilities(docCaps())
	fmt.Println(len(rendered) > 0)
	// Output: true
}

// ExampleToolCall_ArgInt shows the accessor normalizing what models actually emit: JSON numbers
// arrive as float64, and a model may quote them. Both parse.
func ExampleToolCall_ArgInt() {
	fromNumber := agentkit.ToolCall{Args: map[string]any{"minutes": float64(30)}}
	fromString := agentkit.ToolCall{Args: map[string]any{"minutes": "30"}}

	a, okA := fromNumber.ArgInt("minutes")
	b, okB := fromString.ArgInt("minutes")
	fmt.Println(a, okA, b, okB)
	// Output: 30 true 30 true
}

// ExampleRulesVerifier shows a deterministic pre-check. The returned strings are handed to the
// executor on its next turn, so the model can FIX the problem rather than merely failing.
func ExampleRulesVerifier() {
	type verdict struct {
		Status string
		Reason string
	}

	v := agentkit.RulesVerifier(func(x verdict) (bool, []string) {
		if x.Status == "blocked" && x.Reason == "" {
			return false, []string{"status=blocked requires a reason"}
		}
		return true, nil
	})

	ok, feedback, err := v.Verify(context.Background(), verdict{Status: "blocked"})
	fmt.Println(ok, feedback[0], err)
	// Output: false status=blocked requires a reason <nil>
}

// ExampleMaxRungFor is the tier→autonomy mapping the gate enforces.
func ExampleMaxRungFor() {
	fmt.Println(agentkit.MaxRungFor(agentkit.TierReadOnly) == agentkit.RungAuto)
	fmt.Println(agentkit.MaxRungFor(agentkit.TierReversible) == agentkit.RungAuto)
	fmt.Println(agentkit.MaxRungFor(agentkit.TierExternalReversible) == agentkit.RungAsk)
	fmt.Println(agentkit.MaxRungFor(agentkit.TierIrreversible) == agentkit.RungAsk)
	// Output:
	// true
	// true
	// true
	// true
}

// ExampleActionSet is the PROPOSE side: what the agent may ask the user to bless.
//
// It mirrors ExampleToolSet deliberately — same Add(spec, behaviour) shape — because the two halves
// of an agent should not need two mental models. The difference is what the second argument IS:
// a ToolSet's handler PERFORMS the act; an ActionSet's reader only reports CURRENT STATE, and the
// kit works out whether the act would change anything.
//
// ⭐ You never write the comparison. You declare the action's CATEGORY (the shape of its effect) and
// the kit derives the rule — including the subtle case: an empty value means the user fills it on
// accept, so there is nothing to compare and the ask is always shown.
func ExampleActionSet() {
	// The state your gather already loaded. The kit never sees this type — only the strings your
	// readers return.
	type state struct {
		dueDate string
	}

	actions := agentkit.NewActionSet[state]().
		// "the due_date input, versus the task's current due date" — that is the whole declaration.
		Add(agentkit.Action{
			Opcode: "set_task_due_date", Category: agentkit.ChangeScalarSet, Input: "due_date",
			Description: "Set the task's due date.",
			Tier:        agentkit.TierReversible,
			Operands: []agentkit.Operand{
				{Name: "due_date", Type: "date", Required: true, Description: "ISO-8601 date"},
			},
		}, func(_ context.Context, s *state, _ agentkit.Action) (string, error) { return s.dueDate, nil }).
		// A create has no prior object to compare — so it always mints, and needs NO state logic.
		Add(agentkit.Action{
			Opcode: "create_goal", Category: agentkit.ChangeAlwaysMints,
			Tier:        agentkit.TierReversible, // ⭐ always state it — the zero value AUTO-RUNS
			Description: "Propose a new goal.",
			Operands: []agentkit.Operand{
				{Name: "title", Type: "string", Required: true, Description: "the objective"},
			},
		}, nil)

	// Catches a missing reader, an Input naming no operand, a duplicate opcode — at WIRING time.
	if err := actions.Validate(); err != nil {
		fmt.Println("wiring error:", err)
		return
	}

	// The model proposes three acts against a task already due 2026-08-21.
	current := &state{dueDate: "2026-08-21"}
	surviving, dropped := agentkit.SimulateTurns(actions, current, []agentkit.Action{
		{Opcode: "set_task_due_date", Args: map[string]string{"due_date": "2026-08-21"}}, // already true
		{Opcode: "create_goal", Args: map[string]string{"title": "Ship v2"}},             // always mints
		{Opcode: "invented_opcode"}, // never declared
	})

	fmt.Print(agentkit.Explain(surviving, dropped))
	// Output:
	// ASK    create_goal
	// DROP   set_task_due_date — no-op: applying set_task_due_date would not change state
	// DROP   invented_opcode — malformed: agentkit: action "invented_opcode" is not declared (the model invented an opcode); declared: set_task_due_date, create_goal
}

// ExampleToolSet is THE end-to-end loop — declare → render → gate → dispatch → handle both halves.
//
// Every other Example in this file demonstrates ONE function. This one exists because that was the
// gap: a reader could see GateProgram and RenderCapabilities in isolation and still not know how an
// agent is actually wired, because nothing showed what happens AFTER the gate. (Nothing could:
// dispatch did not exist, so the honest answer was "hand-write a switch".)
func ExampleToolSet() {
	// The per-run state handlers share. One call writes a draft another step composes later — which
	// is why handlers take *state rather than being independent funcs.
	type runState struct {
		applied []string
		draft   string
	}

	// 1. DECLARE — spec + handler together. The name exists in exactly ONE place.
	tools := agentkit.NewToolSet[runState]().
		Add(agentkit.Capability{
			Name:        "set_due_date",
			Description: "Set the item's due date. Use only when the evidence states a real deadline.",
			Tier:        agentkit.TierReversible, // may run live
			Args: []agentkit.CapabilityArg{
				{Name: "due", Type: "date", Required: true, Description: "ISO-8601 date"},
			},
		}, func(_ context.Context, s *runState, c agentkit.ToolCall) error {
			s.applied = append(s.applied, "set_due_date="+c.ArgString("due"))
			return nil
		}).
		Add(agentkit.Capability{
			Name:        "write",
			Description: "Draft the reply in full. Nobody has seen it yet, so this is reversible.",
			Tier:        agentkit.TierReversible,
			Args: []agentkit.CapabilityArg{
				{Name: "body", Type: "string", Required: true, Description: "the full text"},
			},
		}, func(_ context.Context, s *runState, c agentkit.ToolCall) error {
			s.draft = c.ArgString("body") // consumed after the loop
			return nil
		}).
		Add(agentkit.Capability{
			Name:        "send_reply",
			Description: "SEND the reply. Always needs the user's approval — you prepare, they send.",
			Tier:        agentkit.TierIrreversible, // can NEVER run live
			Args: []agentkit.CapabilityArg{
				{Name: "body", Type: "string", Description: "what you drafted"},
			},
		}, func(_ context.Context, s *runState, _ agentkit.ToolCall) error {
			s.applied = append(s.applied, "SENT") // unreachable: the tier withholds it
			return nil
		})

	// 2. VALIDATE at wiring time — catches a duplicate name, an empty name, a spec with no handler.
	if err := tools.Validate(); err != nil {
		fmt.Println("wiring error:", err)
		return
	}

	// 3. RENDER — the prompt's tool list is GENERATED from the same declaration, so it cannot drift.
	_ = "…your prompt…\n" + agentkit.RenderCapabilities(tools.Capabilities())

	// 4. The model emits a program. It emits ACTS — it never says "propose".
	program := []agentkit.ToolCall{
		{Name: "write", Args: map[string]any{"body": "Tuesday works for us."}},
		{Name: "set_due_date", Args: map[string]any{"due": "2026-08-21"}},
		{Name: "send_reply", Args: map[string]any{"body": "Tuesday works for us."}},
	}

	// 5. GATE — verifies the WHOLE program, then splits it by tier.
	plan, err := tools.GateProgram(program, agentkit.RungAuto)
	if err != nil {
		fmt.Println("the model invented an opcode/operand/enum:", err)
		return
	}

	// 6. DISPATCH the reversible half. Errors accumulate; the run continues.
	var state runState
	for _, e := range tools.ApplyPlan(context.Background(), &state, plan) {
		fmt.Println("write failed:", e)
	}

	// 7. Handle the withheld half YOURSELF — this is where you ask the human.
	for _, c := range plan.Propose {
		fmt.Println("NEEDS APPROVAL:", c.Name)
	}

	fmt.Println("applied:", state.applied)
	fmt.Println("draft:", state.draft)
	// Output:
	// NEEDS APPROVAL: send_reply
	// applied: [set_due_date=2026-08-21]
	// draft: Tuesday works for us.
}

// docContract is a stand-in for whatever op contract your backend already serves. The kit never
// learns what a "project" is — it consumes only this shape.
type docContract struct{}

func (docContract) BackendOps(context.Context) ([]agentkit.Action, error) {
	return []agentkit.Action{{
		Opcode: "set_task_due_date", Tier: agentkit.TierReversible,
		Description: "Set a task's due date.",
		Operands: []agentkit.Operand{
			{Name: "task_id", Type: "string", Required: true},
			{Name: "due_date", Type: "date", Required: true, Description: "RFC3339"},
		},
	}, {
		Opcode: "archive_project", Tier: agentkit.TierIrreversible,
		Description: "Archive a project.",
		Operands:    []agentkit.Operand{{Name: "project_id", Type: "string", Required: true}},
	}}, nil
}

// ExampleBind is the declaration you actually want to write: WHICH ops the agent uses, and how each
// one's premise works. Everything else — operands, types, enums, required flags, tier, description —
// arrives from the backend, so it cannot be fabricated or drift.
//
// ⭐ A real migration hand-copied all of that into 335 lines, and the coding agent writing it invented
// 5 enum values and dropped 4 while every test stayed green. A mirror of wire-available data is a
// cache with no invalidation.
func ExampleBind() {
	type state struct{ dueByTask map[string]string }

	set, err := agentkit.Bind(context.Background(), docContract{}, []agentkit.Use[state]{{
		Opcode:   "set_task_due_date",
		Category: agentkit.ChangeScalarSet, // the premise — the agent's judgment
		Input:    "due_date",
		// The act carries the target id, so the reader resolves the RIGHT object per call.
		Current: func(_ context.Context, s *state, a agentkit.Action) (string, error) {
			return s.dueByTask[a.Arg("task_id")], nil
		},
	}, {
		Opcode:   "archive_project",
		Category: agentkit.ChangeTerminal, TerminalStates: []string{"archived"},
		Current: func(context.Context, *state, agentkit.Action) (string, error) { return "active", nil },
		// ⭐ Required: the tier ALWAYS withholds this act, so without wording the gate would withhold
		// it and the user would never be asked — Bind rejects that shape.
		Ask: func(agentkit.Action) string { return "Archive this project?" },
	}})
	if err != nil {
		fmt.Println("bind failed:", err)
		return
	}

	a, _ := set.Lookup("set_task_due_date")
	fmt.Println("tier from backend:", a.Tier)
	fmt.Println("operands from backend:", len(a.Operands))

	// The premise still works, and now against the right task.
	st := &state{dueByTask: map[string]string{"t-1": "2026-08-21", "t-2": "2026-12-01"}}
	same, _ := set.WillChange(context.Background(), st, agentkit.Action{Opcode: "set_task_due_date",
		Args: map[string]string{"task_id": "t-1", "due_date": "2026-08-21"}})
	diff, _ := set.WillChange(context.Background(), st, agentkit.Action{Opcode: "set_task_due_date",
		Args: map[string]string{"task_id": "t-2", "due_date": "2026-08-21"}})
	fmt.Println("t-1 already has it, ask warranted:", same)
	fmt.Println("t-2 differs, ask warranted:", diff)

	// Output:
	// tier from backend: reversible
	// operands from backend: 2
	// t-1 already has it, ask warranted: false
	// t-2 differs, ask warranted: true
}
