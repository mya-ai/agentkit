package agentkit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ToolSet — the tests define the contract dispatch must satisfy.
//
// The shape is taken from a REAL consumer — an ops agent's dispatch switch: handlers are NOT
// independent. They share mutable state (a draft written by one call, a note by another, an error
// list), some carry a business brake that SKIPS without failing, and an error must never abort the
// calls that follow. A dispatcher that ignores any of those cannot replace the switch it exists to
// delete.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// runState is a stand-in for a consumer's per-run state — the thing a switch closes over today.
type runState struct {
	applied []string
	draft   string
	brakeOn bool
}

func reversible(name string, args ...CapabilityArg) Capability {
	return Capability{Name: name, Description: "test capability " + name, Tier: TierReversible, Args: args}
}

// TestToolSet_DispatchesToTheRegisteredHandler is the core of A1: one declaration carries the spec
// AND the handler, so the name exists in exactly one place.
func TestToolSet_DispatchesToTheRegisteredHandler(t *testing.T) {
	var st runState

	ts := NewToolSet[runState]().
		Add(reversible("set_title", CapabilityArg{Name: "title", Type: "string", Required: true}),
			func(_ context.Context, s *runState, c ToolCall) error {
				s.applied = append(s.applied, "set_title="+c.ArgString("title"))
				return nil
			})

	plan, err := ts.GateProgram([]ToolCall{{Name: "set_title", Args: map[string]any{"title": "Ship it"}}}, RungAuto)
	if err != nil {
		t.Fatalf("gate rejected a valid call: %v", err)
	}
	if errs := ts.ApplyPlan(context.Background(), &st, plan); len(errs) != 0 {
		t.Fatalf("ApplyPlan returned errors: %v", errs)
	}
	if len(st.applied) != 1 || st.applied[0] != "set_title=Ship it" {
		t.Fatalf("handler did not run with the model's args, got %v", st.applied)
	}
}

// TestToolSet_UnregisteredNameIsAnError is the bug this type exists to make impossible.
//
// Today a capability declared but not wired into the switch matches nothing: no error, no log, the
// call VANISHES and the run reports success. Dispatch must make that loud.
func TestToolSet_UnregisteredNameIsAnError(t *testing.T) {
	var st runState

	// Declared via a raw Plan (as if the spec existed but no handler was registered).
	ts := NewToolSet[runState]()
	plan := Plan{Apply: []ToolCall{{Name: "set_title"}}}

	errs := ts.ApplyPlan(context.Background(), &st, plan)
	if len(errs) == 0 {
		t.Fatal("an unregistered capability was SILENTLY SKIPPED — that is the silent-drop bug: the model " +
			"emitted a call, nothing ran, and the run reported success")
	}
	if !strings.Contains(errs[0].Error(), "set_title") {
		t.Errorf("the error must name the capability, got %v", errs[0])
	}
}

// TestToolSet_HandlersShareState proves the seam serves the real consumer: one call writes a value a
// LATER step composes. A dispatcher of independent func(ctx, call) error could not express this.
func TestToolSet_HandlersShareState(t *testing.T) {
	var st runState

	ts := NewToolSet[runState]().
		Add(reversible("write", CapabilityArg{Name: "body", Type: "string"}),
			func(_ context.Context, s *runState, c ToolCall) error {
				s.draft = c.ArgString("body") // consumed AFTER the loop by the consumer
				return nil
			}).
		Add(reversible("tag"), func(_ context.Context, s *runState, _ ToolCall) error {
			s.applied = append(s.applied, "tag")
			return nil
		})

	plan, err := ts.GateProgram([]ToolCall{
		{Name: "write", Args: map[string]any{"body": "the drafted reply"}},
		{Name: "tag"},
	}, RungAuto)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	ts.ApplyPlan(context.Background(), &st, plan)

	if st.draft != "the drafted reply" {
		t.Errorf("shared state lost: draft = %q", st.draft)
	}
	if len(st.applied) != 1 {
		t.Errorf("the second handler did not run: %v", st.applied)
	}
}

