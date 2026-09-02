package agentkit

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// This file is the actor-per-unit model — ONE live run per work-unit (one agent ⟷ one object), the
// autonomous equivalent of a chat session_id. The runtime depends ONLY on the contracts here
// (Lease / Inbox); two bindings are expected — in-memory (actor_inmem.go, tests/local) and a durable one
// you supply (Postgres, or whatever you already run) — same interface, swappable, the loop unchanged
// between them. The reference consumer hosted each object through AgentRuntime, replacing an ad-hoc
// singleton hack as the single-writer mechanism. Guarantee today: MUTUAL EXCLUSION (one live run at a
// time); the Fence threaded to RunRound is the zombie-write guard but does not yet GUARD the durable
// write (a documented follow-up — it needs fence-aware writes on your side).
//
// The four research-mandated guards (actor turns are µs; LLM turns are seconds + $):
//   - SINGLE-WRITER-PER-UNIT (the lease): a second trigger for a live unit routes to the inbox, never spawns
//     a competing run that could make a conflicting decision.
//   - STARVATION BOUND (rounds ≥ cap → release + re-enqueue): a hot unit never monopolizes a worker.
//   - BOUNDED FOLD (drain a batch, fold into context) — not full-history replay.
//   - STEER AT THE ROUND BOUNDARY: inbox messages are drained between rounds, not mid-LLM-call.

// UnitKey is the grain id = lease key = inbox partition (e.g. "triage:ticket:<uuid>"). One live run per key.
type UnitKey string

// ActorMessage is a v1 inbox message: an external change routed INTO the live unit (an edit, a re-trigger).
// It is deliberately minimal — a full generic AgentMessage[In,Out] (with intent + typed payload) is a
// separate principle; this is the inbox envelope the actor loop needs today. Intent biases the worker's
// fold (it is CONTEXT, never control-flow — no switch on it in the loop).
type ActorMessage struct {
	IdempotencyKey string // dedup key — a redelivered trigger folds once
	Intent         string // optional caller intent (e.g. "project_edited"); CONTEXT for the worker, not control-flow
	Body           string // the change payload (opaque to the kit; the worker interprets it)
	// AckToken is an OPAQUE, BINDING-OWNED handle to THIS EXACT drained row (e.g. the durable inbox row id).
	// DrainBatch sets it; Ack MUST target only the rows whose tokens it was given (not "the oldest N"). The
	// kit and consumers never read or interpret it. This is the single-writer SAFETY fix (review #2): acking
	// by position ("oldest N pending") let a resumed zombie ack the NEW owner's freshly-delivered rows in the
	// lease-steal window → lost wakeups; acking by token means a holder can only ever ack what IT drained.
	AckToken string
}

// ErrLeaseLost is returned by Heartbeat when this holder NO LONGER owns the lease — its fence no longer
// matches the live lease row (another instance stole it after a TTL lapse, e.g. a freeze/partition). It is
// the LEASE-STEAL DETECTION signal: the in-round heartbeat returns it, and the actor loop reacts by
// CANCELLING the round so the superseded ("zombie") run stops wasting work and frees the slot. NOTE the
// honest scope: this is an EFFICIENCY guard (stop a superseded run early; queue redelivery + idempotent
// writes re-drive a clean redo), NOT a conflicting-write guarantee — a cooperative cancel can't stop a write
// already mid-commit, and a frozen process runs no canceller. Conflicting-write SAFETY is the consumer's
// idempotent/last-writer-wins write, not this signal.
var ErrLeaseLost = errors.New("agentkit: lease lost (stolen after TTL lapse)")

// Lease is the single-writer-per-unit primitive. Acquire returns held=false when a run is already live for
// the unit (the caller must route its trigger to the Inbox instead of running). fence is a monotonic token
// bumped on each (re)acquire; it is the LEASE-STEAL DETECTION token (Heartbeat reports ErrLeaseLost when the
// fence no longer matches → the holder was stolen from), NOT a write-conditioner. v1 = an in-memory map; the
// durable v1 = a Postgres unit_lease row (TTL + heartbeat + fence).
type Lease interface {
	Acquire(ctx context.Context, k UnitKey, ttl time.Duration) (held bool, fence uint64, err error)
	// Heartbeat extends the lease iff this holder still owns it (fence matches). It returns ErrLeaseLost if
	// the lease was stolen (fence mismatch) so the loop can cancel a superseded round; a transport/SQL error
	// is returned as itself.
	Heartbeat(ctx context.Context, k UnitKey, fence uint64) error
	Release(ctx context.Context, k UnitKey, fence uint64) error
}

