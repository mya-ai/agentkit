package agentkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// TOOLSET — the missing half of the capability model: DISPATCH.
//
// THE PROBLEM. The kit declares (Capability), renders (RenderCapabilities), verifies (VerifyCall)
// and gates (GateProgram) — then hands back Plan.Apply and STOPS. So every consumer hand-writes a
// `switch call.Name`, and a capability's name ends up living in THREE places nothing reconciles:
//
//	1. the Capability spec        — what the model is told
//	2. a name constant            — what the switch matches on
//	3. a `case` arm               — what actually runs
//
// Miss #2 or #3 and the failure is SILENT AND TOTAL: the model emits the name, VerifyCall passes
// (the spec is real), GateProgram routes it to Apply — and the switch matches nothing, so the call
// VANISHES with no error and no log. The run reports success. The compiler cannot see it.
//
// That is not hypothetical: one production agent carried 11 capability specs, 10 name constants and 8
// switch arms, plus a 228-line drift TEST written to make the mistake loud — because nothing could
// make it impossible. A second agent had the same three parallel lists.
//
// ⚠️ AND DISPATCH IS ONLY ONE OF ITS FOUR name-switch sites (capability_drift_test.go guards all
// four). Adopting ToolSet unifies the apply switch; wellformed.go, proposals.go and runner.go's
// fallback predicates still switch on capability names. Port that test's assertions onto
// ToolSet.Capabilities() before deleting it, or the remaining three lose their only guard.
//
// THE FIX. Declare the spec and its handler TOGETHER, in one call:
//
//	tools := agentkit.NewToolSet[myState]().
//	    Add(agentkit.Capability{
//	        Name: "set_due_date", Tier: agentkit.TierReversible,
//	        Description: "Set the item's due date — only when the evidence states a real deadline.",
//	        Args: []agentkit.CapabilityArg{{Name: "due", Type: "date", Required: true}},
//	    }, func(ctx context.Context, s *myState, c agentkit.ToolCall) error {
//	        return store.SetDue(ctx, s.ItemID, c.ArgString("due"))
//	    })
//
//	system := prompt + agentkit.RenderCapabilities(tools.Capabilities()) // same declaration
//	plan, err := tools.GateProgram(verdict.Calls, agentkit.RungAuto)     // same declaration
//	errs := tools.ApplyPlan(ctx, &state, plan)                           // same declaration
//
// The name is written ONCE. There is no second place for it to drift to. To add a capability, copy
// the whole Add call; to remove one, delete it — both single edits that cannot be half-done.
//
// WHY THE HANDLER TAKES *S. Real handlers are NOT independent: they share mutable state (a draft
// written by one call and composed after the loop by another), carry per-call business brakes that
// SKIP without failing, and accumulate errors rather than aborting. A registry of
// func(ctx, ToolCall) error would force all of that into closures over a struct — which is the
// coupling this type exists to remove, not relocate.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// RunFunc performs one verified, gated call. S is the agent's per-run state.
//
// It runs ONLY for calls the tier gate placed in Plan.Apply — never for a withheld one.
//
//   - Return nil when the call was performed OR DELIBERATELY SKIPPED. A business brake ("the user
//     set this date themselves") is a normal outcome, not an error.
//   - Return an error when the call SHOULD have been performed and failed. ApplyPlan COLLECTS it and
//     CONTINUES to the remaining calls; it never aborts the run. Losing one write is bad; losing the
//     writes that would have succeeded after it is worse.
type RunFunc[S any] func(ctx context.Context, state *S, call ToolCall) error

// ToolSet is an agent's capability set: for each capability, WHAT it is (the spec the model reads)
// and HOW to perform it (the handler your code runs) — declared together, so the name exists once.
//
// The zero value is not usable; construct with NewToolSet.
type ToolSet[S any] struct {
	caps     []Capability
	handlers map[string]RunFunc[S]
	// proposeOnly marks capabilities declared via AddProposeOnly: the tier can never auto-run them, so
	// they legitimately have no handler and Validate must not demand one.
	proposeOnly map[string]bool
}

// NewToolSet returns an empty set. S is your agent's per-run state type.
func NewToolSet[S any]() *ToolSet[S] {
	return &ToolSet[S]{handlers: make(map[string]RunFunc[S]), proposeOnly: make(map[string]bool)}
}

