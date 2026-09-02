package agentkit

import (
	"context"
	"fmt"
	"reflect"
)

// RunToolLoop is the kit's dynamic tool-using loop (ReAct), the single primitive that serves BOTH shapes
// of consumer (a retrieval agent → R=items; a planning agent's tool path → R=a proposed write-program).
// It is composed from the existing atom (the structured call) — it does NOT re-implement
// retry/throttle/fallback (GenerateStructured owns those) and it does NOT write (the loop returns DATA;
// Go disposes). See docs/design/architecture.md for why it is shaped this way.
//
// The key difference from Pair: Pair's judge sees only the executor's OUTPUT (a Verifier[V]); a tool loop's
// "judge" is the executor's NEXT round seeing the prior tool RESULTS and deciding Done-or-reformulate
// (reflect-on-results). That reflection is GROUNDED in retrieved data, so it is acceptable
// where Pair's ungrounded same-model self-critique is not (the Huang/Kamoi caveat is about the latter).

// Tool is what the loop dispatches: a named operation the KIT runs (the I/O the OnceAgent.Render contract
// forbids inside the agent). Generic over the call Args + the result item type R so the kit imports no
// consumer domain (a retrieval agent → R=a context item; a planning agent → R=a proposed write). Call returns a
// BATCH of results for one invocation.
type Tool[Args, R any] interface {
	Name() string
	Call(ctx context.Context, args Args) ([]R, error)
}

// ToolStep is the executor's structured output each round: either a tool-call to run (Call set, Done false)
// or a decision to stop (Done true). Rationale is for the stop log / diagnostics. The json tags must match
// what the executor's prompt is told to emit (GenerateStructured unmarshals the model's JSON into this).
type ToolStep[Args any] struct {
	Done      bool   `json:"done"`
	Call      *Args  `json:"call,omitempty"`
	Rationale string `json:"rationale,omitempty"`
}

// ToolRoundInfo carries what Pair's RoundInfo cannot: the prior tool RESULTS, so the executor can reflect
// on them (the reflect-on-results termination model). It embeds RoundInfo, reusing the round cap +
// self-pacing contract — the executor's Render can say "final round, commit what you have" exactly as
// a Pair executor can.
type ToolRoundInfo[R any] struct {
	RoundInfo
	PriorResults [][]R // results from earlier rounds (one batch per prior tool call) — the executor grades THESE
}

// ToolAgent is the executor seam RunToolLoop drives: render the prompt from the gathered context + the
// prior results, and name the model/role. It returns a ToolStep[Args] (via the structured call the loop
// makes). Identical philosophy to OnceAgent; the only difference is the round info carries prior results.
type ToolAgent[Ctx, Args, R any] interface {
	// Render builds the (system, dynamic) prompt for this round from the context + prior results.
	Render(rc Ctx, info ToolRoundInfo[R]) (system, dynamic string)
	// Role resolves the model/region/fallback/temperature/maxTokens for the executor's structured call.
	Role() RoleSpec
}

// ToolLoopResult is what RunToolLoop returns: the accumulated items + diagnostics. The loop returns DATA;
// the consumer (Go) ranks/dedups/budget-fits/gates it — the LLM never writes. Outcome mirrors the kit's
// loop outcomes: Applied (retrieved items, executor finished), Silent (executor decided it needs nothing),
// Stalemate (degeneration short-circuit — reformulating in circles), Error (LLM/transport or tool failure;
// a throttle wraps ErrThrottled so the consumer releases to its queue). StoppedReason names WHY it ended.
type ToolLoopResult[R any] struct {
	Items         []R
	Rounds        int
	Outcome       Outcome
	StoppedReason string
	// Err is set iff Outcome==OutcomeError. A provider throttle wraps ErrThrottled, so the consumer's
	// consumer does errors.Is(res.Err, ErrThrottled) and releases to its queue (the queue is the backoff)
	// — the same release-to-queue contract Pair's Result.Err carries. nil for every non-error outcome.
	Err error
}

