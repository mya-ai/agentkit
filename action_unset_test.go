package agentkit

import (
	"context"
	"strings"
	"testing"
)

// unsetState is a minimal state type for exercising ChangeUnset handling.
type unsetState struct{ value string }

// TestChangeUnset_StringIsNotAlwaysMints guards the diagnostic, not the behaviour.
//
// ChangeUnset had no String() case and fell through to the "always_mints" default. That made
// Validate emit a SELF-CONTRADICTORY second error for a forgotten Category:
//
//	"is category always_mints, which compares against current state, but no state reader was supplied"
//
// — always_mints is precisely the category that does NOT compare against state. A developer reading
// that goes looking for a missing reader when the real fix is to declare a Category.
func TestChangeUnset_StringIsNotAlwaysMints(t *testing.T) {
	got := ChangeUnset.String()
	if got == ChangeAlwaysMints.String() {
		t.Errorf("ChangeUnset.String() = %q, the same as ChangeAlwaysMints — a forgotten Category "+
			"must not be reported as a real, valid category", got)
	}
	if !strings.Contains(got, "unset") {
		t.Errorf("ChangeUnset.String() = %q; want something naming it as unset/undeclared so the "+
			"error message points at the missing declaration", got)
	}
}

// TestChangeUnset_ValidateReportsOnlyTheMissingCategory pins that a forgotten Category produces ONE
// actionable error, not that error plus a spurious "no state reader was supplied".
//
// needsStateReader() defaulted ChangeUnset to true, so Validate demanded a reader for a category the
// developer had not declared yet — sending them to fix the wrong thing.
func TestChangeUnset_ValidateReportsOnlyTheMissingCategory(t *testing.T) {
	set := NewActionSet[unsetState]().Add(Action{
		Opcode: "forgot_category",
		Tier:   TierReversible,
		Input:  "value",
		// Category deliberately omitted — this is the developer mistake under test.
	}, nil)

	err := set.Validate()
	if err == nil {
		t.Fatal("expected Validate to reject an action with no Category")
	}
	msg := err.Error()

	if !strings.Contains(msg, "declares no Category") {
		t.Errorf("Validate must say the Category is missing; got: %v", err)
	}
	if strings.Contains(msg, "no state reader was supplied") {
		t.Errorf("Validate emitted a spurious state-reader error for an action whose Category was "+
			"never declared — it points the developer at the wrong fix. Got: %v", err)
	}
}

// TestChangeUnset_WillChangeDoesNotSilentlyAlwaysMint is the load-bearing one.
//
// ChangeUnset routed past WillChange's early return into `default: return true, nil` — so an
// unvalidated, hand-built set with a forgotten Category silently always-minted. That is EXACTLY the
// defect ChangeUnset was introduced to close: the old zero value was ChangeAlwaysMints, and a
// forgotten field re-proposed the same ask forever.
//
// Production consumers go through Bind (which calls Validate), so they are safe — but the kit must
// not quietly restore the old behaviour for anyone who builds a set directly.
func TestChangeUnset_WillChangeDoesNotSilentlyAlwaysMint(t *testing.T) {
	set := NewActionSet[unsetState]().Add(Action{
		Opcode: "forgot_category",
		Tier:   TierReversible,
		Input:  "value",
	}, func(_ context.Context, s *unsetState, _ Action) (string, error) {
		return s.value, nil
	})

	// State already equals the proposed value: under any real category this is a fixpoint and must
	// NOT mint. Under the old ChangeUnset fall-through it minted anyway.
	state := &unsetState{value: "same"}
	act := Action{Opcode: "forgot_category", Args: map[string]string{"value": "same"}}

	_, err := set.WillChange(context.Background(), state, act)
	if err == nil {
		t.Fatal("WillChange silently accepted an action with an undeclared Category — an unvalidated " +
			"set must ERROR, not fall through to always-mint (the exact defect ChangeUnset closes)")
	}
	if !strings.Contains(err.Error(), "Category") {
		t.Errorf("the error should name the undeclared Category as the cause; got: %v", err)
	}
}
