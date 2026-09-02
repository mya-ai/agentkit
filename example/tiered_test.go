package example

import (
	"context"
	"fmt"
	"testing"

	"github.com/mya-ai/agentkit"
)

// scriptedReminderCaller emits a ToolStep[reminderArgs] per round from a script.
type scriptedReminderCaller struct {
	steps []agentkit.ToolStep[reminderArgs]
	round int
}

func (c *scriptedReminderCaller) CompleteJSON(_ context.Context, _ agentkit.StructuredRequest) (string, error) {
	step := agentkit.ToolStep[reminderArgs]{Done: true}
	if c.round < len(c.steps) {
		step = c.steps[c.round]
	}
	c.round++
	if step.Done {
		return `{"done":true}`, nil
	}
	return fmt.Sprintf(`{"done":false,"call":{"query":%q,"text":%q}}`, step.Call.Query, step.Call.Text), nil
}

// TestTieredRunner_ProposesMutationNeverExecutes is the end-to-end structural guarantee at the consumer level: the
// executor wants to create a reminder (a mutating, external-reversible action); the loop captures it as a
// proposal, the consumer drafts it, and the tool's live-execute path is NEVER hit.
func TestTieredRunner_ProposesMutationNeverExecutes(t *testing.T) {
	caller := &scriptedReminderCaller{steps: []agentkit.ToolStep[reminderArgs]{
		{Done: false, Call: &reminderArgs{Text: "call the dentist Monday"}},
	}}
	tool := &reminderTool{}

	var drafted []string
	proposer := func(_ context.Context, _ /*userID*/, _ /*toolName*/, text string) error {
		drafted = append(drafted, text)
		return nil
	}
	r := NewTieredRunner(caller, tool, proposer, "test/model")

	if err := r.assist(context.Background(), "u1"); err != nil {
		t.Fatalf("assist err: %v", err)
	}
	if len(tool.created) != 0 {
		t.Fatalf("SAFETY VIOLATION: the mutating tool executed live: %+v", tool.created)
	}
	if len(drafted) != 1 || drafted[0] != "call the dentist Monday" {
		t.Fatalf("expected the mutation drafted-and-held for the user, got %+v", drafted)
	}
}
