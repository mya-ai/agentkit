// Package example is the REFERENCE AGENT for agentkit — a complete, runnable, ~150-line autonomous agent
// that demonstrates the full kit pattern with no external dependency. It is the pattern-match target the
// developer guide (docs/guides/building-an-agent.md) points to: copy THIS. A real agent is mostly its own
// domain logic, which is exactly the part you should not copy from anyone.
//
// The example is a "morning note" agent — a free-text (RunOnceText) agent, the shape a daily-digest agent
// takes: gather a tiny bit of context → one text LLM pass → "record" the result. It shows every
// seam a RunOnce agent uses:
//
//   - its CONTEXT type (noteContext) + a Gatherer (gather);
//   - a TextAgent (Render + Role) — the prompt + model config;
//   - an orchestrator (Run) — gather → RunOnceText → persist;
//   - RunBounded — the timeout every agent must apply, and the whole hosting contract.
//
// A structured (Pair) agent looks the same but: its output is a typed struct (satisfying agentkit.Material),
// it uses RunOnce + a Verifier via agentkit.Pair, and its Run branches on the agentkit.Outcome.
//
// A TOOL-LOOP agent (the third shape) is in toolloop.go: it gathers, then loops calling a read Tool via
// agentkit.RunToolLoop — reflecting on the retrieved items each round until it has enough — and records a
// note from what it found. That file is the reference RunToolLoop consumer (the Context-Platform "brain" is
// this shape at scale); copy it when your agent needs to INVESTIGATE (call a tool, look at results, decide
// to dig further) rather than answer in one pass.
package example

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mya-ai/agentkit"
)

// --- 1. The agent's CONTEXT (its gathered state) ---------------------------------------------------

// noteContext is what this agent reasons over for one unit (one user's morning note). A real agent's
// gather reads durable state (projects, evidence, calendar); here it's a couple of fields to keep the
// example self-contained. Re-gathered fresh per run (the kit threads decisions, not state).
type noteContext struct {
	userID    string
	timezone  string
	itemCount int // e.g. "you have N things on your plate" — stands in for real gathered evidence
}

// noteStore is the agent's durable seam — what it reads (gather) and writes (record). A real agent uses
// catalog gRPC; the example uses an interface so it's testable with a fake and has no prod dependency.
type noteStore interface {
	// gatherCount returns the user's open-item count (stands in for a real recency-biased gather).
	gatherCount(ctx context.Context, userID string) (int, error)
	// recordNote persists the generated note. The kit NEVER writes — the agent's Run calls this.
	recordNote(ctx context.Context, userID, markdown string) error
}

// --- 2. The TextAgent: the prompt + the model/role config ------------------------------------------

// noteAgent implements agentkit.TextAgent: it renders the (system, dynamic) prompt from the gathered
// context and names the model role. A real agent resolves Role through its keyed TemplateProvider;
// the example hardcodes a model id to stay dependency-free.
type noteAgent struct {
	model string
}

func (a noteAgent) Render(rc noteContext) (system, dynamic string) {
	system = "You are a warm morning-note assistant. Write 2-3 sentences of plain markdown. " +
		"If there's nothing on the user's plate, say so kindly — a quiet day is a good outcome."
	dynamic = fmt.Sprintf("User timezone: %s.\nThings on the plate today: %d.", rc.timezone, rc.itemCount)
	return system, dynamic
}

func (a noteAgent) Role() agentkit.RoleSpec {
	// NOTE for anyone copying this: a PRODUCTION agent resolves Model/Region through the keyed
	// agentkit.TemplateProvider (`p.Model("<agent>","main")` / `p.Region(...)`) so the {template, model}
	// pair is tunable without a redeploy. This example takes a fixed model id to stay dependency-free.
	return agentkit.RoleSpec{Model: a.model, PromptType: "example-morning-note", MaxTokens: 512, Temperature: 0.4}
}

// --- 3. The AgentRunner: the orchestrator (gather → RunOnceText → record) --------------------------

