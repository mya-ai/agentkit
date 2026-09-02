package agentkit

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// InMemoryActorStore is the v1 ActorStore binding: an in-process lease + inbox. It is a REAL binding (not a
// stub) — it is what unit tests and single-process local dev use, and it implements the same contract the
// durable Postgres/queue binding will, so the actor loop never changes when the durable binding swaps in. It
// is concurrency-safe (a sync.Mutex) so concurrent RunActor calls for different units — and the
// single-writer race for the SAME unit — behave correctly in-process.
//
// What it does NOT provide (by design, deferred to the durable binding): cross-process leasing (this is
// in-process only) and durable inbox persistence. Those matter only across processes/restarts — exactly
// where the Postgres binding lands.
//
// Fence semantics MATCH the durable binding's GUARANTEE (strict monotonicity) by a DIFFERENT mechanism: the
// durable binding keeps the lease row and bumps fence on ON CONFLICT; this binding uses a PROCESS-GLOBAL
// nextFence counter that only ever increments, so even though Release deletes the map entry, a re-acquire
// gets a fresh HIGHER fence — never the reset-to-1 the durable DELETE bug had (review #1). So both bindings
// honor "fence never repeats"; tests on this binding therefore exercise the same monotonic-fence contract.
type InMemoryActorStore struct {
	mu        sync.Mutex
	leases    map[UnitKey]leaseState
	inbox     map[UnitKey][]ActorMessage
	nextFence uint64
	nextSeq   uint64 // monotonic AckToken source — mirrors the durable binding's row id so Ack targets exact rows
}

type leaseState struct {
	held    bool
	fence   uint64
	expires time.Time
	ttl     time.Duration // the acquire-time TTL, so Heartbeat extends by the SAME window the durable binding does
}

// NewInMemoryActorStore returns a ready in-memory ActorStore.
func NewInMemoryActorStore() *InMemoryActorStore {
	return &InMemoryActorStore{
		leases: make(map[UnitKey]leaseState),
		inbox:  make(map[UnitKey][]ActorMessage),
	}
}

// Acquire grants the lease iff no live (unexpired) lease exists for k. An expired lease (TTL passed without
// a heartbeat — a crashed holder) is taken over with a NEW fence, so a stale holder's later Heartbeat/
// Release no-ops (fence mismatch).
func (s *InMemoryActorStore) Acquire(_ context.Context, k UnitKey, ttl time.Duration) (bool, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ls := s.leases[k]
	if ls.held && nowFunc().Before(ls.expires) {
		return false, 0, nil // a run is live
	}
	s.nextFence++
	s.leases[k] = leaseState{held: true, fence: s.nextFence, expires: nowFunc().Add(ttl), ttl: ttl}
	return true, s.nextFence, nil
}

// Heartbeat extends the lease by its ACQUIRE-TIME TTL iff the caller still holds it (fence matches) — matching
// the durable binding's semantics so a test that pins a short LeaseTTL to exercise expiry/takeover sees the
// SAME timing in-memory (review #3: a hardcoded 1m extension silently defeated such tests). A stale holder
// (fence mismatch / no live lease) was stolen from: Heartbeat returns ErrLeaseLost — the GUARD 2 signal that
// startHeartbeat uses to cancelRound() the superseded run so it stops wasting work. This mirrors the durable
// binding's fence-mismatched UPDATE (0 rows affected → ErrLeaseLost). NOTE for maintainers: the return on
// mismatch MUST be ErrLeaseLost, not nil — a nil return would silently disable Guard 2 detect-and-kill
// (startHeartbeat keys on errors.Is(err, ErrLeaseLost)).
func (s *InMemoryActorStore) Heartbeat(_ context.Context, k UnitKey, fence uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ls := s.leases[k]
	if ls.held && ls.fence == fence {
		ls.expires = nowFunc().Add(ls.ttl)
		s.leases[k] = ls
		return nil
	}
	// Fence mismatch (or no live lease): this holder was stolen from. Report ErrLeaseLost so the loop's
	// heartbeat cancels the superseded round (Guard 2). Mirrors the durable binding's 0-rows-affected.
	return ErrLeaseLost
}

// Release frees the lease iff the caller holds it (fence matches) — a stale holder's release is a no-op, so
// it can't free a lease another run took over.
func (s *InMemoryActorStore) Release(_ context.Context, k UnitKey, fence uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ls := s.leases[k]; ls.held && ls.fence == fence {
		delete(s.leases, k)
	}
	return nil
}

// Deliver appends a message to the unit's inbox, deduping on a non-empty IdempotencyKey (a redelivered
// trigger lands once).
func (s *InMemoryActorStore) Deliver(_ context.Context, k UnitKey, msg ActorMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.IdempotencyKey != "" {
		for _, existing := range s.inbox[k] {
			if existing.IdempotencyKey == msg.IdempotencyKey {
				return nil // already queued — dedup
			}
		}
	}
	// Stamp a monotonic AckToken (mirrors the durable binding's row id) so Ack targets EXACTLY the drained
	// rows, never "the front N" — the same single-writer safety the Postgres binding has (review #2).
	s.nextSeq++
	msg.AckToken = strconv.FormatUint(s.nextSeq, 10)
	s.inbox[k] = append(s.inbox[k], msg)
	return nil
}

// DrainBatch returns up to max messages from the front of the inbox WITHOUT removing them — they are removed
// on Ack (at-least-once: a crash between drain and ack redelivers). This matches the durable binding's
// drain→process→ack contract.
func (s *InMemoryActorStore) DrainBatch(_ context.Context, k UnitKey, maxN int) ([]ActorMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.inbox[k]
	if len(q) > maxN {
		q = q[:maxN]
	}
	out := make([]ActorMessage, len(q))
	copy(out, q)
	return out, nil
}

// Ack removes EXACTLY the given messages (by AckToken) from the inbox — never "the front N". Acking by
// token means a stale holder can only ever remove rows IT drained, never a new owner's freshly-delivered
// rows in the lease-steal window (review #2; matches the durable binding's id = ANY semantics).
func (s *InMemoryActorStore) Ack(_ context.Context, k UnitKey, msgs []ActorMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(msgs) == 0 {
		return nil
	}
	ackTokens := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		if m.AckToken != "" {
			ackTokens[m.AckToken] = true
		}
	}
	if len(ackTokens) == 0 {
		return nil
	}
	q := s.inbox[k]
	kept := q[:0:0] // a fresh slice (don't alias q's backing array)
	for _, m := range q {
		if !ackTokens[m.AckToken] {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		delete(s.inbox, k)
	} else {
		s.inbox[k] = kept
	}
	return nil
}

// Empty reports whether the unit's inbox has no pending messages.
func (s *InMemoryActorStore) Empty(_ context.Context, k UnitKey) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inbox[k]) == 0, nil
}