// TestToolSet_ErrorsAccumulateAndTheRunContinues pins the real consumer's semantics: a failed write
// is collected, and the remaining calls STILL RUN. Aborting would throw away work that succeeded.
func TestToolSet_ErrorsAccumulateAndTheRunContinues(t *testing.T) {
	var st runState
	boom := errors.New("backend unavailable")

	ts := NewToolSet[runState]().
		Add(reversible("first"), func(_ context.Context, _ *runState, _ ToolCall) error { return boom }).
		Add(reversible("second"), func(_ context.Context, s *runState, _ ToolCall) error {
			s.applied = append(s.applied, "second ran")
			return nil
		})

	plan, err := ts.GateProgram([]ToolCall{{Name: "first"}, {Name: "second"}}, RungAuto)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	errs := ts.ApplyPlan(context.Background(), &st, plan)

	if len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Fatalf("the failure must be COLLECTED, got %v", errs)
	}
	if len(st.applied) != 1 {
		t.Error("the run ABORTED after the first error — a later call that would have succeeded was lost")
	}
}

// TestToolSet_HandlerMaySkipWithoutFailing pins the business brake (a real consumer's: "the user's
// own date is authoritative"). A deliberate skip is a normal outcome — nil, not an error.
func TestToolSet_HandlerMaySkipWithoutFailing(t *testing.T) {
	st := runState{brakeOn: true}

	ts := NewToolSet[runState]().
		Add(reversible("set_due_date", CapabilityArg{Name: "due", Type: "date"}),
			func(_ context.Context, s *runState, _ ToolCall) error {
				if s.brakeOn {
					return nil // the user set this themselves — skip, do not fail
				}
				s.applied = append(s.applied, "set_due_date")
				return nil
			})

	plan, _ := ts.GateProgram([]ToolCall{{Name: "set_due_date", Args: map[string]any{"due": "2026-08-21"}}}, RungAuto)
	if errs := ts.ApplyPlan(context.Background(), &st, plan); len(errs) != 0 {
		t.Fatalf("a deliberate skip was reported as an error: %v", errs)
	}
	if len(st.applied) != 0 {
		t.Error("the brake did not hold")
	}
}

// TestToolSet_NeverRunsProposeTier is the safety invariant, at the dispatch layer.
//
// GateProgram already keeps an irreversible call out of Apply. ApplyPlan must ALSO never execute
// Plan.Propose — otherwise a consumer passing the whole plan would defeat the gate.
func TestToolSet_NeverRunsProposeTier(t *testing.T) {
	var st runState

	ts := NewToolSet[runState]().
		Add(Capability{Name: "send_email", Description: "sends", Tier: TierIrreversible},
			func(_ context.Context, s *runState, _ ToolCall) error {
				s.applied = append(s.applied, "SENT") // must never happen
				return nil
			})

	plan, err := ts.GateProgram([]ToolCall{{Name: "send_email"}}, RungAuto)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(plan.Propose) != 1 {
		t.Fatalf("an irreversible call must be withheld, got Apply=%d Propose=%d", len(plan.Apply), len(plan.Propose))
	}
	ts.ApplyPlan(context.Background(), &st, plan)
	if len(st.applied) != 0 {
		t.Fatal("⛔ ApplyPlan EXECUTED a withheld call — the tier gate is defeated at the dispatch layer")
	}
}

// TestToolSet_RefusesAHandBuiltProposePlan is the regression guard for a bypass an adversarial review
// demonstrated: the tier guarantee held only for Plans that CAME FROM GateProgram.
//
// Plan is an exported struct with exported slices, so the natural "retry what the gate held back"
// mistake — ApplyPlan(ctx, s, Plan{Apply: gated.Propose}) — executed an irreversible call live:
//
//	gated: Apply=[] Propose=[{transfer_funds}]
//	after hand-built Plan: ran=[TRANSFERRED]
//
// A safety guarantee conditional on a value's provenance is not structural. ApplyPlan now re-checks
// the tier itself, so no Plan of any origin can carry a withheld call into execution.
func TestToolSet_RefusesAHandBuiltProposePlan(t *testing.T) {
	var st runState

	ts := NewToolSet[runState]().
		Add(Capability{Name: "transfer_funds", Description: "moves money", Tier: TierIrreversible},
			func(_ context.Context, s *runState, _ ToolCall) error {
				s.applied = append(s.applied, "TRANSFERRED")
				return nil
			})

	gated, err := ts.GateProgram([]ToolCall{{Name: "transfer_funds"}}, RungAuto)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(gated.Propose) != 1 {
		t.Fatalf("the gate must withhold an irreversible call, got Apply=%d Propose=%d",
			len(gated.Apply), len(gated.Propose))
	}

	// The mistake: feed the withheld half back in as if it were approved.
	errs := ts.ApplyPlan(context.Background(), &st, Plan{Apply: gated.Propose})

	if len(st.applied) != 0 {
		t.Fatalf("⛔ a hand-built Plan EXECUTED an irreversible call: %v — the tier gate must not depend "+
			"on the Plan having come from GateProgram", st.applied)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "REFUSED") {
		t.Errorf("the refusal must be LOUD and explain why, got: %v", errs)
	}
}

