# The agent runtime

How one agent instance is executed: exactly one live run per work-unit, external changes routed into
that run rather than spawning a competitor, and a bounded loop that never wedges. This page also
states plainly what single-writer does and does not guarantee, because the honest version is narrower
than the phrase suggests.

Symbols: `RunActor`, `RunActorWithMessage`, `AgentRuntime`, `Lease`, `Inbox`, `ActorStore`,
`UnitWorker`, `InMemoryActorStore` — see [reference.md](../reference.md) for signatures.

## The problem

An autonomous agent is triggered by things it does not control: a cron sweep, a queue message, a user
editing the object it is reasoning about. Without a single-instance guarantee, two runs for the same
object reason from different snapshots and make conflicting decisions, and the later write silently
loses the earlier one's work.

The obvious fix — "check whether a run is live, then start one if not" — is a broken two-level
design. The check and the start are not atomic, so both callers can pass the check. The decision has
to be one transaction over the source of truth.

## The model

Each agent instance owns one work-unit: one planning agent to one project, one triage agent to one
ticket. `UnitKey` is that grain id, and it is simultaneously the lease key and the inbox partition.
This is the autonomous equivalent of a chat `session_id`.

Two verbs are the whole runtime API:

```go
type AgentRuntime interface {
    Submit(ctx context.Context, k UnitKey, ask ActorMessage, worker UnitWorker, obs Observer) (ActorResult, error)
    RunUnit(ctx context.Context, k UnitKey, worker UnitWorker, obs Observer) (ActorResult, error)
}
```

`Submit` is the producer front door. It delivers the ask durably **first**, then makes the atomic
wake-or-enqueue decision against the lease: no live instance, and this caller becomes the instance and
runs it; a live instance exists, and the ask is left in the inbox for that instance to fold, returning
`ActorBusyDeferred`. Delivering before the decision is deliberate — it guarantees the ask is durable
even on the deferred path.

`RunUnit` is the consumer side: be the live instance with no seed ask, draining whatever `Submit`
already delivered. A wake consumer that carries only a unit key uses this.

The caller never guesses liveness. That guess was the broken design.

## The loop

```
acquire the lease  →  not held? deliver the ask to the inbox, return BusyDeferred
each round:
  drain an inbox batch, fold it into the worker's context   # steer at the round boundary
  run one round of the agent's own loop (RunOnce / Pair / RunToolLoop)
  ack the drained batch — only now                          # ack after durable effect
  converged and inbox empty?  release, re-check, done
  rounds ≥ cap?  release + tell the caller to re-enqueue     # starvation bound
```

Four properties in that sketch are load-bearing.

**Steer at the round boundary, never mid-call.** Inbox messages are drained between rounds. A round
is an entire agent run — a multi-call pair loop under retry and throttle, so many seconds — and
interrupting one mid-call to inject a message would leave the reasoning half-formed.

**Ack after the durable effect, not before.** The ack is deliberately deferred until `RunRound`
succeeds. Acking at drain time loses the folded asks if the round then fails: the worker's `Fold` has
consumed them in memory, a failed round discards that in-memory state, and a pre-round ack would have
already removed them from the durable inbox. Leaving them un-acked means a failed round re-drains the
same batch, which is what at-least-once actually requires.

**Ack by token, never by position.** `DrainBatch` sets an opaque, binding-owned `AckToken` on each
message, and `Ack` must target exactly those rows. Acking "the oldest N pending" let a resumed zombie
ack the *new* owner's freshly-delivered rows during a lease-steal window, losing wakeups. A holder can
only ever ack what it itself drained.

**Release before the final emptiness re-check.** The converged-and-empty check and the release are not
one atomic step. A producer that delivers an ask in the gap, then acquires, gets `held=false` — we
still hold the lease — so it defers, trusting a run that is about to exit. Release-then-recheck closes
that window: after release a producer's acquire succeeds and it runs the straggler itself, and any ask
that landed during the run-then-release window is caught by the re-check and turned into
`ActorYieldedReenqueue`. Check-then-release leaves the window open.

**The starvation bound is non-negotiable.** Hitting the round cap without converging yields:
the lease is released, the caller re-enqueues a wake job, and work already done is persisted by the
worker. A hot unit never monopolises a worker.

Every exit path releases the lease, and the release runs on a **detached** context
(`context.WithoutCancel`) with its own short timeout. A durable release issued on an already-cancelled
run context is simply skipped, which would wedge the unit until its TTL lapsed — precisely on the
error path where releasing matters most.

`DefaultLeaseTTL` is 5 minutes, and it exists because a zero TTL means an instantly-expired lease. An
`ActorConfig{}` with `MaxRounds` and `DrainBatch` defaulted but `LeaseTTL` passed raw produced two
holders of one unit, silently voiding the entire point of the model. Unlike a tier, a lease TTL has a
correct conservative answer, so it is defaulted rather than rejected.

## What single-writer actually guarantees

⚠️ **The guarantee is mutual exclusion plus an idempotent write. It is not fence-rejected zombie
writes.** Be precise about this before you rely on it.

Failure handling splits into two layers with two owners.

**Guard 1 — the control loop degrades gracefully (this framework).** A run never wedges: outcomes for
silent, stalemate and error paths; the starvation bound; ack-after-durable-effect; the detached
release; the in-round heartbeat. Built and tested.

**Guard 2 — detect and stop a superseded instance (your hosting).** A crash lets the TTL lapse so
another worker steals the lease. A *zombie* — a process that froze past its TTL, was stolen from, then
thawed — is caught by the in-round heartbeat: `Heartbeat` returns `ErrLeaseLost` on a fence mismatch,
which cancels the round context, so the superseded run's in-flight work unwinds, it commits and acks
nothing, and at-least-once redelivery re-drives a clean redo against the new owner.

