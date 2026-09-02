package agentkit

// This file defines the core seams the loop depends on: the LLM caller, the executor/judge agent
// interfaces, the model config, and the loop result types. The package doc is in doc.go.

import (
	"context"
	"time"
)

// Material is the one thing the loop needs from a RunOnce result: whether anything materially changed /
// needs the user this run. A non-material result is a legitimate, model-decided SILENT outcome — the loop
// stops and writes nothing (no judge call). This is the structural-silence contract at the loop level
// (the deterministic mode gate runs earlier, in the agent's orchestrator, before the loop is even
// entered). Mirrors a reference agent's Verdict.Material field.
type Material interface {
	IsMaterial() bool
}

// Verifier is the gate on "done" AND the executor+judge pair's judge signal. It is the abstraction that
// keeps Pair from hardcoding a Worker→Reviewer: a planning agent passes an LLM critic, a coding agent
// passes a test-runner, a deterministic agent passes a rules check — same Pair, the right signal each
// time. Verify reports whether the result is acceptable (ok), and if not, specific, addressable feedback
// the executor must act on in the next round.
//
// CRITICAL (the load-bearing research finding — Huang et al. 2024, Kamoi et al. 2024): an UNGROUNDED
// same-model critic can DEGRADE results; a critic only helps when it has signal the generator lacked.
// So prefer an execution- or rules-based Verifier over an LLM critic, and an LLM critic only when it adds
// signal. A single-call or tool-loop agent uses NO Verifier at all (RunOnce / RunToolLoop), and must not
// be forced to pay for one.
type Verifier[V any] interface {
	Verify(ctx context.Context, v V) (ok bool, feedback []string, err error)
}

// OnceAgent is the executor seam RunOnce drives: it renders the prompt for one structured call and names
// the model/role config. rc is the agent's gathered context (opaque to the kit); RoundInfo carries the
// round/budget + the prior round's feedback so a Pair revision is feedback-driven AND the executor can
// SELF-PACE (it knows when it's on the last round). The agent supplies this; the kit owns the call
// (retry/fallback/throttle via GenerateStructured) and the loop.
type OnceAgent[Ctx, V any] interface {
	// Render builds the (system, dynamic) prompt for this call from the gathered context + the round info.
	// system = the rules/format contract (→ SystemInstruction); dynamic = the per-call data + imperative.
	// Matches the StructuredRequest System/Dynamic split.
	Render(rc Ctx, info RoundInfo) (system, dynamic string)
	// Role resolves the model/region/fallback/temperature/maxTokens for this call (your config service
	// overrides the default; the agent resolves it). Kept on the agent so each role (executor vs judge)
	// can be tuned/routed independently.
	Role() RoleSpec
}

// RoundInfo is the budget + feedback the kit hands the executor each round so it can SELF-PACE — the
// implementation of principle 2 ("budget is a ceiling FED INTO context," previously doc-only because the
// Render seam couldn't carry it — an audit finding). Round is 1-based; MaxRounds is the
// ceiling; Feedback is the judge's critique from the prior round (empty on Round 1). An agent's Render can
// say "final round — commit your best result" when Round == MaxRounds, which directly counters the
// heavy-prompt truncation drop seen in a real eval. A standalone RunOnce gets {Round:1, MaxRounds:1}.
type RoundInfo struct {
	Round     int      // 1-based current round
	MaxRounds int      // the ceiling for this loop (DefaultMaxRounds when unset)
	Feedback  []string // the prior round's judge feedback (nil on Round 1)
}

// IsFinalRound reports whether this is the last permitted round — the executor should commit its best
// result now (no further revision is possible). A convenience for prompt rendering.
func (r RoundInfo) IsFinalRound() bool { return r.Round >= r.MaxRounds }