// RunToolLoop runs the dynamic tool-using loop (ReAct over the structured atom). Each round: the executor
// renders a prompt (seeing prior results) and emits a ToolStep; if Done, stop; else the KIT runs the tool
// with the executor's args, accumulates the batch, and loops — up to cfg.MaxRounds (a hard CEILING, not a
// target; unset → DefaultMaxRounds). It exits on the executor's Done (reflect-on-results), a repeated
// identical tool call (degeneration guard), the ceiling, or an error.
//
// Composed from the existing atom: the executor call IS GenerateStructured[ToolStep[Args]] through the
// same StructuredCaller seam, so retry + provider fallback + the ErrThrottled→release-to-queue contract are
// inherited, not re-implemented. obs may be nil (nil-safe).
//
// same is the degeneration comparator (mirrors Pair's SameFunc): it reports whether two consecutive tool
// calls are MATERIALLY the same (the executor is reformulating in circles) → stop. nil falls back to a
// comparator-free default (a stable fmt key over the call) — correct for value-typed Args, but pass an
// explicit SameFunc when Args contains pointers/funcs/unexported fields the default would mis-compare.
//
// Go owns the write: RunToolLoop returns the items + an Outcome; the consumer disposes. A mutating-tool
// consumer gates which tools may run live via the reversibility tier (RunToolLoopTiered) — that composes
// on top of this loop; a retrieval agent's read tool needs no gate.
func RunToolLoop[Ctx, Args, R any](
	ctx context.Context,
	caller StructuredCaller,
	exec ToolAgent[Ctx, Args, R],
	tool Tool[Args, R],
	rc Ctx,
	cfg LoopConfig,
	same SameFunc[Args],
	obs Observer,
) ToolLoopResult[R] {
	// A read-only loop is the tiered loop with no tool above the auto-ceiling — share the ONE core (no
	// duplicated loop body, the no-bespoke discipline applied to the kit itself). nil tierOf ⇒ every call
	// runs live (the read path).
	r := runToolLoopCore[Ctx, Args, R](ctx, caller, exec, tool, nil /*tierOf*/, rc, cfg, same, obs)
	// Project the richer tiered result onto the read-only result (a read loop never proposes).
	return ToolLoopResult[R]{Items: r.Items, Rounds: r.Rounds, Outcome: r.Outcome, StoppedReason: r.StoppedReason, Err: r.Err}
}

