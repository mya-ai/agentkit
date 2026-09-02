package agentkit

import (
	"fmt"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ⭐ THE INVARIANT: EVERY PROPOSAL NAMES WHAT IT ACTS ON.
//
// These cases are drawn from real failures. The large majority of pending proposals could not be blessed:
//
//	"I'll add your existing todo 'Draft the launch checklist' to THIS PROJECT — approve."   ×4, each a different project
//	"I'll mark it complete — your task 'Review and patch vulnerabilities'."          ← on WHAT board?
//	"I'll mark this task done — approve."                                            ← WHICH task?
//
// Four identical Approve buttons acting on invisible objects is asking someone to sign a blank cheque.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

const (
	taskID    = "33333333-4444-4444-4444-555555555555"
	projectID = "66666666-7777-7777-7777-888888888888"
)

// world is what the agent gathered — the labels Go knows and the model never writes.
var world = MapLabeler(map[string]map[string]string{
	TokenTask:    {taskID: "Draft the launch checklist"},
	TokenProject: {projectID: "Platform Hardening"},
})

func taskRef() ObjectRef {
	return ObjectRef{Kind: TokenTask, ID: taskID, Label: "Draft the launch checklist"}
}
func projectRef() ObjectRef {
	return ObjectRef{Kind: TokenProject, ID: projectID, Label: "Platform Hardening"}
}

// ⭐ THE LIVE BUGS. Each of these shipped to a real user, and each is now impossible.
func TestTokenizeQuestion_TheLiveBugs(t *testing.T) {
	tests := []struct {
		name   string
		q      string
		anchor ObjectRef
		also   []ObjectRef
		// after tokenizing, the plain-prose reading a log/email/notification would see:
		wantProse string
		why       string
	}{
		{
			name:   "⭐ '…to THIS PROJECT' ×4 — four identical asks about four different projects",
			q:      `I'll add your existing todo "Draft the launch checklist" to this project so it's tracked here — approve.`,
			anchor: taskRef(),
			also:   []ObjectRef{projectRef()},
			wantProse: "I'll add your existing todo Draft the launch checklist to Platform Hardening so it's tracked here " +
				"— approve.",
			why: "the Go builder had the project's name on the very struct it was handed, and threw it away. " +
				"Four of these in a row were literally indistinguishable. NOTE the tokenizer does not rewrite " +
				"the author's prose ('your existing todo') — it only NAMES what was left invisible.",
		},
		{
			name:      "⭐ 'I'll mark IT complete — your task X' — complete it on WHAT board?",
			q:         `I'll mark it complete — your task "Draft the launch checklist". Approve.`,
			anchor:    taskRef(),
			wantProse: "I'll mark it complete — your task Draft the launch checklist. Approve.",
			why: "the quoted task becomes a chip (pass 2), so the object IS named and pass 3 correctly does " +
				"NOT also replace 'it' — double-naming would mangle the sentence ('mark Draft the launch checklist " +
				"complete — your task Draft the launch checklist'). Naming once is enough; the invariant is satisfied.",
		},
		{
			name:      "⭐ 'I'll mark THIS TASK done' — the Go fallback names nothing at all",
			q:         "I'll mark this task done — approve.",
			anchor:    taskRef(),
			wantProse: "I'll mark Draft the launch checklist done — approve.",
			why:       "the blindest string in the system: a fallback with no object, no quotes, nothing.",
		},
		{
			name:      "a sentence that names nothing and has no deictic → APPEND. Ugly beats ambiguous.",
			q:         "Approve?",
			anchor:    taskRef(),
			wantProse: "Approve? (Draft the launch checklist)",
			why:       "the guarantee holds even when there is nothing to replace",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TokenizeQuestion(world, tc.q, tc.anchor, tc.also...)

			// ⭐ THE INVARIANT: the sentence names its object.
			if !HasTokenFor(got, tc.anchor.Kind, tc.anchor.ID) {
				t.Fatalf("⛔ THE PROPOSAL STILL DOES NOT NAME WHAT IT ACTS ON.\n"+
					"   in:  %q\n   out: %q\n"+
					"   The user is being asked to bless a decision whose object is invisible. That is a blank "+
					"cheque.\n   WHY THIS CASE: %s", tc.q, got, tc.why)
			}

			// And the DUMB READER (a log, an email, a notification) sees clean prose.
			if prose := StripTokens(got); prose != tc.wantProse {
				t.Errorf("prose = %q,\n  want %q\n  (a token-free reader must still read a correct sentence)",
					prose, tc.wantProse)
			}
		})
	}
}