// TestToolSet_ForgottenTierIsRejected is the regression guard for the worst silent failure a cold
// review found: Tier's zero value is TierReadOnly, which AUTO-RUNS. So a capability declared without
// a Tier — the single easiest field to omit — silently became "safe to run live", and a destructive
// act executed with no error:
//
//	Validate did NOT catch the missing Tier
//	gate err=<nil>  Apply=[{delete_mailbox map[]}]  Propose=[]
//	*** A forgotten Tier made a DESTRUCTIVE act AUTO-RUN: [DELETED] ***
//
// ⚠️ It CANNOT be fixed by renumbering the enum: a consumer-side cross-check test pins these int
// values against the backend's own reversibility enum position-for-position. So read-only is OPTED INTO
// explicitly (TierReadOnlyExplicit) and a bare zero Tier is rejected at wiring time.
//
// This is the framework's own review standard applied to itself: "Do the DEFAULTS fail closed? Fail
// looks like: a forgotten field means 'run it live'."
func TestToolSet_ForgottenTierIsRejected(t *testing.T) {
	noop := func(_ context.Context, _ *runState, _ ToolCall) error { return nil }

	t.Run("a forgotten Tier is rejected", func(t *testing.T) {
		ts := NewToolSet[runState]().
			Add(Capability{Name: "delete_mailbox", Description: "destroys everything"}, noop)

		err := ts.Validate()
		if err == nil {
			t.Fatal("⛔ a capability with NO Tier passed Validate — the zero value is TierReadOnly, so " +
				"this destructive act would AUTO-RUN. A forgotten field must never mean 'run it live'.")
		}
		if !strings.Contains(err.Error(), "delete_mailbox") || !strings.Contains(err.Error(), "Tier") {
			t.Errorf("the error must name the capability AND the missing field, got: %v", err)
		}
	})

	t.Run("read-only must be opted into explicitly", func(t *testing.T) {
		ts := NewToolSet[runState]().
			Add(Capability{Name: "search", Description: "looks things up", Tier: TierReadOnlyExplicit}, noop)

		if err := ts.Validate(); err != nil {
			t.Fatalf("an EXPLICIT read-only declaration must be accepted: %v", err)
		}
		// And it must still behave as read-only: auto-runs.
		plan, err := ts.GateProgram([]ToolCall{{Name: "search"}}, RungAuto)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if len(plan.Apply) != 1 {
			t.Errorf("an explicit read-only capability must still auto-run, got Apply=%d Propose=%d",
				len(plan.Apply), len(plan.Propose))
		}
	})

	t.Run("the sentinel never routes as more permissive than read-only", func(t *testing.T) {
		// It is a DECLARATION-time marker, not a 5th tier. It must resolve to read-only for every
		// routing decision, and it must sort BELOW read-only so a stray comparison can never read it
		// as more permissive.
		if TierReadOnlyExplicit >= TierReadOnly {
			t.Error("the sentinel must sort BELOW TierReadOnly so it can never be mistaken for a more " +
				"permissive tier")
		}
		if got := MaxRungFor(TierReadOnlyExplicit); got != MaxRungFor(TierReadOnly) {
			t.Errorf("MaxRungFor(explicit) = %v, must equal MaxRungFor(TierReadOnly) = %v",
				got, MaxRungFor(TierReadOnly))
		}
		if got := TierReadOnlyExplicit.String(); got != TierReadOnly.String() {
			t.Errorf("String() = %q, want %q — the sentinel is a spelling, not a distinct tier",
				got, TierReadOnly.String())
		}
	})
}

