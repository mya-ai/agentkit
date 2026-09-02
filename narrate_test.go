package agentkit

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// recordingSink captures every WriteDoc as a deep copy (the Narrator mutates its doc in place, so we must
// snapshot). It signals each write on a channel so a test can wait for the doc to reach a state.
type recordingSink struct {
	mu     sync.Mutex
	docs   []NarrationDoc
	writes chan struct{}
}

func newRecordingSink() *recordingSink { return &recordingSink{writes: make(chan struct{}, 256)} }

func (r *recordingSink) WriteDoc(doc NarrationDoc) {
	cp := NarrationDoc{Phase: doc.Phase, Entries: append([]NarrationEntry(nil), doc.Entries...)}
	r.mu.Lock()
	r.docs = append(r.docs, cp)
	r.mu.Unlock()
	select {
	case r.writes <- struct{}{}:
	default:
	}
}

// last returns the most recent doc snapshot (or a zero doc).
func (r *recordingSink) last() NarrationDoc {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.docs) == 0 {
		return NarrationDoc{}
	}
	return r.docs[len(r.docs)-1]
}

// waitForWrite blocks until the sink records a write (or times out) — used to synchronize on the async
// goroutine without sleeping.
func (r *recordingSink) waitForWrite(t *testing.T) {
	t.Helper()
	select {
	case <-r.writes:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a WriteDoc")
	}
}

// funcSummarizer adapts a func to NarrationSummarizer.
type funcSummarizer func(ctx context.Context, s NarrationSummary) string

func (f funcSummarizer) Summarize(ctx context.Context, s NarrationSummary) string { return f(ctx, s) }

// TestNarrator_NilIsNoOp — every method on a nil *Narrator is safe (an agent with no narrator is unchanged).
func TestNarrator_NilIsNoOp(t *testing.T) {
	var n *Narrator
	// none of these panic
	n.Start(context.Background())
	n.Emit(Event{Kind: EventTurnStart})
	n.SetLabels([]string{"x"})
	n.Close()
	// NewNarrator with a nil sink returns nil (so a consumer without a sink stays a no-op).
	if got := NewNarrator(nil, nil, nil); got != nil {
		t.Fatal("NewNarrator(nil sink) must return nil")
	}
}

// TestNarrator_TurnStartAppendsActive_TurnEndSettles — the core append/settle lifecycle + stable ids.
func TestNarrator_TurnStartAppendsActive_TurnEndSettles(t *testing.T) {
	sink := newRecordingSink()
	n := NewNarrator(sink, nil, nil, WithHeartbeat(time.Hour)) // heartbeat effectively off
	n.Start(context.Background())

	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1})
	sink.waitForWrite(t)
	d := sink.last()
	if len(d.Entries) != 1 || d.Entries[0].State != StateActive {
		t.Fatalf("turn_start must append one active entry, got %+v", d.Entries)
	}
	firstID := d.Entries[0].ID
	if firstID == "" {
		t.Fatal("entry must have a stable id")
	}

	n.Emit(Event{Kind: EventTurnEnd, Role: "executor", Round: 1})
	sink.waitForWrite(t)
	d = sink.last()
	if d.Entries[0].State != StateDone {
		t.Fatalf("turn_end must settle the active entry to done, got %q", d.Entries[0].State)
	}

	// A second turn appends a NEW entry with a NEW id (append, not revise).
	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 2})
	sink.waitForWrite(t)
	d = sink.last()
	if len(d.Entries) != 2 || d.Entries[1].ID == firstID {
		t.Fatalf("second turn must append a new entry with a new id, got %+v", d.Entries)
	}
	n.Close()
}

// TestNarrator_HeartbeatRevisesActiveInPlace — the pacer revises the ACTIVE entry (same id) on each tick;
// the token-less gap animates without appending duplicate entries.
func TestNarrator_HeartbeatRevisesActiveInPlace(t *testing.T) {
	sink := newRecordingSink()
	pacer := func(phase string, idx int) string {
		return "still working " + phase + " #" + strconv.Itoa(idx)
	}
	n := NewNarrator(sink, nil, pacer, WithHeartbeat(10*time.Millisecond))
	n.Start(context.Background())

	n.Emit(Event{Kind: EventPhase, Phase: "planning"})
	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1})
	sink.waitForWrite(t) // the turn-start append

	// Wait for at least two heartbeat revisions.
	deadline := time.After(2 * time.Second)
	var id string
	revised := 0
	for revised < 2 {
		select {
		case <-sink.writes:
			d := sink.last()
			if len(d.Entries) == 1 && d.Entries[0].State == StateActive {
				if id == "" {
					id = d.Entries[0].ID
				}
				if d.Entries[0].ID == id { // same id → revise in place
					revised++
				}
			}
			if len(d.Entries) > 1 {
				t.Fatalf("heartbeat must REVISE the active entry, not append; got %d entries", len(d.Entries))
			}
		case <-deadline:
			t.Fatalf("timed out waiting for heartbeat revisions (got %d)", revised)
		}
	}
	n.Close()
}