// Inbox is the per-unit durable mailbox. A trigger for a live unit is Delivered here; the live run Drains +
// folds + Acks. v1 = in-memory; a durable binding is a unit_inbox table, or your queue with the UnitKey as
// the message group + dedup on IdempotencyKey.
type Inbox interface {
	Deliver(ctx context.Context, k UnitKey, msg ActorMessage) error
	// DrainBatch returns up to maxN pending messages, each with its AckToken set to a binding-owned handle
	// to that exact row. The caller folds the messages and later passes them (unchanged) to Ack.
	DrainBatch(ctx context.Context, k UnitKey, maxN int) ([]ActorMessage, error)
	// Ack marks EXACTLY the given messages processed, identified by their AckToken — never "the oldest N".
	// This is what keeps a stale holder from acking a new owner's rows in the lease-steal window (review #2).
	Ack(ctx context.Context, k UnitKey, msgs []ActorMessage) error
	Empty(ctx context.Context, k UnitKey) (bool, error)
}

// ActorStore is the union a RunActor needs (lease + inbox). A binding implements both; the in-memory one
// does. (EventLog — state = fold over events — is a third interface; it is deferred to the durable
// binding where snapshot+tail matters. The in-memory v1 folds directly into the worker, which is the
// "bounded fold" guard at this scale.)
type ActorStore interface {
	Lease
	Inbox
}

// Fence is the lease's monotonic ownership token. The actor loop hands it to the worker's RunRound so the
// agent can GUARD its durable write with it: a write conditioned on the fence still matching the lease row
// affects 0 rows if the lease was stolen (the holder's TTL lapsed mid-run and another instance took over).
// This is the zombie-write guard — "a TTL lock without fencing is unsafe" (Kleppmann); without it a holder
// that pauses (GC/partition) past its TTL could wake and clobber the new instance's work. The durable
// binding (Postgres) issues a real monotonic fence; the in-memory binding issues one too. An agent whose
// store has no fence semantics may ignore it.
type Fence uint64

// UnitWorker is the agent's per-unit reasoning seam the actor loop drives. Fold ingests the drained inbox
// batch into the worker's context (the agent decides how the change affects its plan); RunRound runs ONE
// round of the agent's own loop (RunOnce / Pair / RunToolLoop) and reports whether it converged. The actor
// loop owns the lease/inbox/cap/release; the worker owns the reasoning + the durable write (Go owns the
// write — the loop never persists domain state). RunRound receives the lease Fence so the agent can guard
// its durable write against a stale (stolen-lease) holder — see Fence.
type UnitWorker interface {
	Fold(ctx context.Context, msgs []ActorMessage) error
	RunRound(ctx context.Context, round int, fence Fence) (converged bool, err error)
}

// ActorConfig is the actor loop's budget. LeaseTTL bounds how long a held lease survives without a
// heartbeat (crash recovery); MaxRounds is the STARVATION BOUND (the hard cap, then yield + re-enqueue);
// DrainBatch caps how many inbox messages are folded per drain.
type ActorConfig struct {
	LeaseTTL   time.Duration
	MaxRounds  int
	DrainBatch int
}

// ActorOutcome is the terminal result of an actor run.
type ActorOutcome int

const (
	// ActorConverged — the worker converged and the inbox is empty. Lease released; nothing more to do.
	ActorConverged ActorOutcome = iota
	// ActorBusyDeferred — a run was already live for this unit; the trigger was delivered to the inbox and
	// NOT run (the single-writer guarantee). The live run will drain it.
	ActorBusyDeferred
	// ActorYieldedReenqueue — the round cap was hit before convergence (or the inbox refilled at the cap).
	// Lease released; the caller MUST re-enqueue a wake job (the starvation bound). Work is preserved.
	ActorYieldedReenqueue
	// ActorError — a round (or store op) errored. Lease released; the caller disposes (a throttle releases
	// back to your queue via the consumer's own error contract; other errors redeliver).
	ActorError
)

