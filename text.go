package agentkit

import (
	"context"
)

// The kit's RunOnce atom comes in TWO flavors — structured (CompleteJSON → a typed V) and text
// (GenerateText → a string) — because a RunOnce agent's output is not always a JSON struct. A
// digest-style agent emits raw markdown, and forcing that through JSON mode wraps it in a
// {"description": "..."} envelope (a real bug, caught by dogfooding the first RunOnce consumer).
// TextCaller itself is declared in llm.go alongside the rest of the LLM seam.

// TextAgent is the executor seam for a free-text RunOnceText: it renders the prompt and names the role.
// Identical shape to OnceAgent except the result is the model's raw text (the agent post-processes /
// validates / persists it). feedback is unused for a standalone text call (kept for symmetry; a text
// reflection loop is not a current need).
type TextAgent[Ctx any] interface {
	Render(rc Ctx) (system, dynamic string)
	Role() RoleSpec
}

// RunOnceText is the free-TEXT atom: one GenerateText completion → the model's raw text. A daily digest
// agent is its canonical consumer (gather → one text pass → record the markdown). It inherits the same
// machinery as the structured atom — registry routing, the thinking-off budget, and the
// ErrThrottled→release-to-queue contract — because GenerateText sits on the same invokeTransport core.
//
// ONE PRIMITIVE, TWO TRANSPORTS. RunOnceText is NOT a second loop — it is the text *transport flavor*
// of the same atom RunOnce is the structured flavor of: both are render → ONE LLM call → observe, sharing
// the RoleSpec/Observer/throttle discipline. They cannot be one Go function because the result types are
// genuinely different (a typed V via GenerateStructured[V] vs a string via GenerateText) and collapsing
// them would erase the compile-time typing that the {"description": ...}-envelope bug taught us to keep.
// So the kit keeps the atom in two transport flavors, each a thin wrapper over its core (runOnce /
// runOnceText); the LOOP COMPOSITIONS (Pair, RunToolLoop) build on the atom, which is what "one loop
// primitive" means. See docs/design/architecture.md.
//
// Go owns the write: RunOnceText returns the text; the agent decides what to persist (a digest agent's
// "always record something, even on a quiet day" lives in the agent, not here — the kit never writes).
// An empty result is returned as-is (not an error) — the agent decides whether empty is acceptable (a
// digest agent treats empty as a redeliver). obs may be nil.
func RunOnceText[Ctx any](
	ctx context.Context,
	caller TextCaller,
	agent TextAgent[Ctx],
	rc Ctx,
	obs Observer,
) (string, error) {
	system, dynamic := agent.Render(rc)
	// A standalone text call is a single, final round (Round 1 of 1) — mirrors runOnce's RoundInfo so the
	// two transport flavors share the same observation discipline (role label + round).
	return runOnceText(ctx, caller, agent.Role(), system, dynamic, 1, obs)
}

// runOnceText is the free-text core, the sibling of runOnce: a rendered (system, dynamic) prompt → ONE
// GenerateText call → observe → text. Kept as a shared core (rather than inlined) so the text transport
// flavor visibly shares the atom's structure with the structured flavor — "one primitive, two transports,"
// not two copy-pasted bodies (the honesty fix). It takes the already-rendered prompt (not Ctx/agent) —
// rendering is the public wrapper's job, so this core is transport-only, mirroring how runOnce's call body
// is the part below Render. round labels the Observer (1 for a standalone call; a future text loop would
// pass its round).
func runOnceText(
	ctx context.Context,
	caller TextCaller,
	r RoleSpec,
	system, dynamic string,
	round int,
	obs Observer,
) (string, error) {
	start := nowFunc()
	res, err := caller.GenerateText(ctx, GenerateTextRequest{
		Model:       r.Model,
		Region:      r.Region,
		PromptType:  r.PromptType,
		System:      system,
		Dynamic:     dynamic,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
	})
	if obs != nil {
		obs.ObserveLLM("once_text", round, nowFunc().Sub(start))
	}
	if err != nil {
		return "", err
	}
	return res.Text, nil
}