// runToolLoopCore is the ONE shared tool-loop body that both RunToolLoop (read-only) and RunToolLoopTiered
// (tier-gated) build on — so the degeneration guard, ceiling semantics, and structured-call body live in a
// SINGLE place and can't diverge (the bug a review caught when the tiered variant was a copy). tierOf is the
// tier-gate hook: nil → run every call live (read path); non-nil → a call whose tier is above the
// auto-ceiling is captured as a ProposedAction and the loop STOPS (never executed). same is the optional
// degeneration comparator (nil → comparator-free fmt key).
func runToolLoopCore[Ctx, Args, R any](
	ctx context.Context,
	caller StructuredCaller,
	exec ToolAgent[Ctx, Args, R],
	tool Tool[Args, R],
	tierOf func() Tier, // nil for the read-only loop; else the (single) tool's tier
	rc Ctx,
	cfg LoopConfig,
	same SameFunc[Args],
	obs Observer,
) TieredLoopResult[Args, R] {
	// A tool loop is still a budgeted loop — an unset ceiling must NOT mean "infinite" (the same footgun
	// Pair guards). Default to the Reflection sweet spot.
	maxRounds := cfg.MaxRounds
	if maxRounds < 1 {
		maxRounds = DefaultMaxRounds
	}

	var items []R          // accumulated across rounds
	var priorResults [][]R // one batch per executed tool call (what the executor reflects on)
	var lastCall *Args     // the prior round's call (for the degeneration guard)

	role := exec.Role()

	for round := 1; round <= maxRounds; round++ {
		info := ToolRoundInfo[R]{
			RoundInfo:    RoundInfo{Round: round, MaxRounds: maxRounds},
			PriorResults: priorResults,
		}
		system, dynamic := exec.Render(rc, info)

		start := nowFunc()
		step, err := GenerateStructured[ToolStep[Args]](ctx, caller, StructuredRequest{
			Model:       role.Model,
			Region:      role.Region,
			Fallback:    role.Fallback,
			PromptType:  role.PromptType,
			System:      system,
			Dynamic:     dynamic,
			MaxTokens:   role.MaxTokens,
			Temperature: role.Temperature,
		})
		if obs != nil {
			obs.ObserveLLM("tool_executor", round, nowFunc().Sub(start))
		}
		if err != nil {
			// LLM/transport failure (a throttle wraps ErrThrottled) — terminal; the handler disposes
			// (errors.Is(Err, ErrThrottled) → release to your queue).
			return TieredLoopResult[Args, R]{Items: items, Rounds: round, Outcome: OutcomeError, StoppedReason: "llm_error", Err: err}
		}

		// The executor decided it has enough (or needs no retrieval at all on round 1).
		if step.Done || step.Call == nil {
			return TieredLoopResult[Args, R]{Items: items, Rounds: round, Outcome: outcomeForItems(items), StoppedReason: reasonForItems(items, "executor_done")}
		}

		// ⛔ A FORGOTTEN Tier MUST NOT MEAN "RUN IT LIVE". Tier's zero value is TierReadOnly, which
		// auto-runs, so a TieredTool whose author never wrote a Tier() body — or wrote one returning
		// the zero value — would otherwise execute a destructive call with no gate. The Capability
		// path has rejected this since it shipped (ToolSet.Validate, GateProgram); this path did not,
		// and a mutating tool reached tool.Call live. Read-only must be OPTED INTO
		// (TierReadOnlyExplicit) so "this may auto-run" is a statement, never an omission.
		if tierOf != nil && tierOf() == TierReadOnly {
			return TieredLoopResult[Args, R]{
				Items:         items,
				Rounds:        round,
				Outcome:       OutcomeError,
				StoppedReason: "tool_declares_no_tier:" + tool.Name(),
				Err: fmt.Errorf("agentkit: tool %q declares no Tier — the zero value is read-only, "+
					"which AUTO-RUNS, so a forgotten field would silently make a destructive act live. "+
					"State it: TierReadOnlyExplicit / TierReversible / TierExternalReversible / "+
					"TierIrreversible", tool.Name()),
			}
		}

		// TIER GATE: a tool above the auto-ceiling is NEVER called live — capture the intended call as
		// a proposal and stop. nil tierOf (the read path) skips this entirely.
		if tierOf != nil && MaxRungFor(tierOf()) == RungAsk {
			return TieredLoopResult[Args, R]{
				Items:         items,
				Rounds:        round,
				Outcome:       OutcomeApplied, // the run figured out WHAT to do; doing it is gated, not failed
				StoppedReason: "proposed_irreversible",
				Proposed: []ProposedAction[Args]{{
					ToolName:  tool.Name(),
					Tier:      tierOf(),
					Args:      *step.Call,
					Rationale: step.Rationale,
				}},
			}
		}

		// Degeneration guard: the executor re-emitted a MATERIALLY identical tool call (reformulating in
		// circles). Stop rather than burn budget. Uses the caller's SameFunc, else a comparator-free fmt key.
		if lastCall != nil && callsSame(same, *lastCall, *step.Call) {
			return TieredLoopResult[Args, R]{Items: items, Rounds: round, Outcome: OutcomeStalemate, StoppedReason: "degeneration_repeated_call"}
		}
		callCopy := *step.Call
		lastCall = &callCopy

		// Run the tool (the I/O the kit owns, not the agent).
		batch, terr := tool.Call(ctx, *step.Call)
		if terr != nil {
			// A broken organ is terminal — never silently swallow it.
			return TieredLoopResult[Args, R]{Items: items, Rounds: round, Outcome: OutcomeError, StoppedReason: "tool_error:" + tool.Name(), Err: terr}
		}
		items = append(items, batch...)
		priorResults = append(priorResults, batch)
	}

	// Ceiling reached without the executor saying Done — return what we gathered. The outcome follows the
	// SAME rule as the Done path (Applied iff we got items, else Silent): "did the loop produce anything?"
	// must not depend on HOW it ended (the inconsistency a review caught — the ceiling used to force Applied
	// even with 0 items). The ceiling is a safety backstop, not itself a success/failure signal.
	return TieredLoopResult[Args, R]{Items: items, Rounds: maxRounds, Outcome: outcomeForItems(items), StoppedReason: reasonForItems(items, "round_ceiling")}
}

// outcomeForItems is the ONE rule for "did the loop produce anything?": items → Applied, none → Silent.
// Shared by the Done path and the ceiling path so the outcome can't depend on how the loop ended.
func outcomeForItems[R any](items []R) Outcome {
	if len(items) == 0 {
		return OutcomeSilent
	}
	return OutcomeApplied
}

// reasonForItems annotates the stopped reason: a non-empty loop keeps the structural reason (executor_done /
// round_ceiling); an empty loop is reported as nothing_retrieved (the Silent case) regardless of how it ended.
func reasonForItems[R any](items []R, structural string) string {
	if len(items) == 0 {
		return "nothing_retrieved"
	}
	return structural
}

// callsSame reports whether two tool calls are materially the same (the degeneration check). It uses the
// caller's SameFunc when supplied; otherwise reflect.DeepEqual, which is structurally correct for value AND
// pointer-bearing Args (it follows pointers and compares pointee values). The earlier fmt.Sprintf("%+v")
// default embedded pointer ADDRESSES, so two logically-identical calls stringified differently and the
// degeneration guard silently never fired for pointer-containing Args (caught in review). A consumer with a
// type DeepEqual can't compare (funcs, channels) should pass an explicit SameFunc.
func callsSame[Args any](same SameFunc[Args], a, b Args) bool {
	if same != nil {
		return same(a, b)
	}
	return reflect.DeepEqual(a, b)
}
