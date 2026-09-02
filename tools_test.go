package agentkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// --- fakes -------------------------------------------------------------------------------------------

// queryArgs is the example consumer's tool-call args: a (reformulated) query string. Generic Args in the
// kit; concrete here so the test reads like a real consumer.
type queryArgs struct{ Query string }

// item is the example consumer's result type R (e.g. a retrieved document). Generic R in the kit.
type item struct{ ID string }

// fakeTool records each call and returns a queued batch of items (or an error). It is the I/O the loop
// runs — the agent never calls it.
type fakeTool struct {
	name    string
	batches [][]item // batches[i] returned on the i-th Call
	err     error
	calls   []queryArgs
}

func (f *fakeTool) Name() string { return f.name }

func (f *fakeTool) Call(_ context.Context, args queryArgs) ([]item, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	i := len(f.calls) - 1
	if i < len(f.batches) {
		return f.batches[i], nil
	}
	return nil, nil
}

// fakeToolCaller fulfils a ToolStep[queryArgs] each structured call from a queued script. The loop's
// executor is a structured call returning a ToolStep, so we drive it through the StructuredCaller seam.
type fakeToolCaller struct {
	steps []ToolStep[queryArgs] // one per round
	round int
	reqs  []StructuredRequest
}

func (c *fakeToolCaller) CompleteJSON(_ context.Context, req StructuredRequest) (string, error) {
	c.reqs = append(c.reqs, req)
	step := ToolStep[queryArgs]{Done: true} // default: stop if the script is exhausted
	if c.round < len(c.steps) {
		step = c.steps[c.round]
	}
	c.round++
	return toolStepJSON(step), nil
}

// toolStepJSON serializes a ToolStep the way GenerateStructured[ToolStep] will parse it back.
func toolStepJSON(s ToolStep[queryArgs]) string {
	if s.Done {
		return fmt.Sprintf(`{"done":true,"rationale":%q}`, s.Rationale)
	}
	return fmt.Sprintf(`{"done":false,"call":{"query":%q},"rationale":%q}`, s.Call.Query, s.Rationale)
}

// fakeToolAgent renders a trivial prompt + role; it does not need rc/info for the test (the caller script
// decides the ToolStep), but Render is exercised so the seam is covered.
type fakeToolAgent struct{ renders int }

func (a *fakeToolAgent) Render(_ struct{}, info ToolRoundInfo[item]) (string, string) {
	a.renders++
	return "sys", fmt.Sprintf("round %d of %d; prior batches %d", info.Round, info.MaxRounds, len(info.PriorResults))
}

func (a *fakeToolAgent) Role() RoleSpec {
	return RoleSpec{Model: "test/model", PromptType: "tool-loop", MaxTokens: 1024}
}

func ptr(q queryArgs) *queryArgs { return &q }

// --- tests -------------------------------------------------------------------------------------------

// TestRunToolLoop_SingleRoundDone: the executor runs one tool call then says Done — the loop returns the
// retrieved items, OutcomeApplied, 1 round.
func TestRunToolLoop_SingleRoundDone(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "swim levels"})},
		{Done: true, Rationale: "enough"},
	}}
	tool := &fakeTool{name: "search", batches: [][]item{{{ID: "a"}, {ID: "b"}}}}
	agent := &fakeToolAgent{}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, agent, tool, struct{}{}, LoopConfig{MaxRounds: 3}, nil, nil,
	)

	if res.Outcome != OutcomeApplied {
		t.Fatalf("expected OutcomeApplied, got %v", res.Outcome)
	}
	if len(res.Items) != 2 || res.Items[0].ID != "a" || res.Items[1].ID != "b" {
		t.Fatalf("expected the retrieved items carried out, got %+v", res.Items)
	}
	if res.Rounds != 2 { // round 1 = tool call; round 2 = Done
		t.Fatalf("expected 2 rounds (call + done), got %d", res.Rounds)
	}
	if len(tool.calls) != 1 || tool.calls[0].Query != "swim levels" {
		t.Fatalf("expected one tool call with the query, got %+v", tool.calls)
	}
}

// TestRunToolLoop_MultiRoundReformulate: the executor reformulates across rounds (sees PriorResults), then
// stops. The loop accumulates results across rounds and the executor sees prior batches.
func TestRunToolLoop_MultiRoundReformulate(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "q1"})},
		{Done: false, Call: ptr(queryArgs{Query: "q2"})},
		{Done: true},
	}}
	tool := &fakeTool{name: "search", batches: [][]item{{{ID: "a"}}, {{ID: "b"}}}}
	agent := &fakeToolAgent{}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, agent, tool, struct{}{}, LoopConfig{MaxRounds: 5}, nil, nil,
	)

	if res.Outcome != OutcomeApplied {
		t.Fatalf("expected applied, got %v", res.Outcome)
	}
	if len(res.Items) != 2 {
		t.Fatalf("expected results accumulated across rounds, got %+v", res.Items)
	}
	if len(tool.calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tool.calls))
	}
	// The executor must have rendered with prior results in hand on the reflect rounds.
	if agent.renders < 3 {
		t.Fatalf("expected the executor to render each round, got %d", agent.renders)
	}
}

