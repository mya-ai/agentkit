package agentkit

import (
	"context"
	"fmt"
	"strings"
)

// This file implements STRUCTURAL safety in the tool loop — a tool above the auto-ceiling is
// NEVER executed live, only PROPOSED. "Irreversible → propose, never execute" becomes a property of the
// CODE (the dispatcher routes by tier), not a hope in the prompt.
//
// DRIFT GUARD: if your backend has its own reversibility enum, these become hand-maintained parallel
// enums. Write a cross-check test in YOUR code (the one place that may import both agentkit and your
// registry) that fails loudly if the int values or the must-propose-vs-may-auto classification ever
// diverge. Keep them in sync.
//
// Tier/AutonomyRung/MaxRungFor mirror a backend registry's model IN SPIRIT but are defined HERE,
// independently — agentkit is the framework and must NOT import a concrete service. Your registry's tier
// maps onto this kit tier at the seam; the two stay aligned by this shared vocabulary, not a shared import.

// Tier classifies a tool by how recoverable its effect is — the hard SAFETY FLOOR that caps how much
// autonomy the tool may ever run at. It is a property of the TOOL (declared by the tool, outside the
// agent's reach), never a policy choice. Ordered most→least recoverable.
type Tier int

// ⚠️ THE ZERO VALUE IS `TierReadOnly`, WHICH AUTO-RUNS — so a FORGOTTEN `Tier:` field would silently
// declare a destructive act as safe-to-run-live. That is the "a forgotten field means run it live"
// failure the review standard forbids.
//
// It cannot be fixed by renumbering: a consumer-side cross-check test pins these int values
// position-for-position against the backend's own reversibility enum, and shifting them would silently
// mis-tier every op in that registry.
//
// ⭐ So a bare zero `Tier` is REJECTED, in TWO places. `ToolSet.Validate` catches it at wiring time,
// and `GateProgram` catches it again on the path that actually gates — because a guard that only runs
// in `Validate` is a guard nobody runs: a grep for `.Validate()` across every consumer returned
// nothing, so the declaration-time check sat off the live path entirely. Both exist now, and the
// second is the one that makes the guarantee real.
//
// Declare the tier explicitly on every capability — read-only ones use `TierReadOnlyExplicit`.
const (
	// TierReadOnly — queries/lookups; no mutation. A retrieval agent's Search tool. Safe to run live.
	// ⚠️ Also the zero value — see the note above; state it explicitly, never by omission.
	TierReadOnly Tier = iota
	// TierReversible — a mutation with a real compensating undo (prior value captured / inverse defined).
	// Safe to run live within a bounded mandate (it can be rolled back).
	TierReversible
	// TierExternalReversible — an external side effect recoverable but not silently (send/booking). Above
	// the auto-ceiling: PROPOSE, never auto-execute.
	TierExternalReversible
	// TierIrreversible — delete / mass-comms / payments / perms. HARD CEILING — only ever PROPOSED.
	TierIrreversible
)

// TierReadOnlyExplicit declares a capability read-only ON PURPOSE.
//
// ⭐ It exists so that "this may auto-run" is always a STATEMENT, never an OMISSION. Because
// TierReadOnly is the zero value, a `Tier` left unset and a `Tier` deliberately set to read-only are
// otherwise indistinguishable — and one of them is a forgotten field on a destructive act.
//
// It is a NEGATIVE sentinel, deliberately: it sorts BELOW TierReadOnly, so it can never be mistaken
// for a more permissive tier, and `normalizeTier` maps it to TierReadOnly before any routing. The
// int-value cross-check is unaffected — the four real tiers keep their positions.
//
//	Add(Capability{Name: "search", Tier: agentkit.TierReadOnlyExplicit}, run)  // ✅ stated
//	Add(Capability{Name: "delete_everything"}, run)                            // ⛔ rejected by Validate
const TierReadOnlyExplicit Tier = -1

// normalizeTier resolves the explicit read-only sentinel to the real tier. Every routing decision
// (MaxRungFor, GateProgram) goes through it, so the sentinel never reaches comparison logic.
func normalizeTier(t Tier) Tier {
	if t == TierReadOnlyExplicit {
		return TierReadOnly
	}
	return t
}

func (t Tier) String() string {
	switch normalizeTier(t) {
	case TierReadOnly:
		return "read_only"
	case TierReversible:
		return "reversible"
	case TierExternalReversible:
		return "external_reversible"
	case TierIrreversible:
		return "irreversible"
	default:
		return "unknown"
	}
}

// AutonomyRung is how much autonomy a tool call is granted: ask (human approves) or auto (the loop runs it
// live). The kit commits only to the SAFETY FLOOR (MaxRungFor); a richer policy (silent vs auto) is the
// consumer's, deferred — exactly as a backend registry defers it.
type AutonomyRung int

const (
	// RungAsk — the floor: the call must be PROPOSED for human approval (the loop never executes it live).
	RungAsk AutonomyRung = iota
	// RungAuto — the loop may execute the call live (it is read-only or reversible).
	RungAuto
)

func (r AutonomyRung) String() string {
	switch r {
	case RungAsk:
		return "ask"
	case RungAuto:
		return "auto"
	default:
		return "unknown"
	}
}

