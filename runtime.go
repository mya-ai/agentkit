package agentkit

import "context"

// This file is the Agent Runtime seam: the injectable object that hosts an agent INSTANCE — gives it
// identity (UnitKey), keeps exactly ONE live per unit (the lease), feeds it a STREAM of asks (the inbox it
// folds each round), and survives across worker tasks (the durable binding). It is the framework's contract
// for "how an agent instance is executed"; YOU supply the binding (an in-memory one ships here for tests
// and local dev; a durable one — a Postgres lease+inbox, say — is yours). The kit owns the loop + the
// contract; it NEVER imports infrastructure.
//
// Two verbs (the whole runtime API):
//   - Submit  — the PRODUCER front door: an ask entered the stream for a unit. Durably accept it, then make
//               the runtime-owned wake-or-enqueue decision ATOMICALLY (no live instance → become it and run;
//               live instance → the ask is enqueued for the live one to fold). The caller never guesses
//               liveness — that guess was the broken 2-level design.
//   - RunUnit — the CONSUMER side: be the live instance for a unit — drain+fold the inbox each round, reason,
//               persist, release. A bare wake (no seed ask) that drains whatever Submit already delivered.
//
// The fold-streaming behavior (a live instance absorbing a STREAM of asks, consolidating N into one pass) IS
// the existing RunActor loop; AgentRuntime is the bound, injectable dependency over it, not a new algorithm.

// AgentRuntime hosts agent instances under single-instance-per-unit semantics. The infra constructs ONE and
// injects it; agents depend on this interface, never the concrete store/substrate.
type AgentRuntime interface {
	// Submit is the producer front door. It durably delivers the ask into the unit's inbox (idempotency-
	// deduped), then makes the atomic wake-or-enqueue decision:
	//   - no live instance → THIS caller becomes the instance and runs it (drains the just-delivered ask +
	//     any queued) → returns the run's ActorResult (Converged / YieldedReenqueue / Error).
	//   - a live instance exists → the ask is enqueued; the live instance will fold it → returns
	//     ActorBusyDeferred (the caller is done; it did NOT run a competitor).
	// worker is the agent's per-unit reasoning seam used IF this caller becomes the instance.
	Submit(ctx context.Context, k UnitKey, ask ActorMessage, worker UnitWorker, obs Observer) (ActorResult, error)

	// RunUnit is the consumer side: be the live instance for k with NO seed ask — drain+fold whatever is
	// already in the inbox (what Submit delivered), reason, persist, release. Used by a wake consumer that
	// only carries the unit key (the asks already live in the inbox). If an instance is already live →
	// ActorBusyDeferred.
	RunUnit(ctx context.Context, k UnitKey, worker UnitWorker, obs Observer) (ActorResult, error)
}

// boundRuntime is the default AgentRuntime: an ActorStore binding + the actor config, closing over the
// existing RunActor loop. The infra calls NewRuntime with its durable store; tests/local use
// NewInMemoryRuntime. There is no other algorithm here — Submit/RunUnit are thin wrappers that make the
// store + config injectable and expose the producer (Submit) vs consumer (RunUnit) verbs.
type boundRuntime struct {
	store ActorStore
	cfg   ActorConfig
}

// NewRuntime binds any ActorStore + config into an AgentRuntime. The infra calls this with its durable
// (Postgres) store; the kit never sees the substrate.
func NewRuntime(store ActorStore, cfg ActorConfig) AgentRuntime {
	return &boundRuntime{store: store, cfg: cfg}
}

// NewInMemoryRuntime is the kit's batteries-included runtime for tests + single-process local dev: a real
// in-memory ActorStore binding behind the same interface the durable binding satisfies.
func NewInMemoryRuntime(cfg ActorConfig) AgentRuntime {
	return NewRuntime(NewInMemoryActorStore(), cfg)
}

// Submit — deliver the ask durably FIRST (so it can't be lost and the both-edges invariant holds: a
// committed ask + no live instance ⟹ an instance will start), then run the actor loop with NO seed message
// (the ask is already in the inbox). RunActorWithMessage's own acquire-else-defer makes the wake-or-enqueue
// decision atomically against the lease:
//   - acquired → drains the just-delivered ask (+ any queued) → Converged/Yielded/Error.
//   - not held (a live instance exists) → the loop defers; the ask we delivered will be folded by the live
//     instance → ActorBusyDeferred.
//
// Delivering before the loop (rather than passing the ask as the loop's seed) is deliberate: it guarantees
// the ask is durable even on the deferred path, and it keeps Submit's two responsibilities — accept + decide
// — in the right order (Contract 1 then Contract 2).
func (rt *boundRuntime) Submit(ctx context.Context, k UnitKey, ask ActorMessage, worker UnitWorker, obs Observer) (ActorResult, error) {
	if hasMessage(ask) {
		if err := rt.store.Deliver(ctx, k, ask); err != nil {
			return ActorResult{Outcome: ActorError}, err
		}
	}
	// No seed message: the ask is already delivered. RunActorWithMessage acquires-or-defers, and on acquire
	// the loop drains the inbox (which now holds our ask).
	return RunActorWithMessage(ctx, k, rt.store, worker, rt.cfg, ActorMessage{}, obs)
}

// RunUnit — be the live instance with no seed ask (a bare wake): acquire-or-defer, then drain+fold the
// inbox. Used by a wake consumer that carries only the unit key.
func (rt *boundRuntime) RunUnit(ctx context.Context, k UnitKey, worker UnitWorker, obs Observer) (ActorResult, error) {
	return RunActor(ctx, k, rt.store, worker, rt.cfg, obs)
}
