package agentkit

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// narrate.go is the reusable AGENT ACTIVITY NARRATION pipeline — the framework capability that lets an
// autonomous agent tell the user "here's what I'm doing right now" WHILE it runs, so a long
// (~2-minute) LLM loop feels alive instead of frozen. Every agent reuses it; a planning agent was the
// first consumer.
//
// THE MODEL (not streaming). The output is a MUTABLE, entry-based DOCUMENT (NarrationDoc) the consumer
// persists somewhere the UI already polls (the reference agent wrote it into a JSON metadata column on
// the object's row; the front end's existing ~3s poll delivered it). "Narration" = the Narrator keeps
// REWRITING that document — APPENDing new entries AND REVISING the current ("active") entry's text in
// place — and hands the whole document to the consumer's sink on every change. There is no token stream
// (the LLM transports are unary); the liveness comes from (a) turn-boundary events, (b) a heartbeat that
// paces the active entry through the token-less gap, and (c) an async summarizer that revises the active
// entry into warmer/specific text off the critical path.
//
// THE ENTRY CONTRACT. Each entry carries a STABLE id: the UI merges/dedupes on it. Revise-in-place keeps
// the same id (the UI updates that row); append mints a new id. State ("active"|"done") is an OPTIONAL
// render hint (shimmer the active one) — a UI that ignores it still renders correctly by merging on id +
// ordering by At.
//
// OWNERSHIP. The kit owns the Narrator (the doc, the pacing, the async-summarizer loop) and the
// Event/seam model; it NEVER writes durable state. The consumer supplies the WRITE (NarrationSink), the
// honest phase/mode line text, and the summarizer (prompt+model). A nil *Narrator is a no-op everywhere,
// so an agent that doesn't narrate is completely unchanged.

// NarrationEntry is one line in the activity document. ID is stable across flushes (the UI's merge key);
// At is an absolute RFC3339 timestamp; Action is a machine-stable verb (for icons/grouping); Detail is the
// human line; State is "active" (still happening, text may change) or "done" (settled) — an OPTIONAL UI
// hint. Only the last/current entry is ever "active".
type NarrationEntry struct {
	ID     string `json:"id"`
	At     string `json:"at"`
	Action string `json:"action"`
	Detail string `json:"detail"`
	State  string `json:"state,omitempty"`
}

// Entry states.
const (
	StateActive = "active" // still happening; Detail may be revised in place (same id)
	StateDone   = "done"   // settled; Detail is final
)

// NarrationDoc is the whole mutable document the Narrator owns and the sink persists verbatim. Phase is the
// consumer's coarse phase label (opaque to the kit). Entries are ordered oldest→newest.
type NarrationDoc struct {
	Phase   string
	Entries []NarrationEntry
}

// NarrationSink is the consumer's WRITE — Go owns persistence, the kit never touches durable state. The
// consumer maps the doc onto its surface and flushes (an activity stream → your object's metadata). Called
// on every document mutation; must be cheap/idempotent and swallow its own errors (best-effort — a failed
// flush must never fail the run).
type NarrationSink interface {
	WriteDoc(doc NarrationDoc)
}

// NarrationSummarizer turns the run's accumulated raw signal into ONE warm, honest, present-tense user
// line, used to REVISE the active entry's detail. The consumer owns the prompt + model (kept out of the
// kit). Returning "" means "keep the current line" (the graceful-degrade path). It runs OFF the critical
// path (a detached goroutine with its own timeout); it must never block the agent's run.
type NarrationSummarizer interface {
	Summarize(ctx context.Context, s NarrationSummary) string
}

// NarrationSummary is the raw signal handed to the summarizer: the current phase, the live modes/labels,
// the reviewer's latest critique lines (softened by the summarizer, never shown verbatim), and the recent
// action details. Honest inputs → an honest line.
type NarrationSummary struct {
	Phase    string
	Labels   []string // e.g. the live modes ("planning","tracking")
	Critique []string // the reviewer's feedback items — the summarizer SOFTENS these, never quotes them
	Actions  []string // recent action detail lines
}

