package agentkit

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mutatingTool is a tool ABOVE the auto-ceiling (it has a real side effect). The structural guarantee:
// RunToolLoop must NEVER call it live — its intended call is captured as a proposed action instead.
type mutatingTool struct {
	tier     Tier
	executed []queryArgs // records any LIVE execution — must stay EMPTY (the bug we guard against)
}

func (m *mutatingTool) Name() string { return "send_email" }
func (m *mutatingTool) Tier() Tier   { return m.tier }
func (m *mutatingTool) Call(_ context.Context, args queryArgs) ([]item, error) {
	m.executed = append(m.executed, args) // if this ever runs in the live loop, the test fails
	return []item{{ID: "sent"}}, nil
}

// readTool is at/below the auto-ceiling — the loop runs it live.
type readTool struct{ batches [][]item }

func (readTool) Name() string { return "search" }

// TierReadOnlyExplicit, not TierReadOnly: the latter is the zero value and is now REJECTED
// on this path, because a forgotten Tier() body is indistinguishable from a deliberate one.
func (readTool) Tier() Tier { return TierReadOnlyExplicit }
func (t readTool) Call(_ context.Context, _ queryArgs) ([]item, error) {
	if len(t.batches) == 0 {
		return nil, nil
	}
	return t.batches[0], nil
}

// TestMaxRungFor mirrors the safety floor: read/reversible may auto-run; external/irreversible are pinned
// to ask (propose only).
func TestMaxRungFor(t *testing.T) {
	cases := []struct {
		tier Tier
		want AutonomyRung
	}{
		{TierReadOnly, RungAuto},
		{TierReversible, RungAuto},
		{TierExternalReversible, RungAsk},
		{TierIrreversible, RungAsk},
	}
	for _, c := range cases {
		if got := MaxRungFor(c.tier); got != c.want {
			t.Fatalf("MaxRungFor(%v) = %v, want %v", c.tier, got, c.want)
		}
	}
}

// TestRunToolLoop_RunsAutoTierLive: a read-tier tool the executor calls IS executed live and its results
// flow back (the normal path, now via the tiered dispatch).
func TestRunToolLoop_RunsAutoTierLive(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "q"})},
		{Done: true},
	}}
	tool := readTool{batches: [][]item{{{ID: "a"}}}}

	res := RunToolLoopTiered[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 3}, nil, nil,
	)
	if res.Outcome != OutcomeApplied {
		t.Fatalf("expected applied, got %v", res.Outcome)
	}
	if len(res.Items) != 1 || res.Items[0].ID != "a" {
		t.Fatalf("expected the read results live, got %+v", res.Items)
	}
}

// TestRunToolLoop_NeverExecutesIrreversibleLive: the STRUCTURAL guarantee. The executor tries to call a
// mutating (irreversible) tool; the loop must NOT execute it — it captures the intended call as a proposed
// action and stops. mutatingTool.executed must stay EMPTY.
func TestRunToolLoop_NeverExecutesIrreversibleLive(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "email the team"})},
	}}
	tool := &mutatingTool{tier: TierIrreversible}

	res := RunToolLoopTiered[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 3}, nil, nil,
	)

	if len(tool.executed) != 0 {
		t.Fatalf("SAFETY VIOLATION: the loop executed an irreversible tool live: %+v", tool.executed)
	}
	if len(res.Proposed) != 1 {
		t.Fatalf("expected the intended call captured as a proposal, got %d", len(res.Proposed))
	}
	if res.Proposed[0].ToolName != "send_email" {
		t.Fatalf("proposed action should name the tool, got %q", res.Proposed[0].ToolName)
	}
	if res.Outcome != OutcomeApplied {
		t.Fatalf("a captured proposal is a usable result (Applied), got %v", res.Outcome)
	}
	if res.StoppedReason != "proposed_irreversible" {
		t.Fatalf("stopped reason should name the propose path, got %q", res.StoppedReason)
	}
}

// TestRunToolLoop_ExternalReversibleAlsoProposed: external-reversible (send/booking) is also above the
// auto-ceiling → proposed, not executed.
func TestRunToolLoop_ExternalReversibleAlsoProposed(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{{Done: false, Call: ptr(queryArgs{Query: "book it"})}}}
	tool := &mutatingTool{tier: TierExternalReversible}

	res := RunToolLoopTiered[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 3}, nil, nil,
	)
	if len(tool.executed) != 0 {
		t.Fatalf("external-reversible must not auto-execute, got %+v", tool.executed)
	}
	if len(res.Proposed) != 1 {
		t.Fatalf("expected a proposed action, got %d", len(res.Proposed))
	}
}