// TestToolSet_ProposeOnly is the fix for friction a migrating consumer reported: Validate rejects a
// nil handler, so a capability the tier can NEVER auto-run still needed a handler — one written only
// to never be called.
//
// One ops agent had 4 propose-tier capabilities, so adopting ToolSet meant 4 tripwire handlers: dead code
// whose correctness depends on a runtime invariant rather than the type system. AddProposeOnly makes
// "there is no live handler" STRUCTURAL — the declaration says so, and there is no function to
// mistakenly call.
func TestToolSet_ProposeOnly(t *testing.T) {
	t.Run("a propose-tier capability needs no handler", func(t *testing.T) {
		ts := NewToolSet[runState]().
			AddProposeOnly(Capability{Name: "send_reply", Description: "sends it",
				Tier: TierIrreversible})

		if err := ts.Validate(); err != nil {
			t.Fatalf("a propose-only capability must be valid WITHOUT a handler — the tier means it can "+
				"never run live, so a handler would be code that must never be called: %v", err)
		}

		// It must still reach the model, and still be gated to Propose.
		plan, err := ts.GateProgram([]ToolCall{{Name: "send_reply"}}, RungAuto)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if len(plan.Propose) != 1 || len(plan.Apply) != 0 {
			t.Errorf("must be withheld, got Apply=%d Propose=%d", len(plan.Apply), len(plan.Propose))
		}
		if !strings.Contains(RenderCapabilities(ts.Capabilities()), "send_reply") {
			t.Error("a propose-only capability must still be rendered — the model has to be able to emit it")
		}
	})

	t.Run("declaring an auto-runnable capability propose-only is rejected", func(t *testing.T) {
		// A reversible tier CAN auto-run, so "propose-only" would be a lie: the gate would route it to
		// Apply and ApplyPlan would find nothing to run — the silent drop, re-introduced by declaration.
		ts := NewToolSet[runState]().
			AddProposeOnly(Capability{Name: "set_title", Description: "renames", Tier: TierReversible})

		if err := ts.Validate(); err == nil {
			t.Error("a REVERSIBLE capability declared propose-only must be rejected — its tier lets the " +
				"gate route it to Apply, where there is no handler: the silent drop, by declaration")
		}
	})

	t.Run("a propose-only call is never executed", func(t *testing.T) {
		var st runState
		ts := NewToolSet[runState]().
			AddProposeOnly(Capability{Name: "send_reply", Description: "sends", Tier: TierIrreversible})

		// Even forced through the Apply half, there is no handler to run and the refusal is loud.
		errs := ts.ApplyPlan(context.Background(), &st, Plan{Apply: []ToolCall{{Name: "send_reply"}}})
		if len(st.applied) != 0 {
			t.Fatal("a propose-only capability was EXECUTED")
		}
		if len(errs) != 1 {
			t.Errorf("the attempt must be reported, got: %v", errs)
		}
	})
}

// TestToolSet_Validate catches the three declaration mistakes at wiring time, before a run.
func TestToolSet_Validate(t *testing.T) {
	noop := func(_ context.Context, _ *runState, _ ToolCall) error { return nil }

	t.Run("duplicate name", func(t *testing.T) {
		ts := NewToolSet[runState]().
			Add(reversible("dup"), noop).
			Add(Capability{Name: "dup", Description: "second", Tier: TierIrreversible}, noop)
		if err := ts.Validate(); err == nil {
			t.Error("a duplicate name is unreachable and its tier silently ignored — Validate must reject it")
		}
	})

	t.Run("empty name", func(t *testing.T) {
		ts := NewToolSet[runState]().Add(Capability{Description: "x", Tier: TierReversible}, noop)
		if err := ts.Validate(); err == nil {
			t.Error("a capability with no name can never be emitted by the model")
		}
	})

	t.Run("nil handler", func(t *testing.T) {
		ts := NewToolSet[runState]().Add(reversible("orphan"), nil)
		if err := ts.Validate(); err == nil {
			t.Error("a spec with no handler is the silent-drop bug, declared at wiring time")
		}
	})

	t.Run("valid set passes", func(t *testing.T) {
		ts := NewToolSet[runState]().Add(reversible("ok"), noop)
		if err := ts.Validate(); err != nil {
			t.Errorf("a well-formed set was rejected: %v", err)
		}
	})
}