// Add declares one capability and the handler that performs it. Returns the set, so declarations
// chain.
//
// ⭐ Declare the spec and the handler HERE, together — this is the whole point. The capability's
// name exists in exactly one place, so it cannot drift out of sync with what the model is told or
// with what runs.
//
// Duplicate names, an empty name and a nil handler are reported by Validate, not by this call, so a
// declaration block stays a declaration block. Call Validate once at wiring time.
func (ts *ToolSet[S]) Add(spec Capability, run RunFunc[S]) *ToolSet[S] {
	ts.caps = append(ts.caps, spec)
	ts.handlers[spec.Name] = run
	return ts
}

// AddProposeOnly declares a capability the tier can NEVER auto-run — so it needs no handler.
//
// ⭐ WHY. Without it, a propose-tier capability still had to supply a handler that must never be
// called: dead code whose correctness rests on a runtime invariant instead of the type system. A
// consumer migrating onto this library had FOUR such tripwires. Declaring it here makes "there is no
// live handler" structural — there is no function to mistakenly call, and no reviewer has to check
// that a handler is unreachable.
//
//	tools.AddProposeOnly(agentkit.Capability{
//	    Name: "send_reply", Tier: agentkit.TierIrreversible,
//	    Description: "SEND the reply. You prepare it; the user sends it.",
//	})
//
// The capability is still rendered to the model and still verified — only the execution half is
// absent. Handle it from plan.Propose, as you would any withheld call.
//
// ⚠️ Validate REJECTS this for a tier that CAN auto-run (read-only/reversible): the gate would route
// such a call to Apply, where there is no handler — re-introducing the silent drop by declaration.
func (ts *ToolSet[S]) AddProposeOnly(spec Capability) *ToolSet[S] {
	ts.caps = append(ts.caps, spec)
	ts.proposeOnly[spec.Name] = true
	return ts
}

// Capabilities returns the specs — for RenderCapabilities, so the prompt's tool list is generated
// from the SAME declaration that carries the handlers. Never hand-write a tool list.
func (ts *ToolSet[S]) Capabilities() []Capability {
	// ⭐ A COPY, not the backing slice. Returning ts.caps let a consumer building a per-tenant view
	// ("tenant B may not auto-tag") mutate a returned Capability and silently corrupt the
	// process-wide set for every subsequent tenant — and the corruption was COHERENT, since
	// ApplyPlan's re-check reads the same mutated specs, so gate and dispatch agreed with each other
	// while both disagreed with the declaration.
	//
	// ⚠️ This does not MAKE per-tenant policy work (Tier is static — that is a real gap). It makes the
	// wrong way fail loudly rather than silently.
	out := make([]Capability, len(ts.caps))
	copy(out, ts.caps)
	return out
}

// Lookup finds a declared capability by name.
//
// ⭐ The mirror of ActionSet.Lookup, and it exists for the same reason: if the declaration is the
// single source, reading it back must not require rebuilding an index over Capabilities().
//
// ⚠️ Its absence was an ASYMMETRY, not a boundary — the PROPOSE side could answer "what did I
// declare?" and the DO side could not. A consumer asserting "send_reply must be withheld by the
// gate" had to either scan Capabilities() by hand or reach for the package-internal
// lookupCapability. Reaching for an internal is a signal that the surface is missing something, not
// that the consumer is misbehaving.
//
// Returns a COPY of the spec (Capability is a value type; its Args slice is shared — treat the
// result as read-only, as with Capabilities()).
func (ts *ToolSet[S]) Lookup(name string) (Capability, bool) {
	if ts == nil {
		return Capability{}, false
	}
	return lookupCapability(ts.caps, name)
}

// GateProgram verifies the whole program against this set's specs, then splits it by tier. It is
// the package-level GateProgram over ts.Capabilities(); see that function for the failure semantics
// (an invalid call fails the WHOLE program — a half-applied multi-op write is worse than none).
func (ts *ToolSet[S]) GateProgram(program []ToolCall, rung AutonomyRung) (Plan, error) {
	return GateProgram(ts.caps, program, rung)
}

