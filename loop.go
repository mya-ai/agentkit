package agentkit

import (
	"context"
)

// RunOnce is THE atom: one structured LLM call → a typed result V. Single-shot agents (an answer, a
// classification, a generated artifact like a daily digest) use it directly — no judge, no loop. It
// renders the agent's prompt, makes one GenerateStructured call (inheriting retry + model fallback +
// the ErrThrottled→release-to-queue contract for free), records latency, and returns (V, error).
//
// Go owns the write: RunOnce returns DATA; the agent's orchestrator decides what to persist. A provider
// throttle surfaces as the ErrThrottled-wrapped error so the consumer releases to its queue.
//
// obs may be nil (nil-safe). feedback: pass nil for a standalone RunOnce — it's the executor seam shared
// with Pair, which threads the prior round's judge feedback through it on a revision; a direct caller has
// none. (The param earns its place by making RunOnce the single executor atom Pair also drives, rather than
// duplicating the render+call+observe sequence.)
func RunOnce[Ctx, V any](
	ctx context.Context,
	caller StructuredCaller,
	agent OnceAgent[Ctx, V],
	rc Ctx,
	feedback []string,
	obs Observer,
) (V, error) {
	// A standalone RunOnce is a single, final round (Round 1 of 1) — no revision is possible, so the
	// executor should treat it as the final round. feedback is nil for a direct caller (Pair threads it).
	return runOnce[Ctx, V](ctx, caller, agent, rc, RoundInfo{Round: 1, MaxRounds: 1, Feedback: feedback}, obs, "once")
}

// runOnce is the shared executor step both RunOnce and Pair build on. info carries the round/budget +
// feedback (Render uses it to self-pace); role labels the Observer; info.Round is the Observer round.
func runOnce[Ctx, V any](
	ctx context.Context,
	caller StructuredCaller,
	agent OnceAgent[Ctx, V],
	rc Ctx,
	info RoundInfo,
	obs Observer,
	role string,
) (V, error) {
	system, dynamic := agent.Render(rc, info)
	r := agent.Role()
	// Narration: signal the turn is ABOUT to run (opens a fresh active entry so the user sees motion before
	// the ~55s call). nil-safe + no-op unless the observer implements NarratingObserver. The consumer's
	// Narrate decides the honest, mode-aware seed line (the kit carries no domain vocabulary).
	emitNarration(obs, Event{Kind: EventTurnStart, Role: role, Round: info.Round})
	start := nowFunc()
	v, err := GenerateStructured[V](ctx, caller, StructuredRequest{
		Model:       r.Model,
		Region:      r.Region,
		Fallback:    r.Fallback,
		PromptType:  r.PromptType,
		System:      system,
		Dynamic:     dynamic,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
	})
	if obs != nil {
		obs.ObserveLLM(role, info.Round, nowFunc().Sub(start))
	}
	// Narration: the turn finished — settle the active entry (nil-safe/no-op without a NarratingObserver).
	emitNarration(obs, Event{Kind: EventTurnEnd, Role: role, Round: info.Round})
	return v, err
}

// SameFunc reports whether two executor results are MATERIALLY equal — the degeneration guard. Pair uses
// it to detect "the executor ignored the judge's feedback and resubmitted essentially the same result"
// and short-circuit to a stalemate rather than burning the rest of the budget on repeated flawed
// reasoning. The agent supplies it (only the agent knows which fields are material); a nil SameFunc
// disables the guard (the loop then relies on the round ceiling alone). Generalized from a reference
// agent's verdictsMateriallyEqual.
type SameFunc[V any] func(a, b V) bool

