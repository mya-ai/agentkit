package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// scriptedCaller is a fake StructuredCaller that returns a queued sequence of JSON responses (one per
// CompleteJSON call), or a queued error. A response of "" with
// an err returns that err (to exercise the throttle/transport path).
type scriptedCaller struct {
	responses []string
	errs      []error
	calls     int
}

func (s *scriptedCaller) CompleteJSON(_ context.Context, _ StructuredRequest) (string, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return "", s.errs[i]
	}
	if i >= len(s.responses) {
		return "", fmt.Errorf("scriptedCaller: no response queued for call %d", i)
	}
	return s.responses[i], nil
}

// testVerdict is a minimal Material result the executor returns.
type testVerdict struct {
	Material bool   `json:"material"`
	Plan     string `json:"plan,omitempty"`
}

func (v testVerdict) IsMaterial() bool { return v.Material }

// fixedExec is a no-op OnceAgent — the loop logic doesn't depend on prompt CONTENT (the renderer is
// tested separately). It records the feedback it last saw so a test can assert revision feedback flows.
type fixedExec struct {
	lastFeedback []string
	lastInfo     RoundInfo
	seenInfos    []RoundInfo
}

func (e *fixedExec) Render(_ struct{}, info RoundInfo) (string, string) {
	e.lastFeedback = info.Feedback
	e.lastInfo = info
	e.seenInfos = append(e.seenInfos, info)
	return "sys", "dyn"
}
func (e *fixedExec) Role() RoleSpec { return RoleSpec{Model: "test/model", PromptType: "test"} }

// scriptedJudge approves on the Nth call (1-based); before that it returns not-ok + fixed feedback.
type scriptedJudge struct {
	approveOnCall int
	calls         int
	verifyErr     error
}

func (j *scriptedJudge) Verify(_ context.Context, _ testVerdict) (bool, []string, error) {
	j.calls++
	if j.verifyErr != nil {
		return false, nil, j.verifyErr
	}
	if j.calls >= j.approveOnCall {
		return true, nil, nil
	}
	return false, []string{"address X"}, nil
}

func sameVerdict(a, b testVerdict) bool { return a.Plan == b.Plan }

func mustJSON(v testVerdict) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// --- RunOnce ---

func TestRunOnce_ReturnsResult(t *testing.T) {
	caller := &scriptedCaller{responses: []string{mustJSON(testVerdict{Material: true, Plan: "do it"})}}
	v, err := RunOnce[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, struct{}{}, nil, nil)
	if err != nil {
		t.Fatalf("RunOnce err: %v", err)
	}
	if !v.Material || v.Plan != "do it" {
		t.Fatalf("unexpected result: %+v", v)
	}
	if caller.calls != 1 {
		t.Fatalf("expected 1 LLM call, got %d", caller.calls)
	}
}

func TestRunOnce_ThrottleSurfaces(t *testing.T) {
	caller := &scriptedCaller{errs: []error{fmt.Errorf("429: %w", ErrThrottled)}}
	_, err := RunOnce[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, struct{}{}, nil, nil)
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("expected ErrThrottled to surface, got %v", err)
	}
}

// --- Pair ---

func TestPair_ApprovedFirstRound(t *testing.T) {
	caller := &scriptedCaller{responses: []string{mustJSON(testVerdict{Material: true, Plan: "p1"})}}
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, &scriptedJudge{approveOnCall: 1}, sameVerdict, struct{}{}, LoopConfig{MaxRounds: 2}, nil)
	if res.Outcome != OutcomeApplied {
		t.Fatalf("expected applied, got %s (%+v)", res.Outcome, res)
	}
	if res.Rounds != 1 || res.Result.Plan != "p1" {
		t.Fatalf("unexpected: rounds=%d result=%+v", res.Rounds, res.Result)
	}
	if caller.calls != 1 {
		t.Fatalf("expected 1 executor call (no revision), got %d", caller.calls)
	}
}

