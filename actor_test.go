package agentkit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeWorker is a UnitWorker that runs a scripted number of rounds before converging, recording how many
// rounds ran and what messages it folded. It stands in for a real agent's per-round reasoning.
type fakeWorker struct {
	convergeAfter int      // converge once rounds >= this
	rounds        int      // rounds actually run
	folded        []string // message bodies folded in, in order
	roundErr      error    // if set, RunRound returns it on the first round
}

func (w *fakeWorker) Fold(_ context.Context, msgs []ActorMessage) error {
	for _, m := range msgs {
		w.folded = append(w.folded, m.Body)
	}
	return nil
}

func (w *fakeWorker) RunRound(_ context.Context, _ int, _ Fence) (converged bool, err error) {
	w.rounds++
	if w.roundErr != nil {
		return false, w.roundErr
	}
	return w.rounds >= w.convergeAfter, nil
}

func cfg() ActorConfig {
	return ActorConfig{LeaseTTL: time.Minute, MaxRounds: 3, DrainBatch: 10}
}

// ctxAwareWorker blocks in RunRound until EITHER the round ctx is cancelled (the Guard-2 kill) or a fallback
// timer fires. It records whether it was cancelled — the assertion that detect-and-kill actually stopped it.
type ctxAwareWorker struct {
	entered   chan struct{}
	cancelled bool
}

func (w *ctxAwareWorker) Fold(context.Context, []ActorMessage) error { return nil }
func (w *ctxAwareWorker) RunRound(ctx context.Context, _ int, _ Fence) (bool, error) {
	close(w.entered)
	t := time.NewTimer(5 * time.Second) // fallback so the test can't hang if the kill never fires
	defer t.Stop()
	select {
	case <-ctx.Done():
		w.cancelled = true
		return false, ctx.Err()
	case <-t.C:
		return true, nil
	}
}

// TestRunActor_StolenLeaseCancelsRound (Guard 2): if the lease is STOLEN mid-round (fence bumped by another
// instance after a TTL lapse), the in-round heartbeat detects it (Heartbeat→ErrLeaseLost) and CANCELS the
// round → the superseded run stops (returns error) WITHOUT acking the inbox or converging. This is the
// efficiency kill: a frozen/slow holder that lost its lease stops wasting work; queue redelivery plus
// idempotent writes re-drive a clean redo. (Correctness — no conflicting write — is the consumer's idempotent write, not this.)
func TestRunActor_StolenLeaseCancelsRound(t *testing.T) {
	store := NewInMemoryActorStore()
	k := UnitKey("u-steal")
	// Short TTL so the heartbeat interval (TTL/2) fires fast; the worker blocks until the kill.
	c := ActorConfig{LeaseTTL: 60 * time.Millisecond, MaxRounds: 1, DrainBatch: 10}
	_ = store.Deliver(context.Background(), k, ActorMessage{Body: "edit"})

	w := &ctxAwareWorker{entered: make(chan struct{})}
	go func() {
		<-w.entered // the round is running, holding fence=1
		// Another instance STEALS the lease: force-expire then re-acquire → fence bumps to 2.
		store.mu.Lock()
		ls := store.leases[k]
		ls.expires = nowFunc().Add(-time.Second) // expire it
		store.leases[k] = ls
		store.mu.Unlock()
		_, _, _ = store.Acquire(context.Background(), k, time.Minute) // steal → fence=2
	}()

	res, err := RunActorWithMessage(context.Background(), k, store, w, c, ActorMessage{}, nil)

	if !w.cancelled {
		t.Fatal("Guard 2 FAILED: the round was NOT cancelled after the lease was stolen — a superseded run kept running")
	}
	if err == nil || res.Outcome != ActorError {
		t.Fatalf("a stolen-lease round must end ActorError (cancelled), got %v / %v", res.Outcome, err)
	}
	// The inbox ask must NOT be acked (the superseded run committed nothing) → the new owner re-drains it.
	if empty, _ := store.Empty(context.Background(), k); empty {
		t.Fatal("a killed superseded run must NOT ack the inbox — the ask must survive for the new owner")
	}
}