// Event is a producer signal — honest data, not a rendered string. Producers (the loop, the consumer's
// apply path) call Narrator.Emit; the Narrator turns events into document mutations.
type Event struct {
	Kind    string   // EventTurnStart | EventTurnEnd | EventCritique | EventPhase | EventAction
	Role    string   // "executor" | "judge" (for turn events)
	Round   int      // 1-based round (for turn events)
	Phase   string   // consumer phase label (for EventPhase; opaque to the kit)
	Action  string   // machine verb (for EventAction)
	Text    string   // the human/raw line (EventAction detail; a turn-start seed line)
	Context []string // structured context (critique feedback items, mode labels)
}

// Event kinds.
const (
	EventTurnStart = "turn_start" // an LLM turn is ABOUT to run (opens a fresh active entry)
	EventTurnEnd   = "turn_end"   // an LLM turn finished (settles the active entry)
	EventCritique  = "critique"   // the reviewer's feedback — enriches the summary buffer ONLY (never its own entry)
	EventPhase     = "phase"      // the consumer advanced the coarse phase
	EventAction    = "action"     // a concrete action happened (append a settled entry)
)

// NarratingObserver EXTENDS the loop's Observer with a narration seam. The loop already takes an optional
// Observer; a consumer whose Observer ALSO implements NarratingObserver gets turn/critique events for free,
// and every existing Observer (digest agents, tests) is byte-for-byte unchanged (the loop type-asserts). Kept as
// an embed so the metrics seam and the narration seam travel together on the one observer the loop holds.
type NarratingObserver interface {
	Observer
	Narrate(ev Event)
}

// emitNarration is the loop's nil-safe dispatch: if the (optional) observer implements NarratingObserver,
// forward the event; otherwise no-op. This is the ONLY coupling between the loop and the narrator.
func emitNarration(obs Observer, ev Event) {
	if n, ok := obs.(NarratingObserver); ok && n != nil {
		n.Narrate(ev)
	}
}

// NarratorConfig tunes pacing + cost. Zero value is sensible (defaults applied in NewNarrator).
type NarratorConfig struct {
	Heartbeat          time.Duration // how often the active entry is refreshed/advanced (default 12s)
	MaxSummarizerCalls int           // per-run cap on async summarizer calls (default 4; 0 disables the summarizer)
	SummarizeTimeout   time.Duration // per-call timeout for the async summarizer (default 3s)
	BufferSize         int           // event channel buffer (default 64); Emit is non-blocking (drops when full)
}

func (c NarratorConfig) withDefaults() NarratorConfig {
	if c.Heartbeat <= 0 {
		c.Heartbeat = 12 * time.Second
	}
	if c.SummarizeTimeout <= 0 {
		c.SummarizeTimeout = 3 * time.Second
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 64
	}
	// MaxSummarizerCalls: leave as-is (0 legitimately disables the summarizer; negative clamped to 0).
	if c.MaxSummarizerCalls < 0 {
		c.MaxSummarizerCalls = 0
	}
	return c
}

// Opt configures a Narrator.
type Opt func(*NarratorConfig)

// WithHeartbeat sets the active-entry refresh cadence.
func WithHeartbeat(d time.Duration) Opt { return func(c *NarratorConfig) { c.Heartbeat = d } }

// WithMaxSummarizerCalls caps the async summarizer calls per run (0 disables it).
func WithMaxSummarizerCalls(n int) Opt { return func(c *NarratorConfig) { c.MaxSummarizerCalls = n } }

// WithSummarizeTimeout bounds each async summarizer call.
func WithSummarizeTimeout(d time.Duration) Opt {
	return func(c *NarratorConfig) { c.SummarizeTimeout = d }
}