func TestPair_NonMaterialIsSilent(t *testing.T) {
	caller := &scriptedCaller{responses: []string{mustJSON(testVerdict{Material: false})}}
	judge := &scriptedJudge{approveOnCall: 1}
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, judge, sameVerdict, struct{}{}, LoopConfig{MaxRounds: 2}, nil)
	if res.Outcome != OutcomeSilent {
		t.Fatalf("expected silent, got %s", res.Outcome)
	}
	if judge.calls != 0 {
		t.Fatalf("judge must NOT be called on a non-material result, got %d calls", judge.calls)
	}
}

func TestPair_RevisionThenApprove(t *testing.T) {
	// Round 1: executor p1, judge rejects with feedback. Round 2: executor p2, judge approves.
	caller := &scriptedCaller{responses: []string{
		mustJSON(testVerdict{Material: true, Plan: "p1"}),
		mustJSON(testVerdict{Material: true, Plan: "p2"}),
	}}
	exec := &fixedExec{}
	res := Pair[struct{}, testVerdict](context.Background(), caller, exec, &scriptedJudge{approveOnCall: 2}, sameVerdict, struct{}{}, LoopConfig{MaxRounds: 2}, nil)
	if res.Outcome != OutcomeApplied || res.Rounds != 2 || res.Result.Plan != "p2" {
		t.Fatalf("expected applied@2 p2, got %s rounds=%d result=%+v", res.Outcome, res.Rounds, res.Result)
	}
	if len(exec.lastFeedback) == 0 || exec.lastFeedback[0] != "address X" {
		t.Fatalf("revision feedback did not flow to executor: %v", exec.lastFeedback)
	}
}

func TestPair_DegenerationShortCircuits(t *testing.T) {
	// Executor resubmits the SAME plan despite feedback → degeneration guard trips at round 2 BEFORE a
	// second judge call.
	caller := &scriptedCaller{responses: []string{
		mustJSON(testVerdict{Material: true, Plan: "same"}),
		mustJSON(testVerdict{Material: true, Plan: "same"}),
	}}
	judge := &scriptedJudge{approveOnCall: 99} // never approves
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, judge, sameVerdict, struct{}{}, LoopConfig{MaxRounds: 5}, nil)
	if res.Outcome != OutcomeStalemate {
		t.Fatalf("expected stalemate via degeneration, got %s", res.Outcome)
	}
	if res.Rounds != 2 {
		t.Fatalf("degeneration should short-circuit at round 2, got %d", res.Rounds)
	}
	if judge.calls != 1 {
		t.Fatalf("judge should be called once (round 1 only); round 2 short-circuits before it, got %d", judge.calls)
	}
}

func TestPair_BudgetExhaustedEndsAtExecutor(t *testing.T) {
	// THE CORE INVARIANT: the loop is executor → (judge → executor)* and ALWAYS ends at the executor —
	// never at the judge. Here the executor changes the plan each round (no degeneration) and the judge
	// never approves. Old behavior: budget exhausted → OutcomeStalemate carrying the JUDGE's feedback (the
	// judge had the last word, its critique leaked out). New behavior: the final executor turn (round 2) IS
	// the terminal result → OutcomeApplied with the executor's output. The judge only ever gated.
	caller := &scriptedCaller{responses: []string{
		mustJSON(testVerdict{Material: true, Plan: "p1"}),
		mustJSON(testVerdict{Material: true, Plan: "p2"}),
	}}
	judge := &scriptedJudge{approveOnCall: 99} // never approves
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, judge, sameVerdict, struct{}{}, LoopConfig{MaxRounds: 2}, nil)
	if res.Outcome != OutcomeApplied || res.Rounds != 2 {
		t.Fatalf("budget exhausted must END AT THE EXECUTOR (applied@2), got %s rounds=%d", res.Outcome, res.Rounds)
	}
	if res.Result.Plan != "p2" {
		t.Fatalf("the terminal result must be the FINAL EXECUTOR turn's output (p2), got %+v", res.Result)
	}
	// There is deliberately NO field for judge text on Result — that is what makes the leaked-critique
	// bug unrepresentable rather than merely discouraged. The assertion above (Result.Plan == "p2") IS
	// the invariant: what ships is the executor's final output.
	// The judge ran only on round 1 (between executor turns); there is NO judge pass after the final
	// executor turn — a rejection there could only be surfaced (forbidden) or ignored (pointless).
	if judge.calls != 1 {
		t.Fatalf("judge runs only BETWEEN executor turns (once, after round 1), never after the final turn; got %d", judge.calls)
	}
}

