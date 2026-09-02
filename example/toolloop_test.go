package example

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mya-ai/agentkit"
)

// scriptedStructuredCaller returns a queued ToolStep JSON per round (then defaults to done). It stands in
// for the model a real agent would use; in production your own provider adapter satisfies
// agentkit.StructuredCaller (see docs/providers.md).
type scriptedStructuredCaller struct {
	steps []agentkit.ToolStep[searchArgs]
	round int
	err   error
}

func (c *scriptedStructuredCaller) CompleteJSON(_ context.Context, _ agentkit.StructuredRequest) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	step := agentkit.ToolStep[searchArgs]{Done: true}
	if c.round < len(c.steps) {
		step = c.steps[c.round]
	}
	c.round++
	if step.Done {
		return fmt.Sprintf(`{"done":true,"rationale":%q}`, step.Rationale), nil
	}
	return fmt.Sprintf(`{"done":false,"call":{"query":%q}}`, step.Call.Query), nil
}

func qStep(q string) agentkit.ToolStep[searchArgs] {
	return agentkit.ToolStep[searchArgs]{Call: &searchArgs{Query: q}}
}

// TestToolLoopRunner_RetrievesAndRecords: the reference tool-loop agent runs two search rounds (broad then
// refining), reflects, stops, and records a note listing what it found — the RunToolLoop consumer shape
// end-to-end (a retrieval agent at small scale).
func TestToolLoopRunner_RetrievesAndRecords(t *testing.T) {
	caller := &scriptedStructuredCaller{steps: []agentkit.ToolStep[searchArgs]{
		qStep("swim lessons"),
		qStep("swim lessons level 2"),
		{Done: true, Rationale: "covered"},
	}}
	tool := searchTool{corpus: map[string][]doc{
		"swim lessons":         {{ID: "1", Title: "Intro to swimming"}},
		"swim lessons level 2": {{ID: "2", Title: "Level 2 swim test"}},
	}}
	store := &fakeStore{}
	r := NewToolLoopRunner(caller, tool, store, "test/model")

	if err := r.research(context.Background(), researchContext{userID: "u1", topic: "Nora swimming"}); err != nil {
		t.Fatalf("research err: %v", err)
	}
	if len(store.recorded) != 1 {
		t.Fatalf("expected one recorded note, got %d", len(store.recorded))
	}
	note := store.recorded[0]
	if !strings.Contains(note, "Intro to swimming") || !strings.Contains(note, "Level 2 swim test") {
		t.Fatalf("note should list both retrieved sources, got:\n%s", note)
	}
}

// TestToolLoopRunner_SilentWhenNothingRetrieved: the executor decides immediately it needs no retrieval →
// OutcomeSilent → nothing recorded (a legitimate quiet outcome, not an error).
func TestToolLoopRunner_SilentWhenNothingRetrieved(t *testing.T) {
	caller := &scriptedStructuredCaller{steps: []agentkit.ToolStep[searchArgs]{{Done: true, Rationale: "no retrieval needed"}}}
	store := &fakeStore{}
	r := NewToolLoopRunner(caller, searchTool{}, store, "test/model")

	if err := r.research(context.Background(), researchContext{userID: "u1", topic: "x"}); err != nil {
		t.Fatalf("silent run must not error: %v", err)
	}
	if len(store.recorded) != 0 {
		t.Fatalf("expected nothing recorded on a silent run, got %d", len(store.recorded))
	}
}

// TestToolLoopRunner_ThrottlePropagates: a throttle on the structured call surfaces so the orchestrator
// returns an agentkit.ErrThrottled-wrapped error (the consumer's handlr releases to SQS).
func TestToolLoopRunner_ThrottlePropagates(t *testing.T) {
	caller := &scriptedStructuredCaller{err: fmt.Errorf("429: %w", agentkit.ErrThrottled)}
	store := &fakeStore{}
	r := NewToolLoopRunner(caller, searchTool{}, store, "test/model")

	err := r.research(context.Background(), researchContext{userID: "u1", topic: "x"})
	if !errors.Is(err, agentkit.ErrThrottled) {
		t.Fatalf("expected ErrThrottled to propagate, got %v", err)
	}
	if len(store.recorded) != 0 {
		t.Fatalf("expected nothing recorded on a throttle, got %d", len(store.recorded))
	}
}
