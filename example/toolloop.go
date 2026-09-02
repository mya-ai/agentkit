package example

// This file is the REFERENCE TOOL-LOOP agent — the validating consumer for agentkit.RunToolLoop, the
// same way example.go (the morning-note agent) validates RunOnceText. It is a "research note" agent: gather
// a topic → loop calling a READ tool (a stand-in "search" organ), reflecting on the retrieved items each
// round until it has enough → record a note built from what it found.
//
// It demonstrates every seam a RunToolLoop consumer uses:
//   - a Tool[Args, R] — the I/O the KIT runs (the agent never calls it directly);
//   - a ToolAgent[Ctx, Args, R] — Render(rc, ToolRoundInfo) emitting a ToolStep + Role();
//   - an orchestrator that runs RunToolLoop and branches on the ToolLoopResult.Outcome.
//
// A production retrieval agent is this shape at scale: its Tool is a hybrid search index, its R is a
// richer retrieved item, and its ToolAgent's Render drives a real prime/build/reflect cognition. The
// example keeps the cognition trivial (a fixed two-query plan) so it stays dependency-free; the POINT is
// the loop wiring, which is identical.

import (
	"context"
	"fmt"
	"strings"

	"github.com/mya-ai/agentkit"
)

// --- the result type R: a retrieved document (the kit is generic over this) ------------------------

// doc is one retrieved item — the example's R. A production agent's R is a richer item type.
type doc struct {
	ID    string
	Title string
}

// --- the tool: a read-only "search" organ (the I/O the loop runs) ----------------------------------

// searchArgs is the tool-call Args the executor emits each round (a query). Generic Args in the kit.
type searchArgs struct {
	Query string `json:"query"`
}

// searchTool implements agentkit.Tool[searchArgs, doc]: a read-only retrieval over some corpus. The
// example backs it with an in-memory map; a production agent backs it with a real search index. Either
// way the kit runs Call — the agent only ever EMITS the args.
type searchTool struct {
	corpus map[string][]doc // query → docs (a stand-in for a real index)
}

func (t searchTool) Name() string { return "search" }

func (t searchTool) Call(_ context.Context, args searchArgs) ([]doc, error) {
	return t.corpus[args.Query], nil // nil for an unknown query (a real organ returns ranked hits)
}

// --- the ToolAgent: prime → build the query → reflect on prior results ------------------------------

// researchContext is the gathered state for one run (the topic to research). Re-gathered fresh per run.
type researchContext struct {
	userID string
	topic  string
}

// researchAgent implements agentkit.ToolAgent[researchContext, searchArgs, doc]. Its Render is the
// cognition — in the real brain this is the ContextModel-driven prompt; here it is a deterministic
// two-step plan (broad query, then a refining query) so the example needs no LLM judgment to be readable.
// The structured CALLER (the model in prod; a scripted fake in tests) turns this prompt into the ToolStep.
type researchAgent struct {
	model string
}

func (a researchAgent) Render(rc researchContext, info agentkit.ToolRoundInfo[doc]) (system, dynamic string) {
	system = "You are a research assistant. Each round, either emit a search query (done:false, call.query) " +
		"or stop (done:true) when the retrieved results cover the topic. Reflect on the results you already have."
	// The prompt SHOWS the executor what it already retrieved (the reflect-on-results contract) and where it
	// is in the budget (self-pacing) — exactly what a real cognition prompt renders.
	dynamic = fmt.Sprintf("Topic: %s\nRound %d of %d.\nResults so far: %d batch(es).",
		rc.topic, info.Round, info.MaxRounds, len(info.PriorResults))
	if info.IsFinalRound() {
		dynamic += "\nThis is the final round — stop and use what you have."
	}
	return system, dynamic
}

func (a researchAgent) Role() agentkit.RoleSpec {
	// As in the morning-note example: a PRODUCTION agent resolves this through the keyed TemplateProvider so
	// the {prompt, model} pair is live-tunable. Fixed here to stay dependency-free.
	return agentkit.RoleSpec{Model: a.model, PromptType: "example-research", MaxTokens: 512, Temperature: 0.2}
}

// --- the orchestrator: gather → RunToolLoop → record -----------------------------------------------

// ToolLoopRunner is the reference RunToolLoop orchestrator. It gathers the topic, runs the budgeted tool
// loop, and records a note from the retrieved docs. Like the morning-note Runner, the kit owns the loop
// (dispatch/reflect/cap/throttle); the agent owns gather, the tool, the cognition, and the disposition.
type ToolLoopRunner struct {
	caller agentkit.StructuredCaller
	tool   searchTool
	store  noteStore
	model  string
}

// NewToolLoopRunner wires the reference tool-loop agent. In production: pass your own provider adapter
// (it satisfies StructuredCaller — see docs/providers.md), a Tool backed by a real search index, and your
// own persistence.
func NewToolLoopRunner(caller agentkit.StructuredCaller, tool searchTool, store noteStore, model string) *ToolLoopRunner {
	return &ToolLoopRunner{caller: caller, tool: tool, store: store, model: model}
}

// research runs one unit: gather → RunToolLoop → record. It branches on the ToolLoopResult.Outcome exactly
// as a structured agent branches on a Pair Result — Applied → use the items; Silent → nothing to do;
// Error → propagate (a throttle releases the work to its queue); Stalemate → record what we have (degeneration isn't fatal
// for retrieval — partial context still beats none, the agent decides).
func (r *ToolLoopRunner) research(ctx context.Context, rc researchContext) error {
	res := agentkit.RunToolLoop[researchContext, searchArgs, doc](
		ctx, r.caller, researchAgent{model: r.model}, r.tool, rc, agentkit.LoopConfig{MaxRounds: 3},
		nil /*same: comparator-free default*/, nil, /*obs*/
	)

	switch res.Outcome {
	case agentkit.OutcomeError:
		// ErrThrottled propagates → the caller releases the work to its queue; any other error redelivers.
		return fmt.Errorf("example research: loop error for user %s (%s): %w", rc.userID, res.StoppedReason, res.Err)
	case agentkit.OutcomeSilent:
		// The executor decided nothing needed retrieving — nothing to record (a legitimate quiet outcome).
		return nil
	default: // OutcomeApplied or OutcomeStalemate — both carry usable items; Go owns the write.
		note := r.noteFrom(rc.topic, res.Items)
		return r.store.recordNote(ctx, rc.userID, note)
	}
}

// noteFrom builds the recorded markdown from the retrieved docs (the agent's disposition of the loop data).
func (r *ToolLoopRunner) noteFrom(topic string, docs []doc) string {
	if len(docs) == 0 {
		return fmt.Sprintf("# %s\n\nNo sources found yet.", topic)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nFound %d source(s):\n", topic, len(docs))
	for _, d := range docs {
		fmt.Fprintf(&b, "- %s\n", d.Title)
	}
	return b.String()
}