// narratorState is the document + summary buffer the goroutine mutates. Kept private; only the goroutine
// touches it (no lock needed on the state itself — Emit sends over a channel).
type narratorState struct {
	doc      NarrationDoc
	seq      int      // monotonic id counter (stable ids: "e1","e2",…)
	labels   []string // current labels (modes) for the summarizer
	critique []string // accumulated reviewer critique (softened by the summarizer)
	actions  []string // recent action details for the summarizer
	calls    int      // summarizer calls made this run (cost cap)
	// lastSummarizedLen is the total signal count (critique+actions+labels) at the last summarizer dispatch;
	// the heartbeat only re-summarizes when the count GREW (new signal), not on every tick over stale input.
	lastSummarizedLen int
	// activeSummarized is true once the SUMMARIZER (the real voice) has revised the CURRENT active entry —
	// while set, the pacer (a mere placeholder) must NOT overwrite it. Reset when a new active entry opens.
	activeSummarized bool
	// paceIdx advances the heartbeat's "still working" variant on the active entry.
	paceIdx int
}

// Narrator owns the mutable document and the async pacing/summarizer loop for ONE run. Construct with
// NewNarrator, Start() to launch the goroutine, Emit() from producers (non-blocking), Close() when the run
// ends (drains, settles the active entry to done, final flush). A nil *Narrator is a no-op everywhere.
type Narrator struct {
	sink NarrationSink
	sum  NarrationSummarizer
	cfg  NarratorConfig

	events chan Event
	// summaries carries an async summarizer result back to the goroutine — the revised text AND the id of
	// the entry it was computed for (so a stale summary landing after a turn boundary doesn't revise a
	// newer, different entry).
	summaries chan summaryResult
	done      chan struct{}
	stopped   chan struct{} // closed by the goroutine on exit; Close() waits on it
	closeOnce sync.Once
	// pacer maps (phase, paceIdx) → a templated "still working" line; supplied by the consumer so the kit
	// carries no domain vocabulary. nil → the heartbeat only re-times the active entry (no text change).
	pacer func(phase string, idx int) string
}