// TestToolSet_CapabilitiesFeedsTheRenderer closes the loop: the SAME declaration that carries the
// handler is what the model is told about. There is no second list to drift.
func TestToolSet_CapabilitiesFeedsTheRenderer(t *testing.T) {
	ts := NewToolSet[runState]().
		Add(reversible("set_title", CapabilityArg{Name: "title", Type: "string", Required: true}),
			func(_ context.Context, _ *runState, _ ToolCall) error { return nil })

	rendered := RenderCapabilities(ts.Capabilities())
	if !strings.Contains(rendered, "set_title") || !strings.Contains(rendered, "title") {
		t.Errorf("the registered capability did not reach the prompt: %q", rendered)
	}
}

// TestCapabilitiesFrom_NoSecondCopy closes the hole a third-party reviewer found by building an agent
// that BOTH does and proposes the same opcodes — the shape neither production consumer has.
//
// ⛔ THE BUG. Capability is a strict SUBSET of Action ({Name, Description, Tier, Args} vs Action's
// superset) with NO converter, so an agent needing both halves hand-typed the contract a second time.
// The reviewer corrupted only the ToolSet copy's enum vocabulary and ran every check the docs
// prescribe:
//
//	ToolSet.Validate:   <nil>          (did not notice)
//	ActionSet.Validate: <nil>          (did not notice)
//	VerifyActionSet:    failed=false   (did not notice)
//	GateProgram:        err=<nil>, routed to Apply — the backend rejects it at verify
//
// ⭐ That is EXACTLY the bug Bind exists to eliminate — a fabricated vocabulary passing every green
// check — reproduced on the half Bind does not cover. Bind fixed the PROPOSE side and left the ACT
// side untouched. VerifyActionSetVocabulary cannot see it either: it takes an ActionSet, and the copy
// that drifted is the ToolSet's.
//
// The fix is to stop having a second copy: derive the capabilities FROM the actions.
func TestCapabilitiesFrom_NoSecondCopy(t *testing.T) {
	type st struct{ cat string }

	actions := NewActionSet[st]().
		Add(Action{Opcode: "set_category", Category: ChangeScalarSet, Input: "category",
			Description: "Categorise the expense.", Tier: TierReversible,
			Operands: []Operand{{Name: "category", Type: "enum", Required: true,
				EnumValues: []string{"travel", "meals", "software"}}}},
			func(_ context.Context, s *st, _ Action) (string, error) { return s.cat, nil })

	// ⭐ ONE declaration, both halves. The capability is DERIVED, so it cannot drift.
	caps := CapabilitiesFrom(actions)
	if len(caps) != 1 {
		t.Fatalf("expected one derived capability, got %d", len(caps))
	}
	c := caps[0]
	if c.Name != "set_category" || c.Tier != TierReversible {
		t.Errorf("identity and tier must carry over, got %+v", c)
	}
	if len(c.Args) != 1 || len(c.Args[0].EnumValues) != 3 {
		t.Fatalf("the operand contract must carry over verbatim, got %+v", c.Args)
	}

	// The vocabulary the MODEL is told is now the same object the premise checks — drift is impossible.
	if c.Args[0].EnumValues[2] != "software" {
		t.Errorf("enum drifted in the derivation: %v", c.Args[0].EnumValues)
	}

	// And the derived capability still gates correctly: a value outside the vocabulary is rejected
	// BEFORE it reaches a handler, instead of being routed to Apply and dying at the backend.
	if err := VerifyCall(caps, ToolCall{Name: "set_category", Args: map[string]any{"category": "saas"}}); err == nil {
		t.Error("⛔ a value outside the DERIVED vocabulary was accepted — the whole point is that the " +
			"model is told exactly what the backend accepts")
	}
}