// TestRunToolLoop_ReversibleRunsLive: reversible-tier (has a compensating undo) is at the auto-ceiling →
// runs live, like read-only.
func TestRunToolLoop_ReversibleRunsLive(t *testing.T) {
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "tweak"})},
		{Done: true},
	}}
	tool := &mutatingTool{tier: TierReversible}

	res := RunToolLoopTiered[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{}, LoopConfig{MaxRounds: 3}, nil, nil,
	)
	if len(tool.executed) != 1 {
		t.Fatalf("reversible should run live (it has an undo), executed=%d", len(tool.executed))
	}
	if res.Outcome != OutcomeApplied {
		t.Fatalf("expected applied, got %v", res.Outcome)
	}
}

func TestTier_String(t *testing.T) {
	for _, c := range []struct {
		tier Tier
		want string
	}{
		{TierReadOnly, "read_only"}, {TierReversible, "reversible"},
		{TierExternalReversible, "external_reversible"}, {TierIrreversible, "irreversible"},
	} {
		if got := fmt.Sprint(c.tier); got != c.want {
			t.Fatalf("Tier(%d).String() = %q, want %q", c.tier, got, c.want)
		}
	}
}

// forgetfulTool is the mistake a newcomer actually makes: a mutating tool whose
// author never stated a Tier, so Tier() returns the zero value.
type forgetfulTool struct{ executed []string }

func (t *forgetfulTool) Name() string { return "delete_account" }
func (t *forgetfulTool) Tier() Tier   { var zero Tier; return zero } // forgotten
func (t *forgetfulTool) Call(_ context.Context, a queryArgs) ([]item, error) {
	t.executed = append(t.executed, a.Query)
	return []item{{ID: "deleted"}}, nil
}

// ⛔ A FORGOTTEN Tier EXECUTED A DESTRUCTIVE CALL LIVE, AND THIS TEST DID NOT EXIST.
//
// Tier's zero value is TierReadOnly, which auto-runs. The Capability path has
// rejected that since it shipped (ToolSet.Validate, GateProgram), but the
// TieredTool path had no such check — so a tool whose author never wrote a Tier()
// body reached tool.Call and did the thing.
//
// ⚠️ It was found by a comment audit, not by a test: the demo told readers "a
// forgotten Tier is REJECTED, not silently treated as safe" while running on the
// one path where it was not. The comment described the guarantee the kit MEANT to
// make. Nothing checked whether it made it.
//
// ⭐ The asymmetry is the lesson. Two gating paths, one guarded and one not, and
// the unguarded one was the newer. A safety invariant needs a test per path, not
// per invariant.
func TestRunToolLoopTiered_ForgottenTierNeverRunsLive(t *testing.T) {
	tool := &forgetfulTool{}
	caller := &fakeToolCaller{steps: []ToolStep[queryArgs]{
		{Done: false, Call: ptr(queryArgs{Query: "acct-42"})},
		{Done: true},
	}}

	res := RunToolLoopTiered[struct{}, queryArgs, item](
		context.Background(), caller, &fakeToolAgent{}, tool, struct{}{},
		LoopConfig{MaxRounds: 2}, nil, nil,
	)

	if len(tool.executed) > 0 {
		t.Fatalf("⛔ SAFETY VIOLATION: a tool that declares no Tier executed live: %v", tool.executed)
	}
	if res.Outcome != OutcomeError {
		t.Errorf("outcome = %v, want OutcomeError — a forgotten Tier is a wiring bug and must be "+
			"LOUD, not silently downgraded to a proposal a human then has to notice", res.Outcome)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "declares no Tier") {
		t.Errorf("the error must name the cause; got %v", res.Err)
	}
	// The error must say what to write instead — a reader hitting this needs the fix, not the diagnosis.
	if res.Err != nil && !strings.Contains(res.Err.Error(), "TierReadOnlyExplicit") {
		t.Errorf("the error must name the explicit spelling; got %v", res.Err)
	}
}