func (o ActorOutcome) String() string {
	switch o {
	case ActorConverged:
		return "converged"
	case ActorBusyDeferred:
		return "busy_deferred"
	case ActorYieldedReenqueue:
		return "yielded_reenqueue"
	case ActorError:
		return "error"
	default:
		return "unknown"
	}
}

// ActorResult is what RunActor returns.
type ActorResult struct {
	Outcome ActorOutcome
	Rounds  int
}

// DefaultLeaseTTL is the lease duration used when ActorConfig.LeaseTTL is unset.
//
// ⚠️ It exists because a ZERO TTL means an instantly-expired lease — so ActorConfig{} silently
// produced TWO holders of one unit, voiding the single-writer guarantee that is the whole point of
// the actor model. MaxRounds and DrainBatch were already defaulted; LeaseTTL was passed to Acquire
// raw. Same defect class as Tier's zero value: a field that is safe to omit everywhere except the
// one place it is load-bearing.
const DefaultLeaseTTL = 5 * time.Minute

// RunActor runs the actor loop for one unit with no initiating message (a cron sweep / wake job).
// See RunActorWithMessage for the with-trigger path.
func RunActor(ctx context.Context, k UnitKey, store ActorStore, worker UnitWorker, cfg ActorConfig, obs Observer) (ActorResult, error) {
	return RunActorWithMessage(ctx, k, store, worker, cfg, ActorMessage{}, obs)
}