// TestToolSet_CapabilitiesDoesNotAlias closes a silent state-corruption bug a scale review found.
//
// ⛔ Capabilities() returned the BACKING SLICE. The natural way to build a per-tenant view — take
// what it returns and tighten a tier — mutated the process-wide set for every subsequent tenant:
//
//	after mutating the returned slice: Apply=0 Propose=1
//
// And the corruption was COHERENT, so invisible: ApplyPlan's safety re-check reads the same mutated
// specs, so the gate and the dispatcher agree with each other while both disagree with the
// declaration. A test asserting the tier would also pass.
//
// ⚠️ The fix does NOT make per-tenant policy work — that is still unmodelled (Tier is static). It
// makes the WRONG WAY fail loudly instead of silently, which is the difference between a missing
// feature and a data-corruption bug.
func TestToolSet_CapabilitiesDoesNotAlias(t *testing.T) {
	ts := NewToolSet[runState]().
		Add(reversible("tag_item"), func(_ context.Context, _ *runState, _ ToolCall) error { return nil })

	caps := ts.Capabilities()
	caps[0].Tier = TierIrreversible // a consumer tightening a tier for one tenant

	plan, err := ts.GateProgram([]ToolCall{{Name: "tag_item"}}, RungAuto)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(plan.Apply) != 1 {
		t.Errorf("⛔ mutating the slice Capabilities() returned CORRUPTED the shared set — the act is "+
			"now withheld for every tenant (Apply=%d Propose=%d)", len(plan.Apply), len(plan.Propose))
	}
}

// TestCapabilitiesFrom_CarriesList is the regression guard for a LOSSY conversion — the bug class
// CapabilitiesFrom exists to prevent, reproduced through the converter itself.
//
// ⛔ CapabilityArg has a List field; Operand did not. So a multi-valued operand was rendered to the
// model as "One of: a, b, c" and then VerifyCall REJECTED a correct multi-value emission — the whole
// program discarded because the model believed what the prompt said.
func TestCapabilitiesFrom_CarriesList(t *testing.T) {
	type st struct{}
	set := NewActionSet[st]().
		Add(Action{Opcode: "tag_item", Category: ChangeAlwaysMints, Tier: TierReversible,
			Description: "Tag it.",
			Operands: []Operand{{Name: "tags", Type: "enum", Required: true, List: true,
				EnumValues: []string{"a", "b", "c"}}}}, nil)

	caps := CapabilitiesFrom(set)
	if len(caps) != 1 || !caps[0].Args[0].List {
		t.Fatal("⛔ List was DROPPED by the bridge — the model is told 'One of' for a multi-valued arg")
	}
	if err := VerifyCall(caps, ToolCall{Name: "tag_item", Args: map[string]any{"tags": []any{"a", "b"}}}); err != nil {
		t.Errorf("a CORRECT multi-value emission was rejected: %v", err)
	}
}

// TestCapabilitiesFrom_OffersWithheldActs is the regression guard for a COMPOSITION TRAP: the kit's
// two flagship recommendations combined into a silent bug in the most common agent shape — one that
// both acts and asks.
//
// ⛔ CapabilitiesFrom used to EXCLUDE withheld acts, reasoning "they are surfaced as an ask, not
// offered as a callable tool." But the model is not told they exist, and models invent opcodes —
// which is the kit's own stated premise. So an emitted withheld act is an UNKNOWN name, and
// GateProgram rejects the WHOLE PROGRAM (deliberately, so a multi-op write is never half-applied):
//
//	caps=1  err=unknown capability "issue_refund"  Apply=0  Propose=0
//
// ⭐ The valid set_tag call was discarded along with it. And the workaround was to hand-build the
// full capability list — the exact duplication CapabilitiesFrom exists to remove.
//
// The fix: OFFER withheld acts. That is what the tier gate is FOR — it routes them to Propose. Hiding
// them from the model does not prevent the act; it only prevents the gate from doing its job.
func TestCapabilitiesFrom_OffersWithheldActs(t *testing.T) {
	type st struct{}
	set := NewActionSet[st]().
		Add(Action{Opcode: "set_tag", Category: ChangeAlwaysMints, Tier: TierReversible,
			Description: "Tag the ticket."}, nil).
		Add(Action{Opcode: "issue_refund", Category: ChangeAlwaysMints, Tier: TierIrreversible,
			Description: "Refund the customer."}, nil)

	caps := CapabilitiesFrom(set)
	if len(caps) != 2 {
		t.Fatalf("both acts must be offered to the model, got %d — a withheld act the model does not "+
			"know about becomes an INVENTED opcode, and the whole program is rejected", len(caps))
	}

	plan, err := GateProgram(caps, []ToolCall{{Name: "set_tag"}, {Name: "issue_refund"}}, RungAuto)
	if err != nil {
		t.Fatalf("⛔ the whole program was rejected: %v — the valid call died with the withheld one", err)
	}
	if len(plan.Apply) != 1 || len(plan.Propose) != 1 {
		t.Errorf("the gate must ROUTE, not reject: want Apply=1 Propose=1, got Apply=%d Propose=%d",
			len(plan.Apply), len(plan.Propose))
	}
}