// TestNarrator_SummarizerRevisesActive — an async summarizer result revises the active entry's detail.
func TestNarrator_SummarizerRevisesActive(t *testing.T) {
	sink := newRecordingSink()
	sum := funcSummarizer(func(_ context.Context, _ NarrationSummary) string {
		return "the agent is tightening the plan"
	})
	n := NewNarrator(sink, sum, nil, WithHeartbeat(10*time.Millisecond), WithMaxSummarizerCalls(4))
	n.Start(context.Background())

	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1})
	n.Emit(Event{Kind: EventCritique, Role: "judge", Round: 1, Context: []string{"missing the LOV step"}})

	// Wait until the active entry's detail becomes the summarizer output.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-sink.writes:
			d := sink.last()
			if len(d.Entries) == 1 && d.Entries[0].Detail == "the agent is tightening the plan" {
				n.Close()
				return
			}
		case <-deadline:
			t.Fatalf("summarizer output never revised the active entry; last=%+v", sink.last())
		}
	}
}

// TestNarrator_StaleSummaryDoesNotReviseNewerEntry — a summary computed for entry e1 must NOT overwrite a
// DIFFERENT entry that became active after a turn boundary (the round-1-summary-onto-round-2-entry bug).
func TestNarrator_StaleSummaryDoesNotReviseNewerEntry(t *testing.T) {
	sink := newRecordingSink()
	// A summarizer that SIGNALS when invoked, then BLOCKS until released — so the test can deterministically
	// (1) confirm it dispatched while round-1's entry was active, (2) force a turn boundary, (3) release the
	// now-stale result.
	dispatched := make(chan struct{}, 1)
	release := make(chan string)
	sum := funcSummarizer(func(_ context.Context, _ NarrationSummary) string {
		dispatched <- struct{}{}
		return <-release
	})
	n := NewNarrator(sink, sum, nil, WithHeartbeat(10*time.Millisecond), WithMaxSummarizerCalls(4))
	n.Start(context.Background())

	// Round 1: open an entry + give the summarizer signal so a heartbeat dispatches it (against e1).
	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1})
	n.Emit(Event{Kind: EventCritique, Context: []string{"round-1 signal"}})
	// Wait until the summarizer is ACTUALLY dispatched (against round-1's entry) before advancing.
	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("summarizer never dispatched")
	}
	// Advance to round 2 (settles e1, opens e2) while the summarizer is blocked.
	n.Emit(Event{Kind: EventTurnEnd, Role: "executor", Round: 1})
	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 2})
	sink.waitForWrite(t)
	r2ID := ""
	for deadline := time.After(time.Second); r2ID == ""; {
		select {
		case <-sink.writes:
			if d := sink.last(); len(d.Entries) == 2 && d.Entries[1].State == StateActive {
				r2ID = d.Entries[1].ID
			}
		case <-deadline:
			t.Fatal("round-2 active entry never appeared")
		}
	}
	// NOW release the stale round-1 summary; it must NOT land on the round-2 entry.
	release <- "STALE round-1 summary"
	time.Sleep(60 * time.Millisecond)

	d := sink.last()
	if len(d.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(d.Entries))
	}
	if d.Entries[1].ID != r2ID {
		t.Fatalf("round-2 entry id changed unexpectedly")
	}
	if d.Entries[1].Detail == "STALE round-1 summary" {
		t.Fatal("a stale summary must not overwrite the newer active entry")
	}
	n.Close()
}

// TestNarrator_SummarizerDirtyCheck — the summarizer does NOT re-fire on unchanged signal: after one
// dispatch, further heartbeats over the SAME buffer make no new calls (until new signal arrives).
func TestNarrator_SummarizerDirtyCheck(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	sum := funcSummarizer(func(_ context.Context, _ NarrationSummary) string {
		mu.Lock()
		calls++
		mu.Unlock()
		return ""
	})
	sink := newRecordingSink()
	n := NewNarrator(sink, sum, nil, WithHeartbeat(5*time.Millisecond), WithMaxSummarizerCalls(4))
	n.Start(context.Background())
	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1})
	n.Emit(Event{Kind: EventCritique, Context: []string{"one signal"}}) // signal arrives ONCE
	time.Sleep(120 * time.Millisecond)                                  // many heartbeats over the SAME buffer
	n.Close()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("with no NEW signal, the summarizer must fire once, not once per heartbeat; got %d", got)
	}
}

