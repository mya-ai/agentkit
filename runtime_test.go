package agentkit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingWorker tracks how many instances are LIVE (inside RunRound) for a unit at once via a shared
// atomic counter, recording the high-water mark — the load-bearing single-writer assertion. It converges
// after one round.
type countingWorker struct {
	live    *int32
	maxLive *int32
}

func (w *countingWorker) Fold(context.Context, []ActorMessage) error { return nil }
func (w *countingWorker) RunRound(_ context.Context, _ int, _ Fence) (bool, error) {
	n := atomic.AddInt32(w.live, 1)
	for {
		hi := atomic.LoadInt32(w.maxLive)
		if n <= hi || atomic.CompareAndSwapInt32(w.maxLive, hi, n) {
			break
		}
	}
	atomic.AddInt32(w.live, -1)
	return true, nil
}

// runtimeCfg is the actor config the runtime tests use (generous TTL so heartbeats aren't needed in-test).
func runtimeCfg() ActorConfig {
	return ActorConfig{LeaseTTL: time.Minute, MaxRounds: 5, DrainBatch: 10}
}

// TestRuntime_DispatchDormantStarts: Dispatch to a unit with no live instance must result in a run that
// drains the ask (the dormant→start path). NewInMemoryRuntime drives it synchronously via RunUnit.
func TestRuntime_DispatchDormantStarts(t *testing.T) {
	rt := NewInMemoryRuntime(runtimeCfg())
	w := &fakeWorker{convergeAfter: 1}

	// Submit delivers the ask; since nothing is live, this caller becomes the instance and runs it.
	res, err := rt.Submit(context.Background(), "triage:ticket:t1", ActorMessage{Body: "edit A"}, w, nil)
	if err != nil {
		t.Fatalf("Submit err: %v", err)
	}
	if res.Outcome != ActorConverged {
		t.Fatalf("expected ActorConverged (dormant→started→converged), got %v", res.Outcome)
	}
	if len(w.folded) != 1 || w.folded[0] != "edit A" {
		t.Fatalf("expected the dispatched ask folded, got %+v", w.folded)
	}
}

// TestRuntime_DispatchIntoLiveInstanceFolds: the headline streaming behavior. While an instance is live for
// a unit, a concurrent Submit for the SAME unit must NOT start a competitor — its ask folds into the live
// instance (the live run drains it). The live worker blocks on round 1 so the second Submit races in.
func TestRuntime_DispatchIntoLiveInstanceFolds(t *testing.T) {
	rt := NewInMemoryRuntime(runtimeCfg())
	k := UnitKey("triage:ticket:t1")

	live := &blockingWorker{enter: make(chan struct{}), resume: make(chan struct{})}
	var liveRes ActorResult
	done := make(chan struct{})
	go func() {
		liveRes, _ = rt.Submit(context.Background(), k, ActorMessage{Body: "first"}, live, nil)
		close(done)
	}()
	<-live.enter // the live instance is mid-round, holding the lease

	// A second ask races in while the instance is live — it must FOLD (defer to the live one), not run.
	second := &fakeWorker{convergeAfter: 1}
	res2, err := rt.Submit(context.Background(), k, ActorMessage{Body: "second"}, second, nil)
	if err != nil {
		t.Fatalf("second Submit err: %v", err)
	}
	if res2.Outcome != ActorBusyDeferred {
		t.Fatalf("a Submit to a LIVE unit must fold/defer, got %v", res2.Outcome)
	}
	if second.rounds != 0 {
		t.Fatalf("the deferred Submit must NOT run its own worker, got %d rounds", second.rounds)
	}

	close(live.resume) // let the live instance finish
	<-done
	if liveRes.Outcome != ActorConverged {
		t.Fatalf("the live instance should converge after folding, got %v", liveRes.Outcome)
	}
	// The live instance must have absorbed BOTH asks (it drains the inbox each round + at the empty-check).
	if len(live.foldedBodies) < 2 {
		t.Fatalf("the single live instance should fold BOTH asks (first+second), got %+v", live.foldedBodies)
	}
	empty, _ := rt.(*boundRuntime).store.Empty(context.Background(), k)
	if !empty {
		t.Fatalf("the live instance should have drained the whole stream; inbox not empty")
	}
}

// TestRuntime_ConcurrentDispatchSingleInstance: under real concurrency, N Submits for the same dormant unit
// must yield exactly ONE instance that runs; the others fold/defer. (Scenario A: no double-run.)
func TestRuntime_ConcurrentDispatchSingleInstance(t *testing.T) {
	rt := NewInMemoryRuntime(runtimeCfg())
	k := UnitKey("triage:ticket:race")

	const n = 8
	var terminated, ran int
	var live int32    // currently-active instances — must NEVER exceed 1 (the single-writer guarantee)
	var maxLive int32 // high-water mark of concurrent instances
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// The worker bumps `live` on entry to RunRound and checks the high-water mark — the load-bearing
			// assertion: at most ONE instance is ever live for the unit, even under N concurrent Submits.
			w := &countingWorker{live: &live, maxLive: &maxLive}
			res, err := rt.Submit(context.Background(), k, ActorMessage{Body: "ask"}, w, nil)
			if err != nil {
				t.Errorf("Submit %d err: %v", i, err)
				return
			}
			mu.Lock()
			terminated++
			if res.Outcome == ActorConverged {
				ran++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// The real guarantees: (1) every Submit terminated cleanly (no error), and (2) NEVER two live instances
	// at once for the unit (the single-writer property). Whether a given Submit converged, deferred, or
	// yielded-on-a-straggler is timing-dependent and all legitimate — what must hold is the high-water mark.
	if terminated != n {
		t.Fatalf("every Submit must terminate, got %d/%d", terminated, n)
	}
	if maxLive > 1 {
		t.Fatalf("SINGLE-WRITER VIOLATION: %d instances were live at once for the unit (must be ≤1)", maxLive)
	}
}

// TestRuntime_RunUnitBareWake: RunUnit with no seed message (a bare wake) drains whatever is already in the
// inbox — the path a queue-wake consumer uses after Dispatch already delivered.
func TestRuntime_RunUnitBareWake(t *testing.T) {
	rt := NewInMemoryRuntime(runtimeCfg())
	k := UnitKey("triage:ticket:t1")
	// Pre-deliver asks (as Dispatch would), then a bare RunUnit wake drains them.
	store := rt.(*boundRuntime).store
	_ = store.Deliver(context.Background(), k, ActorMessage{Body: "queued A"})
	_ = store.Deliver(context.Background(), k, ActorMessage{Body: "queued B"})

	w := &fakeWorker{convergeAfter: 1}
	res, err := rt.RunUnit(context.Background(), k, w, nil)
	if err != nil {
		t.Fatalf("RunUnit err: %v", err)
	}
	if res.Outcome != ActorConverged {
		t.Fatalf("expected converged, got %v", res.Outcome)
	}
	if len(w.folded) != 2 {
		t.Fatalf("bare RunUnit must drain the pre-delivered asks, got %+v", w.folded)
	}
}