// TestRunToolLoop_CeilingStop: the executor never says Done — the round ceiling stops it and the loop
// returns what it gathered. Stopped reason names the ceiling.
func TestRunToolLoop_CeilingStop(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "q1"})},
		{Done: false, Call: ptr(queryArgs{Query: "q2"})},
		{Done: false, Call: ptr(queryArgs{Query: "q3"})}, // would continue, but ceiling=2
	}}
	tool := &fakeTool{name: "search", batches: [][]item{{{ID: "a"}}, {{ID: "b"}}}}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 2}, nil, nil,
	)

	if res.Rounds != 2 {
		t.Fatalf("ceiling should cap at 2 rounds, got %d", res.Rounds)
	}
	if len(tool.calls) != 2 {
		t.Fatalf("expected exactly 2 tool calls under the ceiling, got %d", len(tool.calls))
	}
	if len(res.Items) != 2 {
		t.Fatalf("expected the gathered items returned at the ceiling, got %+v", res.Items)
	}
	if res.StoppedReason == "" {
		t.Fatalf("expected a stopped reason naming the ceiling")
	}
}

// TestRunToolLoop_DoneRound1NoResults: the executor decides immediately it needs nothing → OutcomeSilent,
// no tool call, no items. (The understand-with-MaxRounds:1 degenerate case, and the "nothing material" case.)
func TestRunToolLoop_DoneRound1NoResults(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{{Done: true, Rationale: "no retrieval needed"}}}
	tool := &fakeTool{name: "search"}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 3}, nil, nil,
	)

	if res.Outcome != OutcomeSilent {
		t.Fatalf("expected OutcomeSilent when the executor retrieves nothing, got %v", res.Outcome)
	}
	if len(tool.calls) != 0 {
		t.Fatalf("expected no tool call, got %d", len(tool.calls))
	}
	if len(res.Items) != 0 {
		t.Fatalf("expected no items, got %+v", res.Items)
	}
}

// TestRunToolLoop_ThrottleSurfaces: a provider throttle on the structured call surfaces as
// OutcomeError wrapping ErrThrottled (so the caller releases the work to its queue).
func TestRunToolLoop_ThrottleSurfaces(t *testing.T) {
	caller := &errToolCaller{err: fmt.Errorf("429: %w", ErrThrottled)}
	tool := &fakeTool{name: "search"}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 3}, nil, nil,
	)

	if res.Outcome != OutcomeError {
		t.Fatalf("expected OutcomeError, got %v", res.Outcome)
	}
	if !errors.Is(res.Err, ErrThrottled) {
		t.Fatalf("expected ErrThrottled to surface, got %v", res.Err)
	}
}

// TestRunToolLoop_ToolErrorSurfaces: an error from the tool itself is terminal (OutcomeError) — the loop
// does not silently swallow a broken organ.
func TestRunToolLoop_ToolErrorSurfaces(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{{Done: false, Call: ptr(queryArgs{Query: "q"})}}}
	tool := &fakeTool{name: "search", err: errors.New("organ down")}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 3}, nil, nil,
	)

	if res.Outcome != OutcomeError {
		t.Fatalf("expected OutcomeError on a tool failure, got %v", res.Outcome)
	}
	if res.Err == nil {
		t.Fatalf("expected the tool error surfaced")
	}
}

// TestRunToolLoop_DegenerationStop: the executor repeats the SAME query twice — the degeneration guard
// stops the loop (don't burn budget on a model stuck reformulating to the same thing).
func TestRunToolLoop_DegenerationStop(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "same"})},
		{Done: false, Call: ptr(queryArgs{Query: "same"})}, // identical → degeneration
		{Done: false, Call: ptr(queryArgs{Query: "same"})},
	}}
	tool := &fakeTool{name: "search", batches: [][]item{{{ID: "a"}}, {{ID: "a"}}}}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 5}, nil, nil,
	)

	if len(tool.calls) > 2 {
		t.Fatalf("degeneration guard should stop after the repeated query, got %d calls", len(tool.calls))
	}
	if res.StoppedReason == "" {
		t.Fatalf("expected a stopped reason for degeneration")
	}
}