// TestRunActor_StolenLeaseCancelsRound_Hardened runs the steal-mid-round race MANY times under -race to
// shake out timing bugs in the heartbeat-cancel path (review hardening): across iterations, a stolen lease
// must ALWAYS cancel the superseded round (never hang, never commit/ack). Run with `go test -race`.
func TestRunActor_StolenLeaseCancelsRound_Hardened(t *testing.T) {
	for i := 0; i < 30; i++ {
		store := NewInMemoryActorStore()
		k := UnitKey("u-steal-h")
		c := ActorConfig{LeaseTTL: 40 * time.Millisecond, MaxRounds: 1, DrainBatch: 10}
		_ = store.Deliver(context.Background(), k, ActorMessage{Body: "edit"})

		w := &ctxAwareWorker{entered: make(chan struct{})}
		go func() {
			<-w.entered
			store.mu.Lock()
			ls := store.leases[k]
			ls.expires = nowFunc().Add(-time.Second)
			store.leases[k] = ls
			store.mu.Unlock()
			_, _, _ = store.Acquire(context.Background(), k, time.Minute) // steal → fence bumps
		}()

		res, err := RunActorWithMessage(context.Background(), k, store, w, c, ActorMessage{}, nil)
		if !w.cancelled || err == nil || res.Outcome != ActorError {
			t.Fatalf("iter %d: steal must cancel the round (cancelled=%v outcome=%v err=%v)", i, w.cancelled, res.Outcome, err)
		}
		if empty, _ := store.Empty(context.Background(), k); empty {
			t.Fatalf("iter %d: a killed run must not ack the inbox", i)
		}
	}
}

// TestInMemoryLease_TTLExpiryAndStealWithMonotonicFence pins the clock to prove the in-memory binding now
// honors the durable binding's lease semantics (review #3): a lease EXPIRES after its acquire-time TTL (not
// a hardcoded minute), an expired lease is STEALABLE, a live one is not, and the fence is STRICTLY MONOTONIC
// across acquire→release→acquire (never resets). This is the test that was structurally impossible before
// (the hardcoded 1m extension hid TTL expiry).
func TestInMemoryLease_TTLExpiryAndStealWithMonotonicFence(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := base
	orig := nowFunc
	nowFunc = func() time.Time { return clock }
	defer func() { nowFunc = orig }()

	s := NewInMemoryActorStore()
	k := UnitKey("u1")
	ttl := 30 * time.Second

	held, f1, _ := s.Acquire(context.Background(), k, ttl)
	if !held {
		t.Fatal("first acquire should hold")
	}
	// Within TTL: not stealable.
	clock = base.Add(20 * time.Second)
	if held2, _, _ := s.Acquire(context.Background(), k, ttl); held2 {
		t.Fatal("a LIVE (within-TTL) lease must not be stealable")
	}
	// Heartbeat extends by the TTL (not a hardcoded minute): now expiry is at 20s+30s=50s.
	_ = s.Heartbeat(context.Background(), k, f1)
	clock = base.Add(45 * time.Second) // past the ORIGINAL 30s expiry, but within the heartbeated window
	if held3, _, _ := s.Acquire(context.Background(), k, ttl); held3 {
		t.Fatal("heartbeat must have extended the lease by its TTL — still live at 45s")
	}
	// Past the heartbeated expiry (50s): now stealable, fence bumps.
	clock = base.Add(51 * time.Second)
	held4, f2, _ := s.Acquire(context.Background(), k, ttl)
	if !held4 {
		t.Fatal("an EXPIRED lease must be stealable")
	}
	if f2 <= f1 {
		t.Fatalf("steal must bump the fence: f1=%d f2=%d", f1, f2)
	}
	// Clean release then re-acquire → fence still climbs (never resets — the durable binding's bug can't occur here).
	_ = s.Release(context.Background(), k, f2)
	held5, f3, _ := s.Acquire(context.Background(), k, ttl)
	if !held5 || f3 <= f2 {
		t.Fatalf("fence must stay strictly monotonic across release: f2=%d f3=%d", f2, f3)
	}
}

// slowWorker blocks in RunRound for `dur`, so a round outlasts a short LeaseTTL — exercising the in-round
// heartbeat (review A1: the lease must stay alive DURING a long round, not just between rounds).
type slowWorker struct{ dur time.Duration }

func (w slowWorker) Fold(context.Context, []ActorMessage) error { return nil }
func (w slowWorker) RunRound(ctx context.Context, _ int, _ Fence) (bool, error) {
	t := time.NewTimer(w.dur)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
	return true, nil
}