// RunActorWithMessage runs the actor loop, optionally seeding the inbox with an initiating message (a
// trigger — an edit, a re-trigger). The single-writer guarantee: if a run is already live for k, the
// message is delivered to the inbox and the call returns ActorBusyDeferred WITHOUT running a round.
//
// The loop: acquire lease (else defer) → drain+fold inbox → run a round → repeat until
// converged-and-empty (ActorConverged) or the cap (ActorYieldedReenqueue, re-enqueue). The lease is ALWAYS
// released before returning (converged, yielded, or error) so a unit is never wedged.
func RunActorWithMessage(ctx context.Context, k UnitKey, store ActorStore, worker UnitWorker, cfg ActorConfig, msg ActorMessage, obs Observer) (ActorResult, error) {
	maxRounds := cfg.MaxRounds
	if maxRounds < 1 {
		maxRounds = DefaultMaxRounds
	}
	drain := cfg.DrainBatch
	if drain < 1 {
		drain = 1
	}
	// ⭐ DEFAULT THE LEASE TTL. MaxRounds and DrainBatch are defaulted above; LeaseTTL was passed to
	// Acquire raw — so ActorConfig{} yielded a ZERO TTL, an instantly-expired lease, and TWO holders
	// of one unit. That silently voids the single-writer guarantee, which is the whole point of the
	// actor model.
	//
	// ⚠️ Same defect class as the Tier zero value: a struct field that is safe to omit everywhere
	// except the one place it is load-bearing. Defaulted here rather than rejected, because unlike a
	// tier there IS a correct conservative answer — a real TTL.
	ttl := cfg.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}

	// Single-writer: try to acquire the lease. If a run is live, route the trigger to the inbox and defer.
	held, fence, err := store.Acquire(ctx, k, ttl)
	if err != nil {
		return ActorResult{Outcome: ActorError}, err
	}
	if !held {
		if hasMessage(msg) {
			if derr := store.Deliver(ctx, k, msg); derr != nil {
				return ActorResult{Outcome: ActorError}, derr
			}
		}
		return ActorResult{Outcome: ActorBusyDeferred}, nil
	}
	// We hold the lease — release it on every exit path (the unit is never wedged). Use a DETACHED context
	// (not the run ctx) so the release still happens when the run ctx is cancelled/timed-out — otherwise a
	// durable Release(ctx, DELETE/UPDATE) on a dead ctx is skipped and the unit stays wedged until TTL,
	// precisely on the error path where releasing matters most (review #8). Release is idempotent
	// (expire-the-row), so an explicit release on the converged path + this deferred one is harmless.
	released := false
	releaseLease := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = store.Release(rctx, k, fence)
		released = true
	}
	defer func() {
		if !released {
			releaseLease()
		}
	}()

	// Seed the inbox with this run's initiating message so the drain folds it uniformly with any queued ones.
	if hasMessage(msg) {
		if derr := store.Deliver(ctx, k, msg); derr != nil {
			return ActorResult{Outcome: ActorError}, derr
		}
	}

	rounds := 0
	for round := 1; round <= maxRounds; round++ {
		// Drain + fold the inbox at the ROUND BOUNDARY (steer between rounds, never mid-LLM-call). The Ack is
		// deferred to AFTER RunRound SUCCEEDS (below) — ack-after-durable-effect (review #1). Acking here,
		// before the round, would lose the folded asks if RunRound errors (e.g. a Vertex throttle): the
		// agent's Fold has consumed them in-memory and a failed round discards that, so a pre-round Ack would
		// drop them from the inbox too → the "at-least-once into the fold" contract is broken. Leaving them
		// UN-acked until the round commits means a failed round re-drains the same batch next time (the fold
		// is idempotent), honoring at-least-once.
		batch, derr := store.DrainBatch(ctx, k, drain)
		if derr != nil {
			return ActorResult{Outcome: ActorError, Rounds: rounds}, derr
		}
		if len(batch) > 0 {
			if ferr := worker.Fold(ctx, batch); ferr != nil {
				return ActorResult{Outcome: ActorError, Rounds: rounds}, ferr
			}
		}

		// Run one round of the agent's own loop, with an IN-ROUND HEARTBEAT (review A1) + DETECT-AND-KILL
		// (Guard 2). A "round" is an ENTIRE agent run (a multi-LLM pair-loop under throttle/retry) — many
		// seconds. The background heartbeat (TTL/3) keeps the lease alive across the round so a SLOW-but-alive
		// holder isn't falsely stolen from (makes single-writer structural, not a "TTL > round wall-clock"
		// coincidence). If the heartbeat detects the lease WAS stolen (we froze past the TTL → another
		// instance took over), it cancels roundCtx → the agent's in-flight LLM/gather/write unwinds via ctx,
		// this superseded run stops (saving spend + freeing the slot), and — having committed/acked nothing —
		// the queued message redelivers to the fresh owner (idempotent redo). This is the EFFICIENCY guard;
		// conflicting-write SAFETY is the consumer's idempotent write, not this.
		roundStart := nowFunc()
		roundCtx, cancelRound := context.WithCancel(ctx)
		hbStop := startHeartbeat(roundCtx, store, k, fence, cfg.LeaseTTL, cancelRound)
		converged, rerr := worker.RunRound(roundCtx, round, Fence(fence))
		hbStop()
		cancelRound()
		rounds = round
		// Observe the round's REAL wall-clock (the work the loop can legitimately time — fold + the agent's
		// RunRound). Earlier this passed a 0 duration, polluting the latency histogram (review minor); now it
		// reports the actual elapsed time under the "actor_round" role.
		if obs != nil {
			obs.ObserveLLM("actor_round", round, nowFunc().Sub(roundStart))
		}
		if rerr != nil {
			// The round failed (e.g. a throttle) — do NOT ack the drained batch; it stays in the inbox and is
			// re-drained on redelivery (at-least-once, review #1). The agent's in-memory fold is discarded
			// with the failed round, so the durable inbox is the only surviving copy — keep it.
			return ActorResult{Outcome: ActorError, Rounds: rounds}, rerr
		}
		// Round committed its durable effect → NOW ack the batch we folded into it (ack-after-durable-effect).
		if len(batch) > 0 {
			if aerr := store.Ack(ctx, k, batch); aerr != nil {
				return ActorResult{Outcome: ActorError, Rounds: rounds}, aerr
			}
		}

		if converged {
			// Converged — but only DONE if the inbox is empty (a message that arrived mid-round must be
			// handled, not stranded). If it refilled, keep looping (subject to the cap).
			empty, eerr := store.Empty(ctx, k)
			if eerr != nil {
				return ActorResult{Outcome: ActorError, Rounds: rounds}, eerr
			}
			if empty {
				// LOST-WAKEUP GUARD (review #2 — the closing edge of the both-edges invariant). The
				// converged-empty check and the lease release are not one atomic step: a producer that
				// Delivers an ask in the gap, then Acquires, would get held=false (we still hold the lease)
				// → it defers, trusting us to drain — but we're about to exit. So: RELEASE FIRST, then
				// re-check the inbox. After release a producer's Acquire succeeds (it runs the straggler
				// itself), and any ask delivered DURING our run-then-release window is caught here →
				// re-enqueue a wake so a fresh instance drains it. Release-then-recheck closes the window
				// that check-then-release leaves open.
				releaseLease()
				stragglerEmpty, serr := store.Empty(ctx, k)
				if serr != nil {
					// Couldn't verify; treat as "maybe a straggler" → yield+re-enqueue (safe over-fire). Log
					// it — a transient store error here is otherwise invisible (a review's observability nit).
					log.Printf("[WARN] agentkit: actor %s: straggler-empty check failed: %v (yielding to re-enqueue)", k, serr)
					return ActorResult{Outcome: ActorYieldedReenqueue, Rounds: rounds}, nil
				}
				if !stragglerEmpty {
					// An ask landed in the gap; we no longer hold the lease → re-enqueue a wake so a fresh
					// instance picks it up (never stranded).
					return ActorResult{Outcome: ActorYieldedReenqueue, Rounds: rounds}, nil
				}
				return ActorResult{Outcome: ActorConverged, Rounds: rounds}, nil
			}
		}
		// Heartbeat the lease each round so a long run doesn't lose it to TTL expiry.
		if herr := store.Heartbeat(ctx, k, fence); herr != nil {
			return ActorResult{Outcome: ActorError, Rounds: rounds}, herr
		}
	}

	// Starvation bound: hit the cap without converging-and-empty → yield. The caller re-enqueues a wake job;
	// the lease is released by the deferred Release so the next run takes over. Work so far is persisted by
	// the worker (Go owns the write); nothing is lost.
	return ActorResult{Outcome: ActorYieldedReenqueue, Rounds: rounds}, nil
}

