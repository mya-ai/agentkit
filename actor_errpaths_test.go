package agentkit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// faultyStore wraps InMemoryActorStore and injects an error on a chosen op — to exercise the actor loop's
// store-error branches (each must yield ActorError, and the lease must still be released — no wedge).
type faultyStore struct {
	*InMemoryActorStore
	failAcquire bool
	failDeliver bool
	failDrain   bool
	failEmpty   bool
}

func (f *faultyStore) Acquire(ctx context.Context, k UnitKey, ttl time.Duration) (bool, uint64, error) {
	if f.failAcquire {
		return false, 0, errors.New("acquire boom")
	}
	return f.InMemoryActorStore.Acquire(ctx, k, ttl)
}
func (f *faultyStore) Deliver(ctx context.Context, k UnitKey, m ActorMessage) error {
	if f.failDeliver {
		return errors.New("deliver boom")
	}
	return f.InMemoryActorStore.Deliver(ctx, k, m)
}
func (f *faultyStore) DrainBatch(ctx context.Context, k UnitKey, n int) ([]ActorMessage, error) {
	if f.failDrain {
		return nil, errors.New("drain boom")
	}
	return f.InMemoryActorStore.DrainBatch(ctx, k, n)
}
func (f *faultyStore) Empty(ctx context.Context, k UnitKey) (bool, error) {
	if f.failEmpty {
		return false, errors.New("empty boom")
	}
	return f.InMemoryActorStore.Empty(ctx, k)
}

func newFaulty() *faultyStore { return &faultyStore{InMemoryActorStore: NewInMemoryActorStore()} }

// TestRunActor_StoreErrorPaths: every store-op failure in the loop must surface ActorError (so the handler
// redelivers), and on the held-lease paths the lease must end up RELEASED (a store error must never wedge a
// unit). Covers the error branches of RunActorWithMessage (acquire/deliver/drain/empty).
func TestRunActor_StoreErrorPaths(t *testing.T) {
	cfgX := ActorConfig{LeaseTTL: time.Minute, MaxRounds: 2, DrainBatch: 10}

	t.Run("acquire error", func(t *testing.T) {
		s := newFaulty()
		s.failAcquire = true
		res, err := RunActorWithMessage(context.Background(), "k", s, &fakeWorker{convergeAfter: 1}, cfgX, ActorMessage{Body: "x"}, nil)
		if err == nil || res.Outcome != ActorError {
			t.Fatalf("acquire error must yield ActorError+err, got %v / %v", res.Outcome, err)
		}
	})

	t.Run("deliver error on deferred path", func(t *testing.T) {
		// Pre-hold the lease so this Submit defers; the Deliver of its msg then fails.
		s := newFaulty()
		held, _, _ := s.Acquire(context.Background(), "k", time.Minute)
		if !held {
			t.Fatal("pre-acquire failed")
		}
		s.failDeliver = true
		res, err := RunActorWithMessage(context.Background(), "k", s, &fakeWorker{convergeAfter: 1}, cfgX, ActorMessage{Body: "x"}, nil)
		if err == nil || res.Outcome != ActorError {
			t.Fatalf("deliver error must yield ActorError+err, got %v / %v", res.Outcome, err)
		}
	})

	t.Run("drain error releases lease", func(t *testing.T) {
		s := newFaulty()
		s.failDrain = true
		res, err := RunActorWithMessage(context.Background(), "k", s, &fakeWorker{convergeAfter: 1}, cfgX, ActorMessage{Body: "x"}, nil)
		if err == nil || res.Outcome != ActorError {
			t.Fatalf("drain error must yield ActorError+err, got %v / %v", res.Outcome, err)
		}
		// The lease must be released despite the error (no wedge): a fresh acquire succeeds.
		s.failDrain = false
		if held, _, _ := s.Acquire(context.Background(), "k", time.Minute); !held {
			t.Fatal("lease must be released after a drain error (no wedge)")
		}
	})

	t.Run("empty error releases lease", func(t *testing.T) {
		s := newFaulty()
		s.failEmpty = true
		res, err := RunActorWithMessage(context.Background(), "k", s, &fakeWorker{convergeAfter: 1}, cfgX, ActorMessage{Body: "x"}, nil)
		if err == nil || res.Outcome != ActorError {
			t.Fatalf("empty error must yield ActorError+err, got %v / %v", res.Outcome, err)
		}
		s.failEmpty = false
		if held, _, _ := s.Acquire(context.Background(), "k", time.Minute); !held {
			t.Fatal("lease must be released after an empty-check error (no wedge)")
		}
	})
}

// TestRunActor_AckAfterDurableEffect (review #1): a batch drained+folded into a round that then ERRORS must
// NOT be acked — it stays in the inbox so a redelivery re-drains it (at-least-once into the fold). Acking
// before the round (the bug) would drop the folded asks from the inbox while the failed round discards the
// in-memory fold → permanent loss.
func TestRunActor_AckAfterDurableEffect(t *testing.T) {
	store := NewInMemoryActorStore()
	k := UnitKey("k-ack")
	// Pre-deliver two asks, then a worker whose RunRound errors on the first round.
	_ = store.Deliver(context.Background(), k, ActorMessage{Body: "edit A"})
	_ = store.Deliver(context.Background(), k, ActorMessage{Body: "edit B"})
	w := &fakeWorker{convergeAfter: 1, roundErr: context.DeadlineExceeded}

	res, err := RunActor(context.Background(), k, store, w, ActorConfig{LeaseTTL: time.Minute, MaxRounds: 2, DrainBatch: 10}, nil)
	if err == nil || res.Outcome != ActorError {
		t.Fatalf("a round error must surface ActorError, got %v / %v", res.Outcome, err)
	}
	// The folded batch must STILL be in the inbox (un-acked) for redelivery — the lease was released, so a
	// fresh acquire + drain sees both asks again.
	if empty, _ := store.Empty(context.Background(), k); empty {
		t.Fatal("AT-LEAST-ONCE VIOLATION: a failed round acked the folded batch — the asks are lost from the inbox")
	}
	drained, _ := store.DrainBatch(context.Background(), k, 10)
	if len(drained) != 2 {
		t.Fatalf("both folded asks must survive a failed round (un-acked), got %d", len(drained))
	}
}

// TestActorOutcome_String + TestTier/Rung String: cheap but real — the String()s are used in logs/metrics;
// an unlabeled outcome would make an ops trace unreadable. Pins the labels.
func TestActorOutcome_String(t *testing.T) {
	for o, want := range map[ActorOutcome]string{
		ActorConverged: "converged", ActorBusyDeferred: "busy_deferred",
		ActorYieldedReenqueue: "yielded_reenqueue", ActorError: "error", ActorOutcome(99): "unknown",
	} {
		if got := o.String(); got != want {
			t.Errorf("ActorOutcome(%d).String()=%q want %q", o, got, want)
		}
	}
}

func TestAutonomyRung_String(t *testing.T) {
	for r, want := range map[AutonomyRung]string{RungAsk: "ask", RungAuto: "auto", AutonomyRung(9): "unknown"} {
		if got := r.String(); got != want {
			t.Errorf("AutonomyRung(%d).String()=%q want %q", r, got, want)
		}
	}
}