// Job is the example's own trigger payload — the fields THIS agent needs to do one
// unit of work. It is deliberately declared here rather than imported from the kit:
// agentkit has no opinion about how your agent is triggered (cron, a queue, an HTTP
// handler, a test), so the payload is yours to shape. Keep it small; it is a
// trigger, not a context object — the context is gathered fresh in gather().
type Job struct {
	UserID       string
	UserTimezone string // IANA tz for local-time framing; "" = UTC
}

// Runner implements agentkit.AgentRunner. It is the single self-contained unit of work: gather the
// user's context, make ONE free-text pass via the kit's RunOnceText atom, and ALWAYS record the result
// (a quiet day is still a note — the "Go owns the write, unconditionally" lesson). The error contract
// is the worker contract: agentkit.ErrThrottled surfaces so the caller can RELEASE the work back to its
// queue (the queue is the backoff), an empty model output is a redeliver, any other error redelivers.
type Runner struct {
	caller agentkit.TextCaller
	store  noteStore
	model  string
}

// NewRunner wires the example agent. In production you pass your own TextCaller (an adapter over
// whichever provider you use — see docs/providers.md) and your own persistence.
func NewRunner(caller agentkit.TextCaller, store noteStore, model string) *Runner {
	return &Runner{caller: caller, store: store, model: model}
}

// Run is the agentkit.AgentRunner entrypoint: one self-contained unit of work (gather → RunOnceText →
// record). The error contract is the worker contract — agentkit.ErrThrottled surfaces (the caller
// releases the work), an empty output redelivers, any other error redelivers.
func (r *Runner) Run(ctx context.Context, cmd Job) error {
	// --- gather (fresh, per run) ---
	rc, err := r.gather(ctx, cmd)
	if err != nil {
		return fmt.Errorf("example: gather user %s: %w", cmd.UserID, err)
	}

	// --- one free-text pass (the RunOnceText atom; inherits throttle→release + fallback) ---
	note, err := agentkit.RunOnceText[noteContext](ctx, r.caller, noteAgent{model: r.model}, rc, nil)
	if err != nil {
		// ErrThrottled propagates so the caller releases the work to its queue; anything else redelivers.
		return fmt.Errorf("example: generate user %s: %w", cmd.UserID, err)
	}
	note = strings.TrimSpace(note)
	if note == "" {
		// Empty model output → don't persist an empty note; redeliver once (the agent decides, not the kit).
		return fmt.Errorf("example: empty output for user %s", cmd.UserID)
	}

	// --- record (Go owns the write; unconditional — a quiet day is still a note) ---
	if err := r.store.recordNote(ctx, cmd.UserID, note); err != nil {
		return fmt.Errorf("example: record user %s: %w", cmd.UserID, err)
	}
	return nil
}

func (r *Runner) gather(ctx context.Context, cmd Job) (noteContext, error) {
	tz := cmd.UserTimezone
	if tz == "" {
		tz = "UTC"
	}
	n, err := r.store.gatherCount(ctx, cmd.UserID)
	if err != nil {
		return noteContext{}, err
	}
	return noteContext{userID: cmd.UserID, timezone: tz, itemCount: n}, nil
}

// --- 4. Wiring the Handler -------------------------------------------------------------------------

// RunTimeout bounds the WHOLE run (gather + LLM + record) so a stuck dependency can never wedge a worker
// slot indefinitely. EVERY agent must set one. It is exported so whatever drives this agent — a queue
// consumer, a cron tick, an HTTP handler — applies the same bound; the kit does not impose one for you.
const RunTimeout = 60 * time.Second

// RunBounded is how a caller should invoke this agent: the run timeout applied around Run. This is the
// entire hosting contract — agentkit has no queue, no scheduler and no opinion about what triggers you.
// Wire it to SQS, Pub/Sub, a cron, or a test; the error contract (see Run) is what your caller branches on.
func (r *Runner) RunBounded(ctx context.Context, cmd Job) error {
	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	return r.Run(ctx, cmd)
}
