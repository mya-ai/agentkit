package agentkit

import (
	"errors"
	"fmt"
	"testing"
)

// ⭐ WHY THIS SENTINEL EARNS ITS PLACE. A runner with nothing to do can otherwise
// only say so by returning nil, which every caller reads as SUCCESS — so a real
// outage (every unit skipped because a dependency was unwired) reports as healthy
// throughput. These tests pin the three properties a caller depends on.
func TestSkip_IsNotSuccessAndCarriesItsReason(t *testing.T) {
	err := Skip("deps_unwired")

	if err == nil {
		t.Fatal("Skip must return a non-nil error — a nil would be counted as processed, " +
			"which is the exact mis-count this sentinel exists to prevent")
	}
	reason, ok := IsSkip(err)
	if !ok {
		t.Fatal("IsSkip must recognize a Skip")
	}
	if reason != "deps_unwired" {
		t.Errorf("reason = %q, want %q", reason, "deps_unwired")
	}
	if err.Error() != "agentkit: skip (deps_unwired)" {
		t.Errorf("Error() = %q — the reason must be visible in a log line", err.Error())
	}
}

func TestSkip_OrdinaryErrorsAreNotSkips(t *testing.T) {
	// The distinction is the whole point: a skip acks, a failure redelivers.
	for _, err := range []error{
		errors.New("connection reset"),
		fmt.Errorf("throttled: %w", ErrThrottled),
		nil,
	} {
		if _, ok := IsSkip(err); ok {
			t.Errorf("IsSkip(%v) = true — a genuine failure must NOT be acked as a skip", err)
		}
	}
}

// ⚠️ A skip must not be confusable with a throttle. They demand opposite handling:
// a throttle RELEASES the work so it comes back, a skip ACKS it so it does not.
func TestSkip_IsDistinctFromThrottle(t *testing.T) {
	skip := Skip("not_eligible")
	if errors.Is(skip, ErrThrottled) {
		t.Error("a Skip must never satisfy errors.Is(err, ErrThrottled) — " +
			"the caller would release work that was deliberately skipped, and it would loop")
	}
	throttle := fmt.Errorf("429: %w", ErrThrottled)
	if _, ok := IsSkip(throttle); ok {
		t.Error("a throttle must never read as a Skip — the work would be acked and silently dropped")
	}
}

// ⚠️ A WRAPPED Skip IS NOT A Skip, AND THAT IS A DECISION, NOT AN ACCIDENT.
// `fmt.Errorf("...: %w", err)` is the reflex in every Go codebase, so this narrowing
// is invisible at the call site — which is exactly why it needs a guard rather than
// resting on the implementation.
//
// The rationale: a skip is a TERMINAL decision by the runner about work it examined
// and chose not to do. A decision that survives three layers of wrapping is no
// longer terminal. The reason string is where context belongs — Skip("deps_unwired")
// — not an enclosing error.
//
// ⭐ The failure direction is the safe one, which is what earns the narrowing: a
// missed skip becomes a REDELIVERY (loud, self-correcting), never a silent ack that
// drops the work. That is the opposite of the mis-count Skip exists to prevent.
func TestSkip_DoesNotSurviveWrapping(t *testing.T) {
	wrapped := fmt.Errorf("unit u1: %w", Skip("deps_unwired"))

	if _, ok := IsSkip(wrapped); ok {
		t.Error("a wrapped Skip reads as a skip — if this is now intended, the doc on IsSkip " +
			"must change too: it tells callers to return Skip bare BECAUSE wrapping forfeits it")
	}
	// And the safe direction holds: it is still an ordinary error, so a caller
	// redelivers rather than silently acking.
	if wrapped == nil {
		t.Fatal("a wrapped Skip must remain a non-nil error")
	}
}