// ⭐ AN EMPTY SET IS THE ONE WIRING BUG THAT LOOKED LIKE SUCCESS. With no
// capabilities the model is offered no acts, emits none, and the run reports a
// SILENT outcome — which is legitimate elsewhere ("nothing needed doing"), so
// nothing said "you forgot to register your tools".
//
// A newcomer hit exactly this: their release-notes agent published nothing, and
// the run looked healthy. Every other forgotten-field case in this file fails
// loudly; this one has to as well, or the guarantee is uneven.
func TestToolSet_EmptySetIsRejected(t *testing.T) {
	err := NewToolSet[struct{}]().Validate()
	if err == nil {
		t.Fatal("an empty ToolSet must be REJECTED — a silent run is indistinguishable from success")
	}
	if !strings.Contains(err.Error(), "NO capabilities") {
		t.Errorf("the error must say what is wrong; got %q", err)
	}
}

// ⛔ THE BUG THIS PINS, WHICH I INTRODUCED WHILE "SIMPLIFYING". Merging CapabilityArg
// into Operand as an alias made `Args: a.Operands` compile — and it looks like the
// obvious cleanup, because the field-by-field copy it replaces is genuinely
// redundant once the types are one.
//
// It is not redundant. Actions() hands back the set's own backing array, so a
// consumer that edits what CapabilitiesFrom returned reaches through and corrupts
// the DECLARATION — after which the set renders the mutated vocabulary to the model
// and verifies against it. An enum the agent never declared becomes the contract.
//
// ⭐ The existing aliasing tests did not catch it: they check that two CALLS do not
// share, not that a call does not share with the SOURCE. A defensive copy needs a
// test that mutates and reads back through the original, or it will be optimised
// away by the next person who notices the copy looks pointless.
func TestCapabilitiesFrom_DoesNotShareWithTheDeclaration(t *testing.T) {
	type st struct{ cat string }
	set := NewActionSet[st]().
		Add(Action{Opcode: "set_category", Category: ChangeScalarSet, Input: "category",
			Description: "Categorise the expense.", Tier: TierReversible,
			Operands: []Operand{{Name: "category", Type: "enum", Required: true,
				EnumValues: []string{"travel", "meals"}}}},
			func(_ context.Context, s *st, _ Action) (string, error) { return s.cat, nil })

	caps := CapabilitiesFrom(set)
	if len(caps) != 1 || len(caps[0].Args) != 1 {
		t.Fatalf("setup: got %d capability/ies", len(caps))
	}

	// A consumer edits what it was handed — renaming an arg and rewriting an enum.
	caps[0].Args[0].Name = "renamed"
	caps[0].Args[0].EnumValues[0] = "CORRUPTED"

	// The declaration must be untouched, read back through the set itself.
	act, ok := set.Lookup("set_category")
	if !ok {
		t.Fatal("the action vanished from the set")
	}
	if act.Operands[0].Name != "category" {
		t.Errorf("⛔ the operand NAME was mutated through the returned slice: %q", act.Operands[0].Name)
	}
	if act.Operands[0].EnumValues[0] != "travel" {
		t.Errorf("⛔ the ENUM VOCABULARY was mutated through the returned slice: %v — "+
			"the set would now offer the model a value it never declared, and verify against it",
			act.Operands[0].EnumValues)
	}
}