Guard 2 is an **efficiency** guard — it stops wasted model spend and frees a worker slot. It is not a
correctness guarantee, and saying so plainly matters: a frozen process runs no canceller, and a
cooperative cancel cannot stop a write already mid-commit.

Correctness comes from elsewhere. **Mutual exclusion** is real: at most one live run per unit, fleet
wide, via the lease compare-and-set. **Conflicting-write safety** is your idempotent or
last-writer-wins durable write. The reference consumer's archive-singleton — one live record per
project, a newer one archiving the older — makes a duplicate zombie write harmless.

The `Fence` is a **lease-steal detection token, not a write-conditioner**. `Acquire` bumps it on a
steal; `Heartbeat` reports `ErrLeaseLost` when it no longer matches. It is threaded to `RunRound` so
an agent *may* condition its own write on it, but the kit does not, and dead fence plumbing that
implied otherwise was removed because it read as protection that was not there.

Why not just build the fence-conditioned write? Because for an idempotent or last-writer-wins write, a
lock is an efficiency optimisation rather than a correctness requirement (Kleppmann, *How to Do
Distributed Locking*). Fencing such a write would only save redundant work. A true fence-rejected
write — `UPDATE … WHERE fence < :n` — is the thing to add if and only if a non-idempotent write
appears. If yours is not idempotent, either make it so or add that conditional write.

**The trade-off is not strictly one-directional, and it should not be oversold.** Replacing a coarse
but *present* write guard (a singleton that dedups even a true concurrent double-write at the database)
with mutual exclusion plus an unwired fence means that in the narrow stolen-lease-zombie window — a GC
pause or partition exceeding the TTL, now made small by the in-round heartbeat — there is *less* write
protection than the singleton alone gave. Net: clearly better on the common path (no duplicate runs, no
wasted model spend), with one small uncovered window.

There is a related subtlety worth stating: inbox idempotency dedups only *pending* rows. Once a trigger
is drained and acked, an at-least-once redelivery re-inserts it and the round runs again. The thing
that stops that re-run creating a duplicate record is the consumer's idempotent write — so in that
reference consumer, the archive-singleton was retired as the *single-writer* mechanism but retained as
the *redelivery-dedup* mechanism. It is a live dependency, not a vestige.

## Queue versus store

They have different jobs and are not redundant.

The **queue is intake**: it carries the trigger durably, with retry and a dead-letter path. It can be
sloppy. The **durable store is the source of truth**: the lease row and inbox rows, in one transaction,
are the ask stream, the singleton, and the atomic decision. It is exact. A redelivered message re-runs
`Submit`, whose insert is a no-op on conflict.

⚠️ **The kit ships no queue integration.** Receiving a trigger, acking it, and releasing on a throttle
are yours. The kit's contribution is `ErrThrottled` and the actor contracts; wiring them to a queue is
hosting work.

A FIFO queue was evaluated as the lock and rejected: per-group in-flight messages release when the
visibility timeout expires, so a run lasting tens of seconds can double-fire. It is an ordering
primitive, not a lock.

## The framework/runtime boundary

Cognition belongs to the framework and the agent. Hosting — identity, lifecycle, single-instance,
inbox, durability, wake, observability — belongs to the runtime. Surveying the market makes the line
obvious: some hosted runtimes pin a microVM per session, several agent SDKs never lock at all, others
run stateless against an external store.

A framework can *assume* a single writer. Only a runtime can *guarantee* one. So agentkit defines
`Lease`, `Inbox`, `ActorStore` and `AgentRuntime`, and your hosting binding provides the guarantee.

`InMemoryActorStore` ships here — a real store with the same lease semantics (TTL-aware heartbeat,
strictly monotonic fence) — for tests and single-process local development. A durable binding is
yours. The one this was extracted from is Postgres-backed: a `unit_lease` and `unit_inbox` table pair
behind the same interface, injected via `NewRuntime`. That a real durable binding existed against this
exact interface is the evidence the seam is genuine rather than aspirational.

At scale every race anchors on the single-row lease compare-and-set. Concurrent submits resolve to
exactly one winner; a crash lapses the TTL and the lease is stolen with the fence bumped; a hot unit
hits the starvation bound. Different units run fully parallel on distinct rows; the same unit is
serialised by its row.

## What is not built here

- **A fence-guarded durable write.** Deferred, as argued above — it needs fence-aware writes on your
  side, and buys efficiency rather than correctness for an idempotent write.
- **A session advisory lock instead of a TTL lease.** It would *eliminate* the freeze-zombie rather
  than mitigate it — a frozen-but-connected worker is never stolen from — but it pins a connection and
  reworks the binding. Recorded, deferred.
- **`EventLog`.** Cut, not deferred. An agent re-gathers fresh from its owning backend each run, so the
  durable store plus the agent's persisted output already are the state. An event log would duplicate
  it. Nothing plans to build it unless a consumer needs deterministic event replay.
- **The full generic `AgentMessage`** with typed intents and payloads. `ActorMessage` is the v1
  envelope; its `Intent` is context that biases the worker's fold, never control flow — no `switch`
  on it inside the loop.
- **Alternative hosting bindings.** Designed for via swappability, not built.

## See also

- [architecture.md](architecture.md) — the design principles, including where this one sits
- [building-an-agent.md](../guides/building-an-agent.md) — hosting a unit-scoped agent
- [reference.md](../reference.md) — signatures and traps
- [example/](../../example/) — reference agents wired end to end