// Pair runs the budgeted executor↔judge loop (the Reflection / evaluator-optimizer pattern) — a
// FIRST-CLASS OPTION for tasks with clear evaluation criteria (planning), NOT the mandatory shape. The
// judge is a pluggable Verifier so the signal can be execution/rules-based (strong) rather than an
// ungrounded same-model critic (which can DEGRADE results — Huang 2024 / Kamoi 2024). The loop:
//
// THE SHAPE OF A ROUND IS executor → judge → executor. The judge is INTERLEAVED between executor turns,
// never the terminal step: the loop is executor-first, then (judge → executor) pairs up to the budget, so
// it ALWAYS BOTH STARTS AND ENDS WITH THE EXECUTOR. The judge's only power is to GATE — approve (stop) or
// reject (trigger one more executor turn, within budget). It can never be the last word; the kit never
// ships, surfaces, or persists the judge's text. This is a MECHANISM guarantee only — what the final
// executor turn should SAY when the judge keeps rejecting (reconcile? ask the user? "both must agree or
// escalate"?) is POLICY that lives in the executor's PROMPT, reading RoundInfo.Feedback + IsFinalRound();
// the kit encodes none of it. The flow:
//
//  1. Executor produces a result V (round 1, no feedback).
//  2. A non-material result → OutcomeSilent immediately (no judge call — the model legitimately decided
//     there's nothing to say).
//  3. Judge verifies V. ok → OutcomeApplied with that result.
//  4. Not ok AND budget remains → run ANOTHER executor turn with the feedback, then back to (2). The
//     executor that runs after the last judge rejection IS the terminal result (executor-authored,
//     OutcomeApplied) — the loop does not end at the judge.
//  5. Degeneration guard (a revised result materially unchanged from the prior despite specific feedback)
//     → OutcomeStalemate: the executor is stuck ignoring the critique, so further turns won't help. This
//     is the ONE non-Applied terminal besides Silent/Error — and it carries the EXECUTOR's last output,
//     never the judge's feedback (the kit still never ships judge text).
//
// Go owns persistence — Pair returns a decision, it never writes. A provider throttle on any call →
// OutcomeError wrapping ErrThrottled (the consumer releases to its queue). same may be nil (guard off).
// obs may be nil.
func Pair[Ctx any, V Material](
	ctx context.Context,
	caller StructuredCaller,
	exec OnceAgent[Ctx, V],
	judge Verifier[V],
	same SameFunc[V],
	rc Ctx,
	cfg LoopConfig,
	obs Observer,
) Result[V] {
	var feedback []string // the judge's critique from the prior round (empty on round 1)
	var prev V            // the prior round's executor result (for the degeneration diff)
	var havePrev bool

	// An unset MaxRounds (0) defaults to the Reflection sweet spot (DefaultMaxRounds = 2: a draft + one
	// critique-driven revision), NOT 1 — a 1-round Pair is a degenerate loop (judge rejects round 1 →
	// immediate stalemate, no revision) and was the silent footgun a Pair author got by leaving the field
	// unset. An explicit MaxRounds: 1 is still honored for a deliberately single-shot
	// reflective check.
	maxRounds := cfg.MaxRounds
	if maxRounds < 1 {
		maxRounds = DefaultMaxRounds
	}

	// The loop is executor-FIRST, then (judge → executor) pairs — so it always ends with an executor turn.
	// `round` counts EXECUTOR passes (the unit of budget). After each executor turn the judge may approve
	// (stop) or reject; a rejection only ever buys ANOTHER executor turn (while budget remains). When the
	// budget is spent, we have just run an executor turn — and THAT is the terminal result. The judge never
	// terminates the loop and its text is never returned.
	for round := 1; round <= maxRounds; round++ {
		// --- Executor pass --- (RoundInfo lets the executor self-pace + react to the judge's feedback;
		// IsFinalRound() lets the PROMPT decide its final-turn policy — reconcile, or escalate to the user.)
		v, err := runOnce[Ctx, V](ctx, caller, exec, rc, RoundInfo{Round: round, MaxRounds: maxRounds, Feedback: feedback}, obs, "executor")
		if err != nil {
			return Result[V]{Outcome: OutcomeError, Rounds: round, Err: err}
		}

		// A non-material result is a legitimate, model-decided silent run — stop, write nothing.
		if !v.IsMaterial() {
			return Result[V]{Outcome: OutcomeSilent, Rounds: round, LastResult: v}
		}

		// Degeneration guard (round ≥ 2): the executor ignored the critique and resubmitted essentially the
		// same result. Further executor turns won't help, so stop — but ship the EXECUTOR's output (v), never
		// the judge's feedback. This is the only non-Applied terminal besides Silent/Error.
		if havePrev && same != nil && same(prev, v) {
			return Result[V]{Outcome: OutcomeStalemate, Rounds: round, LastResult: v}
		}

		// This executor turn is the LAST permitted one (budget spent). It IS the terminal result — the loop
		// ends at the executor, never at the judge. We do NOT run a final judge pass we couldn't act on: a
		// rejection here could only be surfaced as judge text (forbidden) or ignored (pointless). The judge
		// already shaped this turn via the prior round's feedback; the executor closed the loop.
		if round == maxRounds {
			return Result[V]{Outcome: OutcomeApplied, Result: v, LastResult: v, Rounds: round}
		}

		// --- Judge pass --- (only between executor turns, never terminal)
		ok, fb, err := judge.Verify(ctx, v)
		if err != nil {
			// A judge error is terminal for this run; the executor's (valid, material) v is NOT applied —
			// it was never verified, and the kit never ships an unverified result. We surface v as
			// LastResult for diagnostics. On a judge THROTTLE this is exactly right (release-to-queue; the
			// whole run re-runs cheaply). For a non-throttle judge failure it's mildly wasteful (a fresh
			// Worker call on redelivery), but applying an unverified verdict would violate "the judge gates
			// done" — so discarding it is the correct, safe choice.
			return Result[V]{Outcome: OutcomeError, Rounds: round, Err: err, LastResult: v}
		}
		if ok {
			return Result[V]{Outcome: OutcomeApplied, Result: v, LastResult: v, Rounds: round}
		}

		// Narration: the reviewer sent feedback — hand it to the Narrator's summary buffer ONLY (it never
		// becomes a user-visible entry and is never shown verbatim; the consumer's summarizer softens it into
		// one warm line). This preserves the no-leaked-judge-text invariant while giving the narration real
		// substance ("refining: the LOV step must come first"). nil-safe / no-op without a NarratingObserver.
		if len(fb) > 0 {
			emitNarration(obs, Event{Kind: EventCritique, Role: "judge", Round: round, Context: fb})
		}

		// Not approved AND budget remains — carry the feedback + this result into ANOTHER executor turn.
		feedback = fb
		prev = v
		havePrev = true
	}

	// Unreachable: the round==maxRounds branch above always returns on the final executor turn. Kept as a
	// total-function backstop (and to satisfy the compiler) — if it ever fires, ship the executor's last
	// output, never judge text.
	return Result[V]{Outcome: OutcomeApplied, Result: prev, LastResult: prev, Rounds: maxRounds}
}
