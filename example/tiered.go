package example

// This file is the REFERENCE TIERED tool-loop consumer — structural tier-safety in the loop
// (agentkit.RunToolLoopTiered). It is a "reminder assistant": it investigates with a READ tool, then wants
// to take a MUTATING action (create a reminder). The kit GUARANTEES the mutating action is never executed
// inside the loop — it is captured as a proposed action the consumer drafts and holds for the user. This is
// the shape any agent takes once it can both look things up and change them.

import (
	"context"
	"fmt"

	"github.com/mya-ai/agentkit"
)

// reminderArgs is the tool-call Args (either a lookup query or a create-reminder payload — one Args type
// for the one tool the executor drives each round; a richer consumer would carry a tool-name discriminator).
type reminderArgs struct {
	Query string `json:"query"`
	Text  string `json:"text"` // set when the executor wants to CREATE a reminder (the mutating intent)
}

// reminderTool is a TIERED tool: when asked to CREATE (Text set) it is an external side effect. We mark the
// whole tool TierExternalReversible so the loop NEVER runs a create live — it captures it as a proposal.
// (A real PA would register TWO tools — a read "lookup" and a mutating "create" — behind a registry; the
// example uses one tiered tool to keep the demo to a single seam. A registry for mixed tool sets is the follow-up.)
type reminderTool struct {
	created []string // records LIVE creates — must stay EMPTY (the safety guarantee)
}

func (t *reminderTool) Name() string        { return "create_reminder" }
func (t *reminderTool) Tier() agentkit.Tier { return agentkit.TierExternalReversible }
func (t *reminderTool) Call(_ context.Context, args reminderArgs) ([]doc, error) {
	// This is reached only for AUTO-tier tools; for this external-reversible tool the loop must propose
	// instead of calling, so a non-empty `created` after a run is a SAFETY VIOLATION the test catches.
	t.created = append(t.created, args.Text)
	return nil, nil
}

// reminderAgent is the executor: it decides each round whether to keep investigating or emit the create.
type reminderAgent struct{ model string }

func (a reminderAgent) Render(_ struct{}, info agentkit.ToolRoundInfo[doc]) (system, dynamic string) {
	system = "You are a reminder assistant. Investigate, then propose creating a reminder when ready " +
		"(emit a call with the reminder text). You never execute the create yourself — the system proposes it."
	dynamic = fmt.Sprintf("Round %d of %d.", info.Round, info.MaxRounds)
	return system, dynamic
}

func (a reminderAgent) Role() agentkit.RoleSpec {
	return agentkit.RoleSpec{Model: a.model, PromptType: "example-reminder", MaxTokens: 512}
}

// TieredRunner is the reference RunToolLoopTiered orchestrator. The point: when the loop returns a
// Proposed action, the consumer DRAFTS it for the user — it never executes it. On the user's approval
// (outside this loop) the action-executor runs it.
type TieredRunner struct {
	caller   agentkit.StructuredCaller
	tool     *reminderTool
	proposer func(ctx context.Context, userID, toolName, text string) error // draft-and-hold (a real consumer: catalog UpsertProposal)
	model    string
}

// NewTieredRunner wires the reference tiered consumer.
func NewTieredRunner(caller agentkit.StructuredCaller, tool *reminderTool, proposer func(ctx context.Context, userID, toolName, text string) error, model string) *TieredRunner {
	return &TieredRunner{caller: caller, tool: tool, proposer: proposer, model: model}
}

// assist runs one unit: the tiered loop investigates and, when it wants to mutate, the loop captures the
// intent as a proposal that THIS code drafts-and-holds. The mutating tool is never executed in the loop.
func (r *TieredRunner) assist(ctx context.Context, userID string) error {
	res := agentkit.RunToolLoopTiered[struct{}, reminderArgs, doc](
		ctx, r.caller, reminderAgent{model: r.model}, r.tool, struct{}{}, agentkit.LoopConfig{MaxRounds: 3},
		nil /*same*/, nil, /*obs*/
	)

	if res.Outcome == agentkit.OutcomeError {
		return fmt.Errorf("example tiered: loop error for %s (%s): %w", userID, res.StoppedReason, res.Err)
	}

	// Draft-and-hold every proposed (above-ceiling) action for the user. The kit guaranteed none of these
	// ran live; this code turns each into a human-gated proposal.
	for _, p := range res.Proposed {
		if err := r.proposer(ctx, userID, p.ToolName, p.Args.Text); err != nil {
			return fmt.Errorf("example tiered: draft proposal for %s: %w", userID, err)
		}
	}
	return nil
}