// TestRunToolLoop_DefaultsMaxRounds: an unset MaxRounds uses DefaultMaxRounds (a tool loop is still a
// budgeted loop; 0 must not mean "infinite").
func TestRunToolLoop_DefaultsMaxRounds(t *testing.T) {
	// An executor that never says Done; with default ceiling it must stop at DefaultMaxRounds.
	steps := make([]ToolStep[queryArgs], 10)
	for i := range steps {
		steps[i] = ToolStep[queryArgs]{Done: false, Call: ptr(queryArgs{Query: fmt.Sprintf("q%d", i)})}
	}
	caller := &fakeToolCaller{steps: steps}
	tool := &fakeTool{name: "search", batches: [][]item{{{ID: "a"}}, {{ID: "b"}}, {{ID: "c"}}}}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{}, nil, nil, // unset
	)

	if res.Rounds != DefaultMaxRounds {
		t.Fatalf("unset MaxRounds must default to %d, got %d rounds", DefaultMaxRounds, res.Rounds)
	}
}

// TestRunToolLoop_CeilingWithNoItemsIsSilent (review fix #2): the outcome must NOT depend on HOW the loop
// ended. A run that hits the round ceiling having retrieved NOTHING is Silent — same as the Done path with
// no items — not Applied. (The bug: the ceiling used to force OutcomeApplied even with 0 items, so a
// consumer branching on Outcome got inconsistent answers for the identical "got nothing" state.)
func TestRunToolLoop_CeilingWithNoItemsIsSilent(t *testing.T) {
	// The executor keeps emitting calls (never Done), but the tool returns NOTHING each time. At the ceiling
	// the loop has 0 items.
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "q1"})},
		{Done: false, Call: ptr(queryArgs{Query: "q2"})},
		{Done: false, Call: ptr(queryArgs{Query: "q3"})},
	}}
	tool := &fakeTool{name: "search"} // no batches → every Call returns nil

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 2}, nil, nil,
	)

	if res.Rounds != 2 {
		t.Fatalf("expected the ceiling at 2 rounds, got %d", res.Rounds)
	}
	if res.Outcome != OutcomeSilent {
		t.Fatalf("a ceiling run with 0 items must be Silent (consistent with Done→Silent), got %v", res.Outcome)
	}
	if res.StoppedReason != "nothing_retrieved" {
		t.Fatalf("an empty run reports nothing_retrieved regardless of how it ended, got %q", res.StoppedReason)
	}
}

// TestRunToolLoop_CeilingWithItemsIsApplied: the symmetric case — a ceiling run that DID retrieve items is
// Applied (the items are real and usable; the ceiling is a backstop, not a failure). The stopped reason
// keeps the structural marker (round_ceiling) so ops can still see HOW it ended.
func TestRunToolLoop_CeilingWithItemsIsApplied(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "q1"})},
		{Done: false, Call: ptr(queryArgs{Query: "q2"})},
	}}
	tool := &fakeTool{name: "search", batches: [][]item{{{ID: "a"}}, {{ID: "b"}}}}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 2}, nil, nil,
	)

	if res.Outcome != OutcomeApplied {
		t.Fatalf("a ceiling run WITH items is Applied, got %v", res.Outcome)
	}
	if res.StoppedReason != "round_ceiling" {
		t.Fatalf("a non-empty ceiling run keeps the structural reason, got %q", res.StoppedReason)
	}
}

// TestRunToolLoop_SameFuncOverride (review fix #1): a caller-supplied SameFunc decides degeneration, not the
// comparator-free default. Here two calls differ by a field the default %+v WOULD distinguish, but the
// caller's SameFunc treats them as the same (e.g. it only compares a normalized query) → the loop stops on
// the second call. Proves the override is honored.
func TestRunToolLoop_SameFuncOverride(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "swim"})},
		{Done: false, Call: ptr(queryArgs{Query: "SWIM"})}, // different to %+v, "same" to the custom func
		{Done: false, Call: ptr(queryArgs{Query: "more"})},
	}}
	tool := &fakeTool{name: "search", batches: [][]item{{{ID: "a"}}, {{ID: "b"}}}}

	// Case-insensitive degeneration: "swim" and "SWIM" are the same query.
	same := func(a, b queryArgs) bool {
		return strings.EqualFold(a.Query, b.Query)
	}

	res := RunToolLoop[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 5}, same, nil,
	)

	if res.Outcome != OutcomeStalemate {
		t.Fatalf("the custom SameFunc should detect degeneration on the 2nd (case-variant) call, got %v", res.Outcome)
	}
	if len(tool.calls) != 1 {
		t.Fatalf("the loop should stop after the first call's tool run (2nd call is degenerate), got %d calls", len(tool.calls))
	}
}

// errToolCaller always errors (for the throttle test).
type errToolCaller struct{ err error }

func (c *errToolCaller) CompleteJSON(_ context.Context, _ StructuredRequest) (string, error) {
	return "", c.err
}