// A sentence that ALREADY names its object correctly is left alone — the tokenizer improves, it does not churn.
func TestTokenizeQuestion_LeavesACorrectSentenceAlone(t *testing.T) {
	q := "I'll add " + RefToken(taskRef()) + " to " + RefToken(projectRef()) + " — approve."

	if got := TokenizeQuestion(world, q, taskRef(), projectRef()); got != q {
		t.Errorf("an already-correct sentence was rewritten:\n  in:  %q\n  out: %q", q, got)
	}
}

// Idempotent — running twice changes nothing. (An agent may tokenize at more than one point.)
func TestTokenizeQuestion_IsIdempotent(t *testing.T) {
	once := TokenizeQuestion(world, "I'll mark this task done — approve.", taskRef())
	twice := TokenizeQuestion(world, once, taskRef())

	if once != twice {
		t.Errorf("not idempotent:\n  1st: %q\n  2nd: %q", once, twice)
	}
}

// ⭐ PASS 1 — the model's fallback may be STALE. Go's label is authoritative.
func TestTokenizeQuestion_RepairsAStaleModelToken(t *testing.T) {
	// The model emitted a real id with the name the task had LAST week.
	q := "I'll complete " + Token(TokenTask, taskID, "Prep the deck (old name)") + " — approve."

	got := TokenizeQuestion(world, q, taskRef())

	if strings.Contains(got, "old name") {
		t.Errorf("the STALE label survived: %q\n"+
			"   The model's fallback is a snapshot; Go's gather is the truth. The live label must win, or the "+
			"user reads a name the object no longer has.", got)
	}
	if !strings.Contains(got, "Draft the launch checklist") {
		t.Errorf("the live label did not replace it: %q", got)
	}
}

// ⭐ PASS 1 — an INVENTED id must never become a chip.
func TestTokenizeQuestion_StripsAnUnresolvableToken(t *testing.T) {
	// The model hallucinated an id. It looks perfectly well-formed.
	ghost := Token(TokenTask, "11111111-2222-3333-4444-555555555555", "A Task That Does Not Exist")
	q := "I'll complete " + ghost + " — approve."

	got := TokenizeQuestion(world, q, ObjectRef{})

	if HasTokenFor(got, TokenTask, "11111111-2222-3333-4444-555555555555") {
		t.Errorf("⛔ a token for an id that resolves to NOTHING survived: %q\n"+
			"   The client would render a chip pointing at a ghost — and the user would click it. Worse: the "+
			"fallback text is the name the model INVENTED, which is exactly the confabulation we are removing. "+
			"Keep the words, refuse the false link.", got)
	}
	if !strings.Contains(got, "A Task That Does Not Exist") {
		t.Errorf("the prose was destroyed along with the token: %q — strip the link, keep the sentence", got)
	}
}

// ⭐ PASS 2 must NOT carpet-bomb. Unquoted substring matching would tokenize "it" inside "I'll mark it".
func TestTokenizeQuestion_DoesNotSubstituteUnquotedText(t *testing.T) {
	// A task whose title is a common word.
	shipIt := MapLabeler(map[string]map[string]string{
		TokenTask: {taskID: "Ship"},
	})
	anchor := ObjectRef{Kind: TokenTask, ID: taskID, Label: "Ship"}

	got := TokenizeQuestion(shipIt, "I'll Ship this task — approve.", anchor)

	// The word "Ship" in prose must NOT have become a chip; the DEICTIC is what gets replaced.
	if strings.Count(got, "{{") != 1 {
		t.Errorf("expected exactly one token (the anchored deictic), got %q", got)
	}
	if prose := StripTokens(got); !strings.HasPrefix(prose, "I'll Ship ") {
		t.Errorf("the bare word was substituted, mangling the sentence: %q", prose)
	}
}