// TestNarrator_PacerStandsDownAfterSummarizer — the pacer is a FALLBACK, not the voice: once the summarizer
// revises the active entry, later heartbeats must NOT overwrite it with the pacer placeholder.
func TestNarrator_PacerStandsDownAfterSummarizer(t *testing.T) {
	sink := newRecordingSink()
	sum := funcSummarizer(func(_ context.Context, _ NarrationSummary) string { return "REAL summarizer voice" })
	pacer := func(_ string, _ int) string { return "placeholder" }
	n := NewNarrator(sink, sum, pacer, WithHeartbeat(5*time.Millisecond), WithMaxSummarizerCalls(5))
	n.Start(context.Background())

	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1})
	n.Emit(Event{Kind: EventCritique, Context: []string{"signal"}}) // gives the summarizer something to say

	// Wait until the summarizer line lands.
	deadline := time.After(2 * time.Second)
	for sink.last().Entries == nil || activeDetail(sink.last()) != "REAL summarizer voice" {
		select {
		case <-sink.writes:
		case <-deadline:
			t.Fatalf("summarizer voice never landed; last=%q", activeDetail(sink.last()))
		}
	}
	// Let several more heartbeats run — the pacer must NOT clobber the summarizer line back to "placeholder".
	time.Sleep(40 * time.Millisecond)
	if got := activeDetail(sink.last()); got != "REAL summarizer voice" {
		t.Fatalf("pacer clobbered the summarizer voice: got %q", got)
	}
	n.Close()
}

// activeDetail returns the active entry's detail (or the tail entry's) from a doc snapshot.
func activeDetail(d NarrationDoc) string {
	for i := len(d.Entries) - 1; i >= 0; i-- {
		if d.Entries[i].State == StateActive {
			return d.Entries[i].Detail
		}
	}
	if n := len(d.Entries); n > 0 {
		return d.Entries[n-1].Detail
	}
	return ""
}

// TestNarrator_SummarizerFailureDegrades — a summarizer returning "" (error/empty) never breaks the run; the
// active entry keeps its templated line.
func TestNarrator_SummarizerFailureDegrades(t *testing.T) {
	sink := newRecordingSink()
	sum := funcSummarizer(func(_ context.Context, _ NarrationSummary) string { return "" }) // always empty
	pacer := func(_ string, _ int) string { return "templated line" }
	n := NewNarrator(sink, sum, pacer, WithHeartbeat(10*time.Millisecond), WithMaxSummarizerCalls(4))
	n.Start(context.Background())

	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1, Text: "seed"})
	n.Emit(Event{Kind: EventCritique, Context: []string{"x"}})
	sink.waitForWrite(t)

	// Give a few heartbeats a chance; the detail must be the templated line, never blank.
	time.Sleep(60 * time.Millisecond)
	d := sink.last()
	if len(d.Entries) == 0 || d.Entries[0].Detail == "" {
		t.Fatalf("empty summarizer must degrade to the templated line, got %+v", d.Entries)
	}
	n.Close()
}

// TestNarrator_SummarizerCostCap — the summarizer is called at most MaxSummarizerCalls times per run.
func TestNarrator_SummarizerCostCap(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	sum := funcSummarizer(func(_ context.Context, _ NarrationSummary) string {
		mu.Lock()
		calls++
		mu.Unlock()
		return "" // don't revise; we only count calls
	})
	sink := newRecordingSink()
	n := NewNarrator(sink, sum, nil, WithHeartbeat(5*time.Millisecond), WithMaxSummarizerCalls(2))
	n.Start(context.Background())
	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1})
	n.Emit(Event{Kind: EventCritique, Context: []string{"signal"}})

	time.Sleep(120 * time.Millisecond) // many heartbeats would fire uncapped
	n.Close()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got > 2 {
		t.Fatalf("summarizer must be capped at 2 calls/run, got %d", got)
	}
}

// TestNarrator_CloseSettlesAndFlushes — Close settles the active entry to done and blocks until the
// goroutine exits (so a following apply write doesn't race).
func TestNarrator_CloseSettlesAndFlushes(t *testing.T) {
	sink := newRecordingSink()
	n := NewNarrator(sink, nil, nil, WithHeartbeat(time.Hour))
	n.Start(context.Background())
	n.Emit(Event{Kind: EventTurnStart, Role: "executor", Round: 1})
	sink.waitForWrite(t)

	n.Close()
	d := sink.last()
	if len(d.Entries) != 1 || d.Entries[0].State != StateDone {
		t.Fatalf("Close must settle the active entry to done, got %+v", d.Entries)
	}
	// Close is idempotent / safe to call again.
	n.Close()
}

// TestNarrator_ActionAppendsSettled — an action event appends a settled entry (not active).
func TestNarrator_ActionAppendsSettled(t *testing.T) {
	sink := newRecordingSink()
	n := NewNarrator(sink, nil, nil, WithHeartbeat(time.Hour))
	n.Start(context.Background())
	n.Emit(Event{Kind: EventAction, Action: "created_tasks", Text: "created 6 tasks"})
	sink.waitForWrite(t)
	d := sink.last()
	if len(d.Entries) != 1 || d.Entries[0].State != StateDone || d.Entries[0].Action != "created_tasks" {
		t.Fatalf("action must append a settled entry, got %+v", d.Entries)
	}
	n.Close()
}