// ApplyPlan runs the handler for every call in plan.Apply, in order, and returns EVERY error that
// occurred (empty when all succeeded).
//
// ⛔ It NEVER runs plan.Propose. Those calls were withheld by the tier gate and can only be
// surfaced to a human — passing the whole plan here must not defeat that.
//
// A call whose name has no registered handler is returned as an ERROR, never skipped silently. That
// silent skip is the bug this type exists to make impossible.
func (ts *ToolSet[S]) ApplyPlan(ctx context.Context, state *S, plan Plan) []error {
	var errs []error
	for _, call := range plan.Apply {
		// ⭐ RE-CHECK THE TIER HERE. Plan is an exported struct with exported slices, so a caller can
		// hand-build one — and the natural "retry what the gate held back" mistake,
		// ApplyPlan(ctx, s, Plan{Apply: gated.Propose}), would otherwise EXECUTE an irreversible call
		// live. A safety guarantee that holds only for Plans of the right provenance is not structural.
		// Verified before this check existed: gate → Propose=[transfer_funds], then ran=[TRANSFERRED].
		if spec, ok := lookupCapability(ts.caps, call.Name); ok && MaxRungFor(spec.Tier) == RungAsk {
			errs = append(errs, fmt.Errorf("agentkit: %s: REFUSED — tier %s is above the auto-ceiling and "+
				"may only be PROPOSED, never executed (this Plan did not come from GateProgram)",
				call.Name, spec.Tier))
			continue
		}
		run, ok := ts.handlers[call.Name]
		if !ok || run == nil {
			// LOUD. The model emitted a call the gate approved and nothing can perform it — the
			// declaration and the wiring have diverged.
			errs = append(errs, fmt.Errorf("agentkit: capability %q has no registered handler "+
				"(declared but not wired — the call would otherwise vanish silently)", call.Name))
			continue
		}
		if err := run(ctx, state, call); err != nil {
			errs = append(errs, fmt.Errorf("agentkit: %s: %w", call.Name, err))
		}
	}
	return errs
}

// Validate reports every problem that would make this set misbehave at runtime. Call it once at
// wiring time: it is cheap, and it turns three classes of silent misbehaviour into one startup error.
//
//   - an EMPTY name — the model can never emit it
//   - a DUPLICATE name — capability lookup takes the FIRST spec, so the other is unreachable and its
//     tier is silently ignored (a duplicate that is irreversible would appear to be gated and not be)
//   - a spec with a NIL handler — declared but not wired: the silent-drop bug, caught before a run
func (ts *ToolSet[S]) Validate() error {
	var errs []error

	// ⭐ AN EMPTY SET IS A WIRING BUG THAT LOOKS LIKE SUCCESS. With no capabilities,
	// RenderCapabilities produces "", the model is offered no acts, it emits none, and the run
	// reports OutcomeSilent — which is a LEGITIMATE outcome elsewhere ("nothing needed doing"),
	// so nothing anywhere says "you forgot to register your tools". Every other forgotten-field
	// case in this file fails loudly; this one has to as well, or the guarantee is uneven.
	if len(ts.caps) == 0 {
		errs = append(errs, errors.New("agentkit: this ToolSet declares NO capabilities — the model would be "+
			"offered no acts, emit none, and the run would report a legitimate-looking silent outcome. If a "+
			"set with nothing to do is genuinely what you want, branch before the loop rather than running it "+
			"with an empty set"))
	}

	seen := make(map[string]bool, len(ts.caps))

	for _, c := range ts.caps {
		if c.Name == "" {
			errs = append(errs, errors.New("agentkit: a capability has an empty Name — the model can never emit it"))
			continue
		}
		if seen[c.Name] {
			errs = append(errs, fmt.Errorf("agentkit: duplicate capability %q — lookup takes the FIRST spec, "+
				"so the other is unreachable and its Tier is silently ignored", c.Name))
		}
		seen[c.Name] = true

		// ⭐ THE DESCRIPTION IS THE PROMPT — see the twin check in ActionSet.Validate.
		if strings.TrimSpace(c.Description) == "" {
			errs = append(errs, fmt.Errorf("agentkit: capability %q has an EMPTY Description — that field "+
				"IS THE PROMPT (RenderCapabilities shows it to the model), so an empty one offers a nameless "+
				"act and the model either ignores it or invents when to use it. Say what it does and WHEN to "+
				"use it", c.Name))
		}

		if ts.proposeOnly[c.Name] {
			// A propose-only declaration is a LIE unless the tier actually withholds it: the gate would
			// route an auto-runnable call to Apply, where no handler exists — the silent drop, declared.
			if MaxRungFor(c.Tier) != RungAsk {
				errs = append(errs, fmt.Errorf("agentkit: capability %q is declared propose-only but its "+
					"tier (%s) CAN auto-run — the gate would route it to Apply, where there is no handler",
					c.Name, c.Tier))
			}
		} else if run, ok := ts.handlers[c.Name]; !ok || run == nil {
			errs = append(errs, fmt.Errorf("agentkit: capability %q has no handler — the model would emit it, "+
				"the gate would approve it, and nothing would run", c.Name))
		}

		// ⭐ A FORGOTTEN Tier must never mean "run it live". Tier's zero value is TierReadOnly (it cannot
		// be renumbered — a test pins the int values), so an
		// omitted Tier is indistinguishable at runtime from a deliberate read-only one. Requiring the
		// explicit spelling makes "this may auto-run" a STATEMENT rather than an OMISSION.
		if c.Tier == TierReadOnly {
			errs = append(errs, fmt.Errorf("agentkit: capability %q declares no Tier — the zero value is "+
				"read-only, which AUTO-RUNS, so a forgotten field would silently make a destructive act "+
				"live. State it: TierReadOnlyExplicit / TierReversible / TierExternalReversible / "+
				"TierIrreversible", c.Name))
		}
	}
	return errors.Join(errs...)
}