// RoleSpec is the per-call model configuration: which model, where, the fallback, and the sampling/length
// knobs. It maps 1:1 onto the fields StructuredRequest needs, so RunOnce builds the request directly.
// PromptType is a metrics/log label (e.g. "triage-worker", "digest"). An agent resolves it from its
// own config provider + model registry, one resolver per role (executor, critic).
type RoleSpec struct {
	Model       string  // primary model (registry ID, provider prefix selects transport)
	Region      string  // optional Vertex location (e.g. "global"); "" = process default
	Fallback    string  // fallback model tried after primary parse-misses; "" = none
	PromptType  string  // metrics/log label
	Temperature float64 // eval-tuned
	MaxTokens   int     // output cap
}

// DefaultMaxRounds is the Reflection sweet spot — a draft + one critique-driven revision, then stop. An
// unset LoopConfig{} (MaxRounds 0) uses this, NOT 1: a 1-round Pair is degenerate (the judge rejects the
// first draft → immediate stalemate, no revision ever happens). This is the kit's default so a Pair author
// who leaves MaxRounds unset gets the documented behavior, not a silent degenerate loop.
const DefaultMaxRounds = 2

// LoopConfig is the budget for a Pair loop. MaxRounds is the hard CEILING (the safety backstop, not the
// target) — one round = one executor pass + one judge pass. The loop exits on the FIRST judge approval
// (converge, not iterate-to-N) or a non-material executor result; only an exhausted budget (or a
// degeneration short-circuit) records a stalemate. Unset (0) → DefaultMaxRounds (2). An explicit 1 is
// honored (a deliberately single-shot reflective check).
type LoopConfig struct {
	MaxRounds int
}

// Outcome is the terminal result of a loop. The agent's orchestrator branches on it to dispose (apply /
// write nothing / record a stalemate / surface the error). The kit never writes — it returns this.
type Outcome int

const (
	// OutcomeApplied — the executor produced a material result the judge approved. The agent applies it.
	OutcomeApplied Outcome = iota
	// OutcomeSilent — the executor judged nothing material (a legitimate model-decided silent run). The
	// agent writes nothing. (RunOnce returns this when its single result is non-material.)
	OutcomeSilent
	// OutcomeStalemate — no judge approval within budget, or a degeneration short-circuit. The agent
	// records a stalemate (never ships the contested result). Carries the last feedback for diagnostics.
	OutcomeStalemate
	// OutcomeError — an LLM/transport failure. A provider throttle surfaces wrapping ErrThrottled so
	// the agent's consumer releases to its queue (the queue is the backoff); any other error redelivers.
	OutcomeError
)

func (o Outcome) String() string {
	switch o {
	case OutcomeApplied:
		return "applied"
	case OutcomeSilent:
		return "silent"
	case OutcomeStalemate:
		return "stalemate"
	case OutcomeError:
		return "error"
	default:
		return "unknown"
	}
}

// Result is what a loop returns. Result is the AGREED result iff Outcome==OutcomeApplied; LastResult is
// the executor's most recent output regardless of outcome (for diagnostics/evals — on a stalemate it's
// what the executor last produced). Err is set iff Outcome==OutcomeError. Rounds is how many executor
// passes ran (≥1).
//
// ⛔ THE KIT NEVER RETURNS JUDGE TEXT. The loop ends at the executor, so a critique is never a result —
// surfacing one was the leaked-critique bug. A consumer asking "why didn't it converge?" reads LastResult
// (what the executor last produced), never the judge's words. There is deliberately no field to hold them:
// a StalemateFeedback slot existed, was never assigned by anything, and was removed rather than left as an
// always-nil promise.
type Result[V any] struct {
	Outcome    Outcome
	Result     V
	LastResult V
	Rounds     int
	Err        error
}

// Observer is the optional, nil-safe metrics seam the loop emits to. The kit emits only the
// per-role-per-round LLM latency it is positioned to see; all business metrics (run outcomes, modes,
// wow-moment latency) stay in the consumer's own *Metrics (which implements this). Generalized from a
// reference agent's observeLLM. role ∈ {"executor","judge","once"}.
type Observer interface {
	ObserveLLM(role string, round int, dur time.Duration)
}

// nowFunc is time.Now, indirected so a test can pin it (latency timing only — not a business clock).
var nowFunc = time.Now