// TestPair_AlwaysRejectingJudgeCannotLoopForever is the explicit "mock a judge that ALWAYS rejects → does
// the loop STOP?" guard. A judge with no power to terminate could, in a naive loop, run forever; here we
// prove the budget BOUNDS it: with MaxRounds=N and an always-rejecting judge, the loop runs exactly N
// executor turns and N-1 judge passes (judge only BETWEEN turns), then stops at the final executor turn —
// never the judge, never unbounded. This is the termination guarantee: consensus OR budget, full stop.
func TestPair_AlwaysRejectingJudgeCannotLoopForever(t *testing.T) {
	const n = 4
	// Enough executor responses for n rounds (the executor varies each time so the degeneration guard never
	// fires — we want to prove BUDGET is what stops it, not degeneration).
	resps := make([]string, 0, n)
	for i := 0; i < n; i++ {
		resps = append(resps, mustJSON(testVerdict{Material: true, Plan: fmt.Sprintf("p%d", i)}))
	}
	caller := &scriptedCaller{responses: resps}
	judge := &scriptedJudge{approveOnCall: 9999} // never approves, ever
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, judge, sameVerdict, struct{}{}, LoopConfig{MaxRounds: n}, nil)

	if res.Outcome != OutcomeApplied {
		t.Fatalf("an always-rejecting judge must NOT prevent termination; the loop stops at the executor (applied), got %s", res.Outcome)
	}
	if res.Rounds != n {
		t.Fatalf("the loop must run exactly the budget (%d executor turns) then stop, got %d", n, res.Rounds)
	}
	// Judge runs only BETWEEN executor turns → n-1 times for n rounds (never after the final turn).
	if judge.calls != n-1 {
		t.Fatalf("judge runs BETWEEN turns only (%d times for %d rounds), never after the last; got %d", n-1, n, judge.calls)
	}
	if res.Result.Plan != fmt.Sprintf("p%d", n-1) {
		t.Fatalf("the terminal result is the FINAL executor turn's output; got %+v", res.Result)
	}
}

func TestPair_ExecutorThrottleIsError(t *testing.T) {
	caller := &scriptedCaller{errs: []error{fmt.Errorf("429: %w", ErrThrottled)}}
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, &scriptedJudge{approveOnCall: 1}, sameVerdict, struct{}{}, LoopConfig{MaxRounds: 2}, nil)
	if res.Outcome != OutcomeError || !errors.Is(res.Err, ErrThrottled) {
		t.Fatalf("expected error wrapping ErrThrottled, got %s / %v", res.Outcome, res.Err)
	}
}

func TestPair_JudgeErrorIsError(t *testing.T) {
	caller := &scriptedCaller{responses: []string{mustJSON(testVerdict{Material: true, Plan: "p1"})}}
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, &scriptedJudge{verifyErr: errors.New("judge boom")}, sameVerdict, struct{}{}, LoopConfig{MaxRounds: 2}, nil)
	if res.Outcome != OutcomeError {
		t.Fatalf("expected error from judge, got %s", res.Outcome)
	}
}