// CapabilitiesFrom derives the model-facing capability list from an ActionSet — so an agent that both
// DOES and PROPOSES the same opcodes declares each one ONCE.
//
// ⛔ THE BUG IT REMOVES. Capability is a strict SUBSET of Action ({Name, Description, Tier, Args} vs
// Action's superset) and there was NO converter, so such an agent hand-typed the contract a second
// time. A third-party reviewer building exactly that shape corrupted only the ToolSet copy's enum
// vocabulary and ran every check the docs prescribe:
//
//	ToolSet.Validate:   <nil>          (did not notice)
//	ActionSet.Validate: <nil>          (did not notice)
//	VerifyActionSet:    failed=false   (did not notice)
//	GateProgram:        err=<nil>, routed to Apply — the backend rejects it at verify
//
// ⭐ That is the SAME bug Bind exists to eliminate — a fabricated vocabulary passing every green
// check — on the half Bind does not cover. Bind stopped the agent copying the BACKEND's contract;
// this stops it copying its OWN. VerifyActionSetVocabulary could not catch it either: it takes an
// ActionSet, and the copy that drifted was the ToolSet's.
//
//	actions, _ := agentkit.Bind(ctx, src, uses)          // contract fetched once
//	system := prompt + agentkit.RenderCapabilities(agentkit.CapabilitiesFrom(actions))
//
// The vocabulary the model is TOLD is now the same object the premise CHECKS, so they cannot diverge.
//
// ⭐ EVERY act is offered, INCLUDING the ones the tier always withholds — see the inline note in the
// loop below for why, and TestCapabilitiesFrom_OffersWithheldActs for the proof.
//
// ⚠️ An earlier version of THIS comment said the opposite ("only acts that can run live are
// included"), describing a filter that was deliberately REVERSED because it was a trap. The code,
// the inline comment and the test all agreed; only this line was stale — and a stale doc comment is
// the more dangerous half, because godoc and editor hover show it WITHOUT the inline reasoning. A
// reader trusting it would re-add the filter and reproduce the whole-program rejection it caused.
func CapabilitiesFrom[S any](set *ActionSet[S]) []Capability {
	if set == nil {
		return nil
	}
	out := make([]Capability, 0, len(set.Actions()))
	for _, a := range set.Actions() {
		// ⭐ WITHHELD ACTS ARE OFFERED TOO. Excluding them looked safe ("it is surfaced as an ask, not
		// a callable tool") and was a trap: the model is not told the act exists, models invent
		// opcodes anyway, and an unknown name makes GateProgram reject the WHOLE program — killing
		// the valid calls beside it. Verified: caps=1, err=unknown capability "issue_refund",
		// Apply=0, Propose=0.
		//
		// Offering it is what the tier gate is FOR: it ROUTES the act to Propose. Hiding it from the
		// model never prevented the act; it only prevented the gate from doing its job.
		// ⚠️ THE SLICE IS COPIED, AND THE COPY IS NOT OPTIONAL. CapabilityArg is now an alias for
		// Operand, so `Args: a.Operands` compiles and looks like the obvious simplification. It is a
		// bug: Actions() hands back the set's own backing array, so a consumer that edits an
		// EnumValues entry on what it got back CORRUPTS THE DECLARATION ITSELF — the set then renders
		// the mutated vocabulary to the model and verifies against it. Demonstrated, not theorised.
		//
		// The alias removed the FIELD-BY-FIELD copy, which is how List once went missing (added to one
		// struct and not the other). It does not remove the need to hand out a copy of the SLICE.
		args := make([]Operand, len(a.Operands))
		copy(args, a.Operands)
		for i := range args {
			// EnumValues is itself a slice — copying the outer one still shares these.
			if len(args[i].EnumValues) > 0 {
				ev := make([]string, len(args[i].EnumValues))
				copy(ev, args[i].EnumValues)
				args[i].EnumValues = ev
			}
		}
		out = append(out, Capability{
			Name: a.Opcode, Description: a.Description, Tier: a.Tier, Args: args,
		})
	}
	return out
}