// ⭐ PASS 4 — a half-emitted token must never reach a human as raw braces.
func TestTokenizeQuestion_SweepsMalformedTokens(t *testing.T) {
	got := TokenizeQuestion(world, "I'll complete {{task:abc}} — approve.", ObjectRef{})

	if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
		t.Errorf("⛔ raw braces reached the output: %q\n"+
			"   The user would see template syntax — WORSE than the ambiguity we set out to fix.", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ⚠️ THE SILENT PROPOSAL-DESTROYER — verified against a real database.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// A non-UUID id does not "skip a chip" — it ABORTS THE WRITE and the whole proposal vanishes.
//
//	INSERT ... ref_type='milestone', ref_id='m1'
//	⛔ ERROR: invalid input syntax for type uuid: "m1"    → the txn rolls back → the ask is gone
//
// And it is REACHABLE: milestone ids are model-supplied when present, so a model that writes "m1" takes out
// the very proposal it was trying to make. No error the user or the agent will ever see.
func TestStorableRefs_DropsANonUUIDID(t *testing.T) {
	good := projectRef()
	bad := ObjectRef{Kind: TokenMilestone, ID: "m1", Label: "Launch"} // a model-supplied id

	got := StorableRefs(good, bad)

	if len(got) != 1 || got[0].ID != projectID {
		t.Fatalf("⛔ a non-UUID reference survived: %+v\n"+
			"   The reference write shares the TRANSACTION with the proposal row. One bad id aborts the insert "+
			"and the ENTIRE ASK IS SILENTLY DESTROYED — verified against a real database: "+
			`ERROR: invalid input syntax for type uuid: "m1"`, got)
	}
}

// Losing a chip is a scratch; losing the ask is fatal. The SENTENCE still names the object — the token
// carries its own label and never touches the database.
func TestTokenizeQuestion_UnstorableRefStillNamesTheObject(t *testing.T) {
	milestones := MapLabeler(map[string]map[string]string{
		TokenMilestone: {"m1": "Launch"},
	})
	anchor := ObjectRef{Kind: TokenMilestone, ID: "m1", Label: "Launch"}

	got := TokenizeQuestion(milestones, "I'd update this milestone — approve?", anchor)

	if prose := StripTokens(got); !strings.Contains(prose, "Launch") {
		t.Errorf("the sentence lost the object's name: %q — the ref may be unstorable, but the SENTENCE is "+
			"unaffected: the token carries its own label", prose)
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// The lifted machinery — byte-identical to what the two agents each hand-rolled.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// The dedupe key omits the QUESTION, deliberately: a re-worded (or freshly tokenized) draft of the SAME ask
// must REFRESH the open row, not pile a second copy beside it. Pinned byte-for-byte so the lift provably
// changes nothing on the wire for either agent.
func TestDedupeKey_MatchesBothAgentsExistingKeys(t *testing.T) {
	pm := DedupeKey("triage", Proposal{
		Object: ObjectRef{Kind: "ticket", ID: "t1"},
		Ops:    []ProposalOp{{Opcode: "add_to_queue"}},
	})
	if want := "triage:ticket:t1:add_to_queue"; pm != want {
		t.Errorf("triage key = %q, want %q — the lift must not change a live proposal's identity", pm, want)
	}

	opsKey := DedupeKey("ops", Proposal{
		Object: ObjectRef{Kind: "signal", ID: "i1"},
		Ops:    []ProposalOp{{Opcode: "ask_user"}},
	})
	if want := "ops:signal:i1:ask_user"; opsKey != want {
		t.Errorf("ops-agent key = %q, want %q", opsKey, want)
	}
}

// The archetype is DERIVED from shape, never hand-set — so it cannot drift from what the ask actually asks.
func TestDeriveArchetype(t *testing.T) {
	askingForSomethingOnlyTheUserKnows := Proposal{
		UserSlots: []UserSlot{{Operand: "answer", Required: true, Default: ""}},
	}
	if got := DeriveArchetype(askingForSomethingOnlyTheUserKnows); got != ArchetypeInput {
		t.Errorf("a REQUIRED slot with an EMPTY default = %q, want %q (the agent is asking; nothing to "+
			"approve — the UI must render a field + Submit)", got, ArchetypeInput)
	}

	proposingADraft := Proposal{
		UserSlots: []UserSlot{{Operand: "title", Required: true, Default: "Draft the launch checklist"}},
	}
	if got := DeriveArchetype(proposingADraft); got != ArchetypeDraft {
		t.Errorf("a required slot WITH a drafted default = %q, want %q (the agent did the thinking; one-tap)",
			got, ArchetypeDraft)
	}

	if got := DeriveArchetype(Proposal{}); got != ArchetypeDraft {
		t.Errorf("no slots = %q, want %q (a pure approve)", got, ArchetypeDraft)
	}
}

// Build is the choke point: it resolves the labels Go knows (the agent never writes a name) and tokenizes.
func TestBuild_ResolvesLabelsAndNamesTheObject(t *testing.T) {
	// The agent supplies IDS ONLY — no labels. Go fills them in.
	p := Build(world, Proposal{
		Question: "I'll mark this task done — approve.",
		Object:   ObjectRef{Kind: TokenTask, ID: taskID},
		Context:  ObjectRef{Kind: TokenProject, ID: projectID},
		Ops:      []ProposalOp{{Opcode: "complete_task"}},
	})

	if p.Object.Label != "Draft the launch checklist" {
		t.Errorf("Build did not resolve the anchor's label: %+v", p.Object)
	}
	if p.Context.Label != "Platform Hardening" {
		t.Errorf("Build did not resolve the context's label: %+v", p.Context)
	}
	if !HasTokenFor(p.Question, TokenTask, taskID) {
		t.Errorf("⛔ Build shipped a proposal that does not name its object: %q", p.Question)
	}
}

// An id the agent cannot resolve is DROPPED, not rendered — it would be a chip pointing at nothing.
func TestBuild_DropsAnUnresolvableRef(t *testing.T) {
	p := Build(world, Proposal{
		Question: "I'll do the thing.",
		Object:   ObjectRef{Kind: TokenTask, ID: "99999999-9999-9999-9999-999999999999"}, // not in the world
		Ops:      []ProposalOp{{Opcode: "complete_task"}},
	})

	if !p.Object.IsZero() {
		t.Errorf("an unresolvable anchor survived: %+v — it would render a chip to a ghost", p.Object)
	}
}

// fakeChangeEvaluator answers WillChange from a per-op-opcode map (and an optional error), so the kit-side
// FilterNoOpProposals gate can be tested without any backend.
type fakeChangeEvaluator struct {
	changes map[string]bool // opcode → would it change state
	err     error
}

func (f fakeChangeEvaluator) WillChange(op ProposalOp, _ ObjectRef) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.changes[op.Opcode], nil
}

// TestFilterNoOpProposals: the kit-side gate keeps only proposals whose op would CHANGE state, drops the
// no-ops, and FAILS OPEN (an evaluator error keeps the proposal). A nil evaluator is a pass-through.
func TestFilterNoOpProposals(t *testing.T) {
	mk := func(op string) Proposal {
		return Proposal{Question: "q", Object: ObjectRef{Kind: TokenTask, ID: "t"}, Ops: []ProposalOp{{Opcode: op}}}
	}
	changing, noop := mk("changes"), mk("noop")

	// Evaluator says "changes" mutates state, "noop" does not → only the changing one survives.
	eval := fakeChangeEvaluator{changes: map[string]bool{"changes": true, "noop": false}}
	got := FilterNoOpProposals(eval, []Proposal{changing, noop})
	if len(got) != 1 || got[0].Ops[0].Opcode != "changes" {
		t.Fatalf("want only the state-changing proposal kept, got %d: %+v", len(got), got)
	}

	// Fail-open: an evaluator error keeps everything (a dropped ask is silent; a shown one is recoverable).
	failing := fakeChangeEvaluator{err: errSentinel}
	if got := FilterNoOpProposals(failing, []Proposal{noop}); len(got) != 1 {
		t.Fatalf("fail-open: an evaluator error must KEEP the proposal, got %d", len(got))
	}

	// Nil evaluator → pass-through (no state to consult).
	if got := FilterNoOpProposals(nil, []Proposal{changing, noop}); len(got) != 2 {
		t.Fatalf("a nil evaluator must be a pass-through, got %d", len(got))
	}

	// A proposal with no op is kept (nothing to evaluate; IsValid handles malformed asks).
	if got := FilterNoOpProposals(eval, []Proposal{{Question: "q", Object: ObjectRef{Kind: TokenTask, ID: "t"}}}); len(got) != 1 {
		t.Fatalf("a proposal with no op must be kept, got %d", len(got))
	}
}

var errSentinel = fmt.Errorf("state read failed")

// TestAskKey_MatchesTheHandRolledConsumers pins AskKey to the exact strings both agents already
// produce. Adoption must be a pure deletion: a changed key orphans every OPEN proposal in prod (the
// old row never collides again, so a second copy of the same ask appears beside it).
func TestAskKey_MatchesTheHandRolledConsumers(t *testing.T) {
	// The planning agent's hand-rolled form — agent + ":" + objectType + ":" + objectID + ":" + op
	if got, want := AskKey("triage", "ticket", "t-1", "set_ticket_due_date"),
		"triage:ticket:t-1:set_ticket_due_date"; got != want {
		t.Errorf("triage key = %q, want %q — adoption would orphan every open proposal", got, want)
	}

	// The ops agent's hand-rolled form — agent + ":" + objectType + ":" + itemID + ":" + ask_user
	if got, want := AskKey("ops", "signal", "i-1", "ask_user"),
		"ops:signal:i-1:ask_user"; got != want {
		t.Errorf("ops-agent ask key = %q, want %q", got, want)
	}

	// The withheld-act key needs a discriminator: every withheld act rides ask_user, so without
	// it "send this reply?" and "archive this?" collide.
	if got, want := AskKey("ops", "signal", "i-1", "ask_user", "withheld:send_reply"),
		"ops:signal:i-1:ask_user:withheld:send_reply"; got != want {
		t.Errorf("ops-agent withheld key = %q, want %q", got, want)
	}

	// An empty discriminator must not leave a trailing separator (that would be a different key).
	if got, want := AskKey("triage", "ticket", "t-1", "close_ticket", ""), "triage:ticket:t-1:close_ticket"; got != want {
		t.Errorf("empty discriminator changed the key: %q, want %q", got, want)
	}
}

// TestDedupeKey_DelegatesToAskKey proves the two entry points cannot drift apart.
func TestDedupeKey_DelegatesToAskKey(t *testing.T) {
	p := Proposal{
		Object: ObjectRef{Kind: "ticket", ID: "t-9"},
		Ops:    []ProposalOp{{Opcode: "set_ticket_status"}},
	}
	if got, want := DedupeKey("triage", p), AskKey("triage", "ticket", "t-9", "set_ticket_status"); got != want {
		t.Errorf("DedupeKey = %q but AskKey = %q — the two forms must produce one key", got, want)
	}
}

// TestObjectRef_HasResolvedLabel pins the trap the README spends its biggest ⚠️ on: a struct literal
// leaves Label empty, and TokenizeQuestion's naming guarantee then silently no-ops.
//
// ⭐ It is NOT an error — an agent that writes its own sentences and never tokenizes has no use for a
// label, and one of the reference agents does exactly that. This is the assertion such a test can make
// when it DOES need the name, instead of discovering it from an ask that says "this project".
func TestObjectRef_HasResolvedLabel(t *testing.T) {
	labeler := MapLabeler(map[string]map[string]string{
		"project": {"p1": "Aruba migration"},
	})

	if got := Ref(labeler, "project", "p1"); !got.HasResolvedLabel() {
		t.Errorf("Ref() over a known id must resolve a label, got %+v", got)
	}
	// The trap, demonstrated: the literal the field list invites.
	if naive := (ObjectRef{Kind: "project", ID: "p1"}); naive.HasResolvedLabel() {
		t.Error("a struct literal has no label — this assertion is what makes that visible")
	}
	// An unresolvable id yields the ZERO ref, so an invented id can never become a chip.
	if got := Ref(labeler, "project", "does-not-exist"); !got.IsZero() {
		t.Errorf("an unresolvable id must yield the zero ref, got %+v", got)
	}
}

// TestTokenizeQuestion_SilentlyNoOpsWithoutALabel is the EVIDENCE for the trap — it demonstrates the
// failure rather than asserting it, so the cost of the empty label is visible in one test run.
func TestTokenizeQuestion_SilentlyNoOpsWithoutALabel(t *testing.T) {
	labeler := MapLabeler(map[string]map[string]string{"project": {"p1": "Aruba migration"}})
	const q = "Should I move the deadline?"

	withLabel := TokenizeQuestion(labeler, q, Ref(labeler, "project", "p1"))
	without := TokenizeQuestion(labeler, q, ObjectRef{Kind: "project", ID: "p1"})

	if withLabel == without {
		t.Fatalf("expected the labelled ref to name the object; both produced %q", without)
	}
	t.Logf("with Ref():        %q", withLabel)
	t.Logf("with bare literal: %q   ← the guarantee silently no-opped", without)
}