// MaxRungFor is the safety floor, enforced in code (never the prompt): read-only and reversible tools may
// auto-run; external-reversible and irreversible tools are pinned to 'ask' (propose only) no matter what.
// This is the one piece of autonomy logic the kit commits to — the same floor a registry commits to.
func MaxRungFor(tier Tier) AutonomyRung {
	switch normalizeTier(tier) {
	case TierReadOnly, TierReversible:
		return RungAuto
	default: // TierExternalReversible, TierIrreversible
		return RungAsk
	}
}

// ParseTier is String()'s inverse: it maps a wire tier back to a Tier.
//
// ⭐ THE KIT OWNS THIS BECAUSE GETTING IT WRONG IS UNRECOVERABLE. Without an inverse, every
// ContractSource hand-rolled the mapping — and the zero Tier is TierReadOnly, which AUTO-RUNS. The
// first consumer to write one said so in a comment: an unrecognized
// tier falling through to the zero value "would silently promote an irreversible op to 'safe to run
// live', which is precisely the failure the tier system exists to prevent."
//
// ⚠️ IT FAILS TO THE SAFEST TIER, NOT THE ZERO ONE. An unknown tier returns
// (TierIrreversible, error) — so even a caller that ignores the error fails CLOSED. A future backend
// adding a tier name makes agents propose rather than act; that is the correct direction to be wrong.
//
// A recognized "read_only" returns TierReadOnlyExplicit, so a legitimately read-only act is not
// mistaken by Validate for a forgotten field.
func ParseTier(s string) (Tier, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read_only", "readonly":
		return TierReadOnlyExplicit, nil
	case "reversible":
		return TierReversible, nil
	case "external_reversible", "externalreversible":
		return TierExternalReversible, nil
	case "irreversible":
		return TierIrreversible, nil
	default:
		return TierIrreversible, fmt.Errorf("agentkit: unknown tier %q — failing to the SAFEST tier "+
			"(%s) so an unrecognized contract can never make an act auto-run", s, TierIrreversible)
	}
}

// TieredTool is a Tool that declares its reversibility Tier. RunToolLoopTiered dispatches by it: a tool the
// loop may auto-run (read-only/reversible) is CALLED live; a tool above the ceiling
// (external-reversible/irreversible) is captured as a ProposedAction and NEVER executed in the loop. This is
// the structural "model proposes, code disposes" guarantee — a mutation physically cannot auto-execute.
type TieredTool[Args, R any] interface {
	Tool[Args, R]
	Tier() Tier
}

// ProposedAction is a tool call the loop DECLINED to run live because the tool is above the auto-ceiling.
// The consumer turns it into a draft-and-hold proposal (human approval), then the action-executor runs it —
// NOT the loop. Args is the exact call the executor intended; the consumer replays it on approval.
type ProposedAction[Args any] struct {
	ToolName  string
	Tier      Tier
	Args      Args
	Rationale string // the executor's stated reason (from the ToolStep), for the proposal surface
}

// TieredLoopResult is what RunToolLoopTiered returns: the same fields as ToolLoopResult plus Proposed —
// the calls the loop declined to run live (above the auto-ceiling) for the consumer to draft-and-hold. It
// is its OWN type (parameterized on Args too) rather than a Proposed field on ToolLoopResult, so the
// read-only RunToolLoop's result stays clean (no Args parameter, no propose concept) — a consumer that
// never uses tiers never sees a ProposedAction.
type TieredLoopResult[Args, R any] struct {
	Items         []R
	Rounds        int
	Outcome       Outcome
	StoppedReason string
	Err           error
	Proposed      []ProposedAction[Args]
}

// RunToolLoopTiered is RunToolLoop with tier-gated dispatch. It is identical to RunToolLoop except the
// tool is a TieredTool and each intended call is routed by MaxRungFor(tool.Tier()):
//   - RungAuto (read-only/reversible) → the loop CALLS the tool live; results flow into the reflect carry.
//   - RungAsk (external-reversible/irreversible) → the loop CAPTURES the call as a ProposedAction and STOPS
//     (an irreversible act ends the live run; the human-accept gate + action-executor dispose of it). The
//     tool is never invoked.
//
// The captured proposal IS a usable result (OutcomeApplied) — the run did its job (it figured out what to
// do); the doing is gated. A consumer whose tools are ALL read-only (a retrieval agent) never hits propose
// and behaves exactly like RunToolLoop.
func RunToolLoopTiered[Ctx, Args, R any](
	ctx context.Context,
	caller StructuredCaller,
	exec ToolAgent[Ctx, Args, R],
	tool TieredTool[Args, R],
	rc Ctx,
	cfg LoopConfig,
	same SameFunc[Args],
	obs Observer,
) TieredLoopResult[Args, R] {
	// Delegate to the ONE shared loop core (tools.go), passing the tier gate. No duplicated loop body — the
	// degeneration guard, ceiling semantics, and structured-call body live in a single place (the no-bespoke
	// discipline applied to the kit itself: the tiered variant must not re-derive the read loop).
	return runToolLoopCore[Ctx, Args, R](ctx, caller, exec, tool, tool.Tier, rc, cfg, same, obs)
}