// NewNarrator builds a run's narrator. sink is required (nil → the returned narrator is a no-op). sum may be
// nil (no summarizer — the heartbeat + templated pacer still animate the active entry). pacer supplies the
// consumer's honest "still working" variants keyed on (phase, idx); nil → no heartbeat text change.
func NewNarrator(sink NarrationSink, sum NarrationSummarizer, pacer func(phase string, idx int) string, opts ...Opt) *Narrator {
	if sink == nil {
		return nil
	}
	cfg := NarratorConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	cfg = cfg.withDefaults()
	return &Narrator{
		sink:      sink,
		sum:       sum,
		cfg:       cfg,
		pacer:     pacer,
		events:    make(chan Event, cfg.BufferSize),
		summaries: make(chan summaryResult, 4),
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
}

// nowRFC3339 is the narrator's clock (indirected for tests). Absolute RFC3339 UTC — no relative dates.
var nowRFC3339 = func() string { return nowFunc().UTC().Format(time.RFC3339) }

// Start launches the narration goroutine (heartbeat + event processing). Idempotent-safe to call once per
// run. nil-safe.
func (n *Narrator) Start(ctx context.Context) {
	if n == nil {
		return
	}
	go n.run(ctx)
}

// Emit hands a producer event to the narrator. NON-BLOCKING: if the buffer is full the event is dropped
// (narration is best-effort — never apply backpressure to the agent's run). nil-safe.
func (n *Narrator) Emit(ev Event) {
	if n == nil {
		return
	}
	select {
	case n.events <- ev:
	default: // buffer full — drop (best-effort)
	}
}

// SetLabels records the run's current labels (e.g. the live modes) for the summarizer. nil-safe.
func (n *Narrator) SetLabels(labels []string) {
	if n == nil {
		return
	}
	n.Emit(Event{Kind: EventPhase, Context: labels})
}

// closeWaitBound caps how long Close() waits for the goroutine to finish its final flush — defense in
// depth so Close() (and thus the agent's run) can NEVER hang on a stuck sink, even if the sink ignores its
// own write deadline. On timeout we return anyway; the goroutine is detached and will exit when the flush
// unblocks (its result is then irrelevant — the run is over). The sink's own per-write timeout should make
// this bound unreachable in practice.
const closeWaitBound = 10 * time.Second

// Close ends the run: stops the goroutine, drains pending events, settles the active entry to "done", and
// does a final flush. Safe to call once; nil-safe. Blocks until the goroutine exits (so apply-phase writes
// that follow don't race the narrator on the shared sink) OR closeWaitBound elapses (so a stuck sink can't
// hang the run).
func (n *Narrator) Close() {
	if n == nil {
		return
	}
	n.closeOnce.Do(func() { close(n.done) })
	t := time.NewTimer(closeWaitBound)
	defer t.Stop()
	select {
	case <-n.stopped: // goroutine drained + did its final flush — the clean path
	case <-t.C: // stuck sink — return anyway rather than hang the run (best-effort narration)
	}
}

// run is the single narration goroutine. It owns narratorState exclusively (no locks — all mutation is
// here). It selects over: incoming producer events, the heartbeat tick (paces the active entry + maybe
// fires the async summarizer), an async summarizer result (revises the active entry), and shutdown.
func (n *Narrator) run(ctx context.Context) {
	defer close(n.stopped)

	st := &narratorState{}
	ticker := time.NewTicker(n.cfg.Heartbeat)
	defer ticker.Stop()

	for {
		select {
		case ev := <-n.events:
			n.handleEvent(st, ev)

		case <-ticker.C:
			n.handleHeartbeat(ctx, st)

		case res := <-n.summaries:
			// An async summarizer returned a revised line for the entry it was computed against (res.entryID).
			// Apply only if non-empty AND that SAME entry is still the active one — a turn boundary may have
			// settled it and opened a NEW active entry meanwhile, and stamping a round-1 summary onto the
			// round-2 entry would be wrong narration. reviseActiveIf enforces the id match (stale → dropped).
			if res.text != "" && n.reviseActiveIf(st, res.entryID, res.text) {
				st.activeSummarized = true // the summarizer (the real voice) now owns this entry — pacer stands down
				n.sink.WriteDoc(st.doc)
			}

		case <-n.done:
			// Drain any buffered events (best-effort), settle the active entry, final flush, exit.
			n.drain(st)
			if n.settleActive(st) {
				n.sink.WriteDoc(st.doc)
			}
			return
		}
	}
}

// handleEvent maps one producer event onto a document mutation + flush.
func (n *Narrator) handleEvent(st *narratorState, ev Event) {
	switch ev.Kind {
	case EventTurnStart:
		// Open a FRESH active entry immediately (fills the token-less gap with honest fallback text). The
		// seed text is the consumer's line (ev.Text) — mode-aware, no LLM. Prior active entry settles.
		st.paceIdx = 0
		// The consumer names the action verb (its domain vocabulary — "planning"/"reviewing") via ev.Action;
		// the raw loop role is only a last-resort fallback so a consumer that doesn't set it still gets a verb.
		action := ev.Action
		if action == "" {
			action = ev.Role
		}
		if action == "" {
			action = "working"
		}
		n.appendActive(st, action, ev.Text)
		n.sink.WriteDoc(st.doc)

	case EventTurnEnd:
		// The turn finished — settle the active entry (its work is done). The NEXT turn-start opens a new one.
		if n.settleActive(st) {
			n.sink.WriteDoc(st.doc)
		}

	case EventCritique:
		// Enrich the summary buffer ONLY — the reviewer's feedback never becomes its own user entry (and is
		// never shown verbatim; the summarizer softens it). No flush (no visible change yet).
		st.critique = append(st.critique, ev.Context...)
		if ev.Text != "" {
			st.critique = append(st.critique, ev.Text)
		}

	case EventPhase:
		// A phase advance and/or a labels update. Labels feed the summarizer; a non-empty phase updates the
		// doc's coarse phase (the consumer's vocabulary, opaque here).
		if len(ev.Context) > 0 {
			st.labels = ev.Context
		}
		if ev.Phase != "" && ev.Phase != st.doc.Phase {
			st.doc.Phase = ev.Phase
			n.sink.WriteDoc(st.doc)
		}

	case EventAction:
		// A concrete action happened — append a SETTLED entry (it already happened; not "active"). Also
		// record it for the summarizer's context.
		if ev.Text != "" {
			st.actions = append(st.actions, ev.Text)
			n.appendDone(st, actionOr(ev.Action, "action"), ev.Text)
			n.sink.WriteDoc(st.doc)
		}
	}
}

// handleHeartbeat paces the active entry through the token-less gap: advance the templated "still working"
// variant (if a pacer is set), and — when there's buffered signal and the cost cap allows — kick the async
// summarizer to revise the active entry into a warmer/specific line.
func (n *Narrator) handleHeartbeat(ctx context.Context, st *narratorState) {
	if activeIdx(st.doc) < 0 {
		return // nothing live to animate
	}
	// (a) the pacer is a FALLBACK placeholder, not the voice — apply it ONLY until the summarizer has
	// spoken for this entry (activeSummarized). Once the real summarizer line has landed, the pacer stands
	// down so it never clobbers it. Key on the active entry's ACTION (planning/reviewing/…) so the
	// placeholder matches the real step. (Skipped entirely if the summarizer already owns the entry.)
	if n.pacer != nil && !st.activeSummarized {
		st.paceIdx++
		key := st.doc.Phase
		if i := activeIdx(st.doc); i >= 0 && st.doc.Entries[i].Action != "" {
			key = st.doc.Entries[i].Action
		}
		if line := n.pacer(key, st.paceIdx); line != "" {
			if n.reviseActive(st, line) {
				n.sink.WriteDoc(st.doc)
			}
		}
	}
	// (b) fire the async summarizer if enabled, under the per-run cap, ONLY when NEW signal has arrived since
	// the last dispatch. Without this dirty-check the summarizer re-fired every heartbeat on the SAME
	// (never-reset) buffer, burning all MaxSummarizerCalls LLM calls to re-summarize identical input (cost +
	// DSQ contention for no new narration). signalLen grows only when a critique/action/label was appended.
	if n.sum == nil || st.calls >= n.cfg.MaxSummarizerCalls {
		return
	}
	signalLen := len(st.critique) + len(st.actions) + len(st.labels)
	if signalLen == 0 || signalLen == st.lastSummarizedLen {
		return // nothing new to summarize since the last call
	}
	st.calls++
	st.lastSummarizedLen = signalLen
	// The summary targets the CURRENT active entry — capture its id so a result landing after a turn
	// boundary revises the RIGHT entry (or is dropped), not whatever is active by then.
	targetID := ""
	if i := activeIdx(st.doc); i >= 0 {
		targetID = st.doc.Entries[i].ID
	}
	summary := NarrationSummary{
		Phase:    st.doc.Phase,
		Labels:   append([]string(nil), st.labels...),
		Critique: append([]string(nil), st.critique...),
		Actions:  append([]string(nil), st.actions...),
	}
	go n.summarizeAsync(ctx, targetID, summary)
}

// summaryResult carries an async summarizer's output back to the goroutine: the revised text + the id of
// the entry it was computed for (so a stale result doesn't revise a newer entry).
type summaryResult struct {
	entryID string
	text    string
}

// summarizeAsync runs one summarizer call OFF the critical path with its own timeout, and posts the result
// back to the goroutine (non-blocking; dropped if the run already ended). A nil/empty/timeout/error result
// simply doesn't revise anything (graceful degrade to the templated line).
func (n *Narrator) summarizeAsync(ctx context.Context, entryID string, s NarrationSummary) {
	cctx, cancel := context.WithTimeout(ctx, n.cfg.SummarizeTimeout)
	defer cancel()
	text := n.sum.Summarize(cctx, s)
	if text == "" {
		return
	}
	select {
	case n.summaries <- summaryResult{entryID: entryID, text: text}:
	case <-n.done:
	default:
	}
}

// drain pulls any buffered events so shutdown reflects the last signals (best-effort, non-blocking).
func (n *Narrator) drain(st *narratorState) {
	for {
		select {
		case ev := <-n.events:
			n.handleEvent(st, ev)
		default:
			return
		}
	}
}

// --- document mutation primitives (goroutine-only) ---

// appendActive settles any current active entry, then appends a NEW active entry with a fresh stable id.
func (n *Narrator) appendActive(st *narratorState, action, detail string) {
	settleAll(&st.doc)
	st.activeSummarized = false // a fresh entry: the pacer placeholder applies until the summarizer speaks again
	st.seq++
	st.doc.Entries = append(st.doc.Entries, NarrationEntry{
		ID:     "e" + strconv.Itoa(st.seq),
		At:     nowRFC3339(),
		Action: action,
		Detail: detail,
		State:  StateActive,
	})
}

// appendDone appends a settled entry (an action that already happened) with a fresh stable id. It does NOT
// disturb a live active entry's identity — but an action landing means prior narration is settled, so we
// settle the active one first (the action supersedes the "working…" line).
func (n *Narrator) appendDone(st *narratorState, action, detail string) {
	// ⛔ A SETTLED ENTRY WITH NO DETAIL IS A ROW THAT SAYS NOTHING — and unlike an ACTIVE one, it
	// will never be filled in. The document renders one line per entry, so this is a permanent BLANK
	// ROW: the list grows while telling the user nothing, which reads as stuttering rather than
	// working. Observed in a real compose run as two {action:"once", detail:""} entries.
	//
	// ⚠️ The equivalent guard does NOT belong on appendActive: an active entry is legitimately empty
	// until the pacer or summarizer speaks, and suppressing it would delete the live "working" row.
	//
	// Suppressed at the SOURCE because one document feeds every surface (the composer, /today's
	// todos, projects) — filtering per client leaves the blank row rendering in the others.
	if strings.TrimSpace(detail) == "" {
		return
	}
	settleAll(&st.doc)
	st.seq++
	st.doc.Entries = append(st.doc.Entries, NarrationEntry{
		ID:     "e" + strconv.Itoa(st.seq),
		At:     nowRFC3339(),
		Action: action,
		Detail: detail,
		State:  StateDone,
	})
}

// reviseActive rewrites the active entry's detail IN PLACE (same id). Returns false if there's no active
// entry or the text is unchanged (so the caller can skip a redundant flush).
func (n *Narrator) reviseActive(st *narratorState, detail string) bool {
	i := activeIdx(st.doc)
	if i < 0 || st.doc.Entries[i].Detail == detail {
		return false
	}
	st.doc.Entries[i].Detail = detail
	st.doc.Entries[i].At = nowRFC3339()
	return true
}

// reviseActiveIf rewrites the active entry's detail ONLY IF the active entry is still the one identified by
// wantID (an empty wantID means "the summary was for no specific entry" and is dropped). This guards the
// async-summary path: a summary computed for round-1's entry must NOT overwrite round-2's entry after a
// turn boundary swapped the active one. Returns false (no flush) when the id doesn't match or the text is
// unchanged.
func (n *Narrator) reviseActiveIf(st *narratorState, wantID, detail string) bool {
	i := activeIdx(st.doc)
	if i < 0 || wantID == "" || st.doc.Entries[i].ID != wantID {
		return false // no active entry, or a different entry is active now → stale, drop
	}
	return n.reviseActive(st, detail)
}

// settleActive marks the active entry done. Returns true if it changed anything.
func (n *Narrator) settleActive(st *narratorState) bool {
	i := activeIdx(st.doc)
	if i < 0 {
		return false
	}
	st.doc.Entries[i].State = StateDone
	return true
}

// --- pure helpers ---

// activeIdx returns the index of the (single) active entry, or -1. Only the last entry can be active, so
// this checks the tail.
func activeIdx(doc NarrationDoc) int {
	if k := len(doc.Entries) - 1; k >= 0 && doc.Entries[k].State == StateActive {
		return k
	}
	return -1
}

// settleAll marks any active entry done (there is at most one — the tail).
func settleAll(doc *NarrationDoc) {
	if i := activeIdx(*doc); i >= 0 {
		doc.Entries[i].State = StateDone
	}
}

// actionOr returns a non-empty action verb.
func actionOr(a, fallback string) string {
	if a == "" {
		return fallback
	}
	return a
}