// TestPair_RoundInfoSelfPacing is the self-pacing regression: the executor's Render receives the round/budget so it
// can self-pace. Across a 2-round run it must see Round 1 (not final) then Round 2 (final), with MaxRounds=2.
func TestPair_RoundInfoSelfPacing(t *testing.T) {
	caller := &scriptedCaller{responses: []string{
		mustJSON(testVerdict{Material: true, Plan: "p1"}),
		mustJSON(testVerdict{Material: true, Plan: "p2"}),
	}}
	exec := &fixedExec{}
	Pair[struct{}, testVerdict](context.Background(), caller, exec, &scriptedJudge{approveOnCall: 2}, sameVerdict, struct{}{}, LoopConfig{MaxRounds: 2}, nil)
	if len(exec.seenInfos) != 2 {
		t.Fatalf("expected 2 executor passes, got %d", len(exec.seenInfos))
	}
	if exec.seenInfos[0].Round != 1 || exec.seenInfos[0].MaxRounds != 2 || exec.seenInfos[0].IsFinalRound() {
		t.Fatalf("round 1 info wrong: %+v (IsFinalRound=%v)", exec.seenInfos[0], exec.seenInfos[0].IsFinalRound())
	}
	if exec.seenInfos[1].Round != 2 || !exec.seenInfos[1].IsFinalRound() {
		t.Fatalf("round 2 must be the final round: %+v (IsFinalRound=%v)", exec.seenInfos[1], exec.seenInfos[1].IsFinalRound())
	}
	// Round 2 must carry the prior round's judge feedback (revision is feedback-driven).
	if len(exec.seenInfos[1].Feedback) == 0 {
		t.Fatalf("round 2 must carry the judge feedback, got none")
	}
}

// TestRunOnce_FinalRoundInfo: a standalone RunOnce is a single final round (no revision possible).
func TestRunOnce_FinalRoundInfo(t *testing.T) {
	caller := &scriptedCaller{responses: []string{mustJSON(testVerdict{Material: true, Plan: "x"})}}
	exec := &fixedExec{}
	if _, err := RunOnce[struct{}, testVerdict](context.Background(), caller, exec, struct{}{}, nil, nil); err != nil {
		t.Fatalf("RunOnce err: %v", err)
	}
	if !exec.lastInfo.IsFinalRound() || exec.lastInfo.MaxRounds != 1 {
		t.Fatalf("standalone RunOnce must be the final round (1 of 1), got %+v", exec.lastInfo)
	}
}

// TestPair_UnsetMaxRoundsDefaultsToTwo is the regression for the unset-MaxRounds default: an unset LoopConfig{}
// must default to DefaultMaxRounds (2), NOT 1 — a 1-round Pair can never revise (judge rejects round 1 →
// immediate stalemate). Here round 1 is rejected, round 2 is approved; with the old `< 1 → 1` default this
// would have stalemated at round 1.
func TestPair_UnsetMaxRoundsDefaultsToTwo(t *testing.T) {
	caller := &scriptedCaller{responses: []string{
		mustJSON(testVerdict{Material: true, Plan: "p1"}),
		mustJSON(testVerdict{Material: true, Plan: "p2"}),
	}}
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, &scriptedJudge{approveOnCall: 2}, sameVerdict, struct{}{}, LoopConfig{}, nil)
	if res.Outcome != OutcomeApplied || res.Rounds != 2 {
		t.Fatalf("unset LoopConfig must allow a 2nd round (default 2), got %s rounds=%d", res.Outcome, res.Rounds)
	}
}

func TestPair_NilSameDisablesDegenerationGuard(t *testing.T) {
	// With same=nil, identical resubmissions don't short-circuit on the degeneration guard; the loop runs to
	// the budget and (per the corrected model) ends at the final executor turn → Applied@2, NOT a stalemate.
	// (Purpose preserved: same=nil disables the guard so the loop doesn't stop early; only the terminal
	// classification changed from the old judge-terminal stalemate to the executor-terminal applied.)
	caller := &scriptedCaller{responses: []string{
		mustJSON(testVerdict{Material: true, Plan: "same"}),
		mustJSON(testVerdict{Material: true, Plan: "same"}),
	}}
	res := Pair[struct{}, testVerdict](context.Background(), caller, &fixedExec{}, &scriptedJudge{approveOnCall: 99}, nil, struct{}{}, LoopConfig{MaxRounds: 2}, nil)
	if res.Outcome != OutcomeApplied || res.Rounds != 2 {
		t.Fatalf("same=nil must run to budget and END AT THE EXECUTOR (applied@2), got %s rounds=%d", res.Outcome, res.Rounds)
	}
}