// TestRunActor_InRoundHeartbeatKeepsLease (review A1): a round that runs LONGER than the LeaseTTL must NOT
// lose its lease mid-round — the background in-round heartbeat keeps it alive, so single-writer is structural,
// not a "round < TTL" config coincidence. Without the heartbeat, a concurrent Acquire mid-round would steal
// the expired lease (two live instances). We assert a concurrent steal FAILS throughout the long round.
func TestRunActor_InRoundHeartbeatKeepsLease(t *testing.T) {
	store := NewInMemoryActorStore()
	k := UnitKey("u-slow")
	// TTL 60ms (heartbeat interval clamps to TTL/2=30ms); the round blocks 180ms = 3× TTL. Without
	// heartbeating, the lease would expire ~3× over during the round.
	c := ActorConfig{LeaseTTL: 60 * time.Millisecond, MaxRounds: 1, DrainBatch: 10}

	stealFailed := make(chan bool, 1)
	go func() {
		// Hammer Acquire during the round; with the heartbeat, every attempt must see a LIVE lease (held=false).
		deadline := time.Now().Add(150 * time.Millisecond)
		stolen := false
		for time.Now().Before(deadline) {
			if held, _, _ := store.Acquire(context.Background(), k, c.LeaseTTL); held {
				stolen = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		stealFailed <- !stolen
	}()

	res, err := RunActorWithMessage(context.Background(), k, store, slowWorker{dur: 180 * time.Millisecond}, c, ActorMessage{Body: "go"}, nil)
	if err != nil {
		t.Fatalf("slow run err: %v", err)
	}
	if res.Outcome != ActorConverged {
		t.Fatalf("expected converged, got %v", res.Outcome)
	}
	if ok := <-stealFailed; !ok {
		t.Fatal("SINGLE-WRITER VIOLATION: the lease was stolen mid-round — the in-round heartbeat must keep it alive across a round longer than the TTL")
	}
}

// TestRunActor_AcquiresRunsReleases: the happy path — acquire the lease, run to convergence, release.
func TestRunActor_AcquiresRunsReleases(t *testing.T) {
	store := NewInMemoryActorStore()
	w := &fakeWorker{convergeAfter: 2}

	res, err := RunActor(context.Background(), "triage:ticket:t1", store, w, cfg(), nil)
	if err != nil {
		t.Fatalf("RunActor err: %v", err)
	}
	if res.Outcome != ActorConverged {
		t.Fatalf("expected ActorConverged, got %v", res.Outcome)
	}
	if w.rounds != 2 {
		t.Fatalf("expected 2 rounds to convergence, got %d", w.rounds)
	}
	// Lease must be released after a converged run (a fresh Acquire succeeds).
	held, _, _ := store.Acquire(context.Background(), "triage:ticket:t1", time.Minute)
	if !held {
		t.Fatalf("lease should be free after a converged run")
	}
}

// TestRunActor_SingleWriterRoutesToInbox: the load-bearing guarantee. While a run holds the lease, a second
// RunActor for the SAME unit must NOT run a competing round — it delivers the trigger to the inbox and
// returns. The first run is simulated by pre-acquiring the lease.
func TestRunActor_SingleWriterRoutesToInbox(t *testing.T) {
	store := NewInMemoryActorStore()
	k := UnitKey("triage:ticket:t1")

	// Simulate a live run holding the lease.
	held, _, err := store.Acquire(context.Background(), k, time.Minute)
	if !held || err != nil {
		t.Fatalf("pre-acquire failed: held=%v err=%v", held, err)
	}

	// A second trigger arrives carrying a message; it must route to the inbox, not run.
	w := &fakeWorker{convergeAfter: 1}
	res, err := RunActorWithMessage(context.Background(), k, store, w, cfg(), ActorMessage{Body: "project edited"}, nil)
	if err != nil {
		t.Fatalf("RunActorWithMessage err: %v", err)
	}
	if res.Outcome != ActorBusyDeferred {
		t.Fatalf("expected ActorBusyDeferred (routed to inbox), got %v", res.Outcome)
	}
	if w.rounds != 0 {
		t.Fatalf("a deferred trigger must NOT run a round, got %d", w.rounds)
	}
	// The message must be sitting in the inbox for the live run to drain.
	empty, _ := store.Empty(context.Background(), k)
	if empty {
		t.Fatalf("the deferred message should be in the inbox")
	}
}

// TestRunActor_DrainsInboxAndFolds: messages already in the inbox are drained + folded into the worker
// before/across rounds (event-driven upkeep: an edit is just an inbox message).
func TestRunActor_DrainsInboxAndFolds(t *testing.T) {
	store := NewInMemoryActorStore()
	k := UnitKey("triage:ticket:t1")
	_ = store.Deliver(context.Background(), k, ActorMessage{Body: "edit A"})
	_ = store.Deliver(context.Background(), k, ActorMessage{Body: "edit B"})

	w := &fakeWorker{convergeAfter: 1}
	res, err := RunActor(context.Background(), k, store, w, cfg(), nil)
	if err != nil {
		t.Fatalf("RunActor err: %v", err)
	}
	if res.Outcome != ActorConverged {
		t.Fatalf("expected converged, got %v", res.Outcome)
	}
	if len(w.folded) != 2 || w.folded[0] != "edit A" || w.folded[1] != "edit B" {
		t.Fatalf("expected both inbox messages folded in order, got %+v", w.folded)
	}
	// Drained messages must be acked (inbox empty after a converged run).
	empty, _ := store.Empty(context.Background(), k)
	if !empty {
		t.Fatalf("inbox should be empty (acked) after a converged run")
	}
}

// TestRunActor_StarvationBound: the non-negotiable guard. A worker that never converges must STOP at
// MaxRounds, release the lease, and signal a re-enqueue (a hot unit can't monopolize a worker).
func TestRunActor_StarvationBound(t *testing.T) {
	store := NewInMemoryActorStore()
	k := UnitKey("triage:ticket:hot")
	w := &fakeWorker{convergeAfter: 999} // never converges within the cap

	res, err := RunActor(context.Background(), k, store, w, cfg(), nil)
	if err != nil {
		t.Fatalf("RunActor err: %v", err)
	}
	if res.Outcome != ActorYieldedReenqueue {
		t.Fatalf("expected ActorYieldedReenqueue at the cap, got %v", res.Outcome)
	}
	if w.rounds != 3 { // MaxRounds
		t.Fatalf("expected exactly MaxRounds (3) rounds, got %d", w.rounds)
	}
	// The lease MUST be released so the re-enqueued job can take over.
	held, _, _ := store.Acquire(context.Background(), k, time.Minute)
	if !held {
		t.Fatalf("lease must be released at the starvation bound")
	}
}

// blockingWorker blocks on its FIRST round until released, so the test can hold a live run while a second
// RunActor races for the same unit. Later rounds (if the inbox refilled mid-run) don't block.
type blockingWorker struct {
	enter        chan struct{} // signal: the first round started
	resume       chan struct{} // the test closes this to let the first round finish
	rounds       int
	entered      bool
	foldedBodies []string // bodies folded across all rounds (proves the live instance absorbed the stream)
}

func (w *blockingWorker) Fold(_ context.Context, msgs []ActorMessage) error {
	for _, m := range msgs {
		w.foldedBodies = append(w.foldedBodies, m.Body)
	}
	return nil
}
func (w *blockingWorker) RunRound(_ context.Context, _ int, _ Fence) (bool, error) {
	w.rounds++
	if !w.entered {
		w.entered = true
		w.enter <- struct{}{}
		<-w.resume
	}
	return true, nil // converge each round (the loop continues only if the inbox refilled)
}

// TestRunActor_ConcurrentSingleWriter is the load-bearing race: two RunActor calls for the SAME unit run
// concurrently. EXACTLY ONE acquires the lease and runs; the other must defer to the inbox (never run a
// competing round). This proves the single-writer guarantee under real concurrency, not just a pre-acquired
// lease.
func TestRunActor_ConcurrentSingleWriter(t *testing.T) {
	store := NewInMemoryActorStore()
	k := UnitKey("triage:ticket:race")

	live := &blockingWorker{enter: make(chan struct{}), resume: make(chan struct{})}
	var liveRes ActorResult
	var liveErr error
	done := make(chan struct{})
	go func() {
		liveRes, liveErr = RunActorWithMessage(context.Background(), k, store, live, cfg(), ActorMessage{Body: "first"}, nil)
		close(done)
	}()

	// Wait until the live run is inside its round (holding the lease).
	<-live.enter

	// A second trigger races in while the lease is held — it must defer.
	second := &fakeWorker{convergeAfter: 1}
	res2, err2 := RunActorWithMessage(context.Background(), k, store, second, cfg(), ActorMessage{Body: "second"}, nil)
	if err2 != nil {
		t.Fatalf("second RunActor err: %v", err2)
	}
	if res2.Outcome != ActorBusyDeferred {
		t.Fatalf("the racing run must defer, got %v", res2.Outcome)
	}
	if second.rounds != 0 {
		t.Fatalf("the racing run must NOT run a round, got %d", second.rounds)
	}

	// Let the live run finish.
	close(live.resume)
	<-done
	if liveErr != nil {
		t.Fatalf("live run err: %v", liveErr)
	}
	if liveRes.Outcome != ActorConverged {
		t.Fatalf("live run should converge, got %v", liveRes.Outcome)
	}
	// The live run is the SINGLE writer: it ran the round(s); the racing run ran none. The live run also
	// correctly absorbed "second" if it arrived before convergence (it drains the inbox each round), so by
	// the end the inbox is empty — the message was handled by the one live writer, never by a competitor.
	if live.rounds < 1 {
		t.Fatalf("the live run should have run at least one round, got %d", live.rounds)
	}
	empty, _ := store.Empty(context.Background(), k)
	if !empty {
		t.Fatalf("the single live writer should have absorbed all messages, inbox not empty")
	}
}

// TestRunActor_RoundErrorReleasesLease: a round error is terminal but must STILL release the lease (a crash
// mid-run can't wedge the unit forever — though here it's a clean error return).
func TestRunActor_RoundErrorReleasesLease(t *testing.T) {
	store := NewInMemoryActorStore()
	k := UnitKey("triage:ticket:t1")
	w := &fakeWorker{convergeAfter: 1, roundErr: context.DeadlineExceeded}

	res, err := RunActor(context.Background(), k, store, w, cfg(), nil)
	if err == nil {
		t.Fatalf("expected the round error surfaced")
	}
	if res.Outcome != ActorError {
		t.Fatalf("expected ActorError, got %v", res.Outcome)
	}
	held, _, _ := store.Acquire(context.Background(), k, time.Minute)
	if !held {
		t.Fatalf("lease must be released even on a round error")
	}
}

// gateWorker signals when its round starts and blocks until released, so a second
// attempt can be raced against a genuinely-held lease.
type gateWorker struct {
	entered chan struct{}
	release chan struct{}
	onRun   func()
	once    sync.Once // RunRound may be called more than once; the gate opens on the first
}

func (w *gateWorker) Fold(context.Context, []ActorMessage) error { return nil }
func (w *gateWorker) RunRound(context.Context, int, Fence) (bool, error) {
	if w.onRun != nil {
		w.onRun()
	}
	w.once.Do(func() {
		if w.entered != nil {
			close(w.entered)
		}
		if w.release != nil {
			<-w.release
		}
	})
	return true, nil
}

// ⭐ THE ONE ROUTE TO A TWO-HOLDER VIOLATION, and nothing exercised it. Every
// other actor test passes an explicit LeaseTTL, so the zero-value default was
// unreachable from the suite: deleting the `ttl <= 0` guard, or setting
// DefaultLeaseTTL to a nanosecond, both survived a full mutation run.
//
// ⚠️ A zero TTL means an INSTANTLY-EXPIRED lease. ActorConfig{} would then let a
// second holder steal immediately and run the same unit concurrently — defeating
// single-writer, the framework's core claim, while every dedicated single-writer
// test stayed green.
func TestRunActor_ZeroLeaseTTLStillSingleWriter(t *testing.T) {
	store := NewInMemoryActorStore()
	const k = UnitKey("triage:ticket:t1")

	held := &gateWorker{entered: make(chan struct{}), release: make(chan struct{})}

	// LeaseTTL deliberately unset — the config a newcomer writes first.
	cfg := ActorConfig{MaxRounds: 2, DrainBatch: 4}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := RunActor(context.Background(), k, store, held, cfg, nil); err != nil {
			t.Errorf("first RunActor: %v", err)
		}
	}()

	<-held.entered
	intruder := &gateWorker{onRun: func() {
		t.Error("⛔ SINGLE-WRITER VIOLATION: a second instance ran while the first held the lease — " +
			"an unset LeaseTTL must DEFAULT, not expire instantly")
	}}
	res, err := RunActorWithMessage(context.Background(), k, store, intruder, cfg,
		ActorMessage{Body: "racing"}, nil)
	if err != nil {
		t.Fatalf("second RunActor: %v", err)
	}
	if res.Outcome != ActorBusyDeferred {
		t.Errorf("second attempt outcome = %v, want ActorBusyDeferred (folded into the live inbox)", res.Outcome)
	}

	close(held.release)
	<-done
}