// hasMessage reports whether an ActorMessage carries anything to deliver (a zero message = a bare wake).
func hasMessage(m ActorMessage) bool {
	return m.Body != "" || m.Intent != "" || m.IdempotencyKey != ""
}

// startHeartbeat keeps the lease alive for the duration of a long RunRound by beating in the background at
// ~TTL/3 (so two missed beats still don't expire the lease), until the returned stop func is called (always
// call it — it ends the goroutine + the timer). This is what makes single-writer STRUCTURAL across a
// multi-LLM round, not dependent on "round wall-clock < LeaseTTL" (review A1). A fence-mismatched heartbeat
// no-ops in the binding, so a stale holder's background beats are harmless. ttl<=0 (no TTL configured) →
// no heartbeat (an in-memory test binding with no expiry needs none). Uses time.NewTimer+Stop, never
// time.After in a select (the documented goroutine-leak pitfall).
// cancelRound is invoked when the heartbeat detects the lease was STOLEN (Heartbeat → ErrLeaseLost) — it
// cancels the round so the superseded run stops (Guard 2, efficiency). nil disables the kill (a binding with
// no steal semantics, e.g. a test that never steals).
func startHeartbeat(ctx context.Context, store ActorStore, k UnitKey, fence uint64, ttl time.Duration, cancelRound context.CancelFunc) (stop func()) {
	if ttl <= 0 {
		return func() {}
	}
	// Beat at ttl/3 (a comfortable ~3 beats per lease window), but never SLOWER than every 10s for a long
	// TTL — and crucially never at-or-beyond the TTL itself. The 10s floor must not push the interval past
	// the TTL (a review caught this: a 15s TTL → max(5s,10s)=10s, and 10s<15s only by luck; a 12s
	// TTL would beat at 10s with the lease dying at 12s — too close). So cap the floored interval at ttl/3
	// again, then take ttl/2 as the hard backstop for a tiny TTL (mostly tests). Result: interval is always
	// < ttl with at least ~2 beats before expiry.
	interval := ttl / 3
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	if interval >= ttl/2 {
		// The 10s floor (or a tiny TTL) pushed the interval too close to expiry → beat at half the TTL so at
		// least one beat reliably lands before the lease dies.
		interval = ttl / 2
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTimer(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := store.Heartbeat(ctx, k, fence); errors.Is(err, ErrLeaseLost) && cancelRound != nil {
					// The lease was stolen (we froze past the TTL; another instance took over). Cancel the
					// round so this superseded run stops — saving LLM spend + freeing the slot. Commits/acks
					// nothing (Guard 2, efficiency); queue redelivery + idempotent writes re-drive a clean redo.
					cancelRound()
					return
				}
				t.Reset(interval)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
