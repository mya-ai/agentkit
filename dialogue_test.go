package agentkit

import (
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// DIALOGUE — the tests define what a turn is.
//
// The shape is taken from what BOTH reference consumers — an ops agent and a planning agent — already
// built by hand: a question, an anchor object, slots the user fills, an identity for dedupe, and — for
// a proposal — the act being asked about. The kit CARRIES those; it does not derive them (the consumer
// owns the op vocabulary) and it does not deliver them (the consumer owns the transport).
// ─────────────────────────────────────────────────────────────────────────────────────────────────

func TestNewQuestion_CarriesWhatTheConsumerSupplies(t *testing.T) {
	q, err := NewQuestion("What cutover date should I propose to Dana?",
		ObjectRef{Kind: "signal", ID: "item-1"},
		TurnSlot{Field: "answer", Type: "text", Label: "Your answer", Required: true})
	if err != nil {
		t.Fatalf("a well-formed question was rejected: %v", err)
	}

	if q.Kind != TurnQuestion {
		t.Errorf("Kind = %v, want TurnQuestion", q.Kind)
	}
	if q.Act != nil {
		t.Error("a question has NO act — that is what distinguishes it from a proposal")
	}
	if len(q.Slots) != 1 || q.Slots[0].Field != "answer" {
		t.Errorf("the consumer's slot was not carried: %+v", q.Slots)
	}
}

// TestNewProposal_CarriesTheAct also pins the vocabulary: a proposal's act is an OP (the backend's
// opcode + operands), NOT a ToolCall (the model's capability call). Conflating them is a real bug
// source — the model FILLS a capability's args, while a user may fill an op's operands.
func TestNewProposal_CarriesTheAct(t *testing.T) {
	act := Op{Opcode: "send_reply", Args: map[string]string{"body": "Tuesday works."}}

	p, err := NewProposal("I've written this reply — send it?", ObjectRef{Kind: "signal", ID: "item-1"}, act)
	if err != nil {
		t.Fatalf("a well-formed proposal was rejected: %v", err)
	}

	if p.Kind != TurnProposal {
		t.Errorf("Kind = %v, want TurnProposal", p.Kind)
	}
	if p.Act == nil || p.Act.Opcode != "send_reply" {
		t.Fatal("a proposal must carry the act it is about — that IS the proposal")
	}
	if got := p.Act.Args["body"]; got != "Tuesday works." {
		t.Errorf("the act's args were lost: %q", got)
	}
}

// TestTurn_RequiresABodyAndAnAnchor pins the two things a turn cannot be useful without: something
// to say, and something to say it ABOUT. An unanchored turn lands on no card and vanishes from the
// UI; an empty one shows the human a blank ask.
func TestTurn_RequiresABodyAndAnAnchor(t *testing.T) {
	anchor := ObjectRef{Kind: "signal", ID: "item-1"}

	if _, err := NewQuestion("", anchor); err == nil {
		t.Error("an empty question was accepted — the human would see an ask with nothing in it")
	}
	if _, err := NewQuestion("real question?", ObjectRef{}); err == nil {
		t.Error("an unanchored turn was accepted — it lands on no object and disappears from the UI")
	}
	if _, err := NewProposal("send it?", anchor, Op{}); err == nil {
		t.Error("a proposal with no act was accepted — that is a question, not a proposal")
	}
}

// TestTurn_Key is the anti-duplicate identity: stable per (agent, object, act), NEVER including the
// wording. A re-worded turn about the same act is the SAME turn and must refresh in place; keying on
// the question makes a better-worded ask pile up beside the stale one.
func TestTurn_Key(t *testing.T) {
	anchor := ObjectRef{Kind: "signal", ID: "item-1"}
	act := Op{Opcode: "send_reply", Args: map[string]string{"body": "v1"}}

	first, _ := NewProposal("Send it?", anchor, act)
	reworded, _ := NewProposal("Shall I send this instead?", anchor,
		Op{Opcode: "send_reply", Args: map[string]string{"body": "v2, better"}})

	if first.Key("ops") != reworded.Key("ops") {
		t.Errorf("a RE-WORDED turn about the same act got a different key (%q vs %q) — it would stack a "+
			"second copy on the user instead of refreshing the first",
			first.Key("ops"), reworded.Key("ops"))
	}

	other, _ := NewProposal("Archive it?", anchor, Op{Opcode: "archive_item"})
	if first.Key("ops") == other.Key("ops") {
		t.Error("two DIFFERENT acts on the same object share a key — one silently replaces the other")
	}

	q, _ := NewQuestion("Which vendor?", anchor)
	if q.Key("ops") == first.Key("ops") {
		t.Error("a question collides with a proposal on the same object")
	}
}

// TestTurn_KeyDiscriminator is the regression guard for a collision BOTH production consumers hit
// independently, from different directions — the strongest signal this repo has produced.
//
// An ops agent surfaced every tier-withheld act through one opcode, so `send_reply` and `archive_item`
// on one object produced the SAME key. A planning agent asked every open question through
// `ask_user` anchored to the parent object, so two unrelated questions produced the same key:
//
//	"The plan overruns the deadline — which scope would you cut?"  → triage:ticket:t-1:ask_user
//	"Stay with the incumbent vendor or re-bid?"                    → triage:ticket:t-1:ask_user
//
// A backend that upserts ON CONFLICT on that key silently OVERWRITES the first with the second, and
// the user is never asked.
//
// ⭐ The root cause is that an opcode like `ask_user` is a CHANNEL, not an act — and
// (object, channel) cannot discriminate what is being asked. AskKey already takes
// `discriminator ...string` for exactly this; Turn.Key called it and passed nothing, exposing no way
// for a consumer to supply one. The wrapper REMOVED a capability the underlying function had, which
// turned a bug a consumer could fix in one line into one they could not fix at all.
func TestTurn_KeyDiscriminator(t *testing.T) {
	anchor := ObjectRef{Kind: "project", ID: "p-1"}

	scope, _ := NewQuestion("The plan overruns the deadline — which scope would you cut?", anchor)
	vendor, _ := NewQuestion("Do you want to stay with the incumbent vendor or re-bid?", anchor)

	// Without a discriminator these are the same ask — correct, and why the wording is excluded.
	if scope.Key("triage") != vendor.Key("triage") {
		t.Fatal("keys must not depend on the wording — a re-worded ask must REFRESH, not stack")
	}

	// ⭐ With one, two genuinely different questions are distinguishable.
	a := scope.Key("triage", "scope_cut")
	b := vendor.Key("triage", "vendor_choice")
	if a == b {
		t.Errorf("⛔ two DIFFERENT questions on one object still collide (%q) — the second silently "+
			"overwrites the first and the user is never asked it", a)
	}

	// And the discriminator must be stable: the same ask re-asked keeps its key.
	if scope.Key("triage", "scope_cut") != a {
		t.Error("the same ask must produce a stable key across runs, or it stacks instead of refreshing")
	}

	// A withheld act discriminates on the capability that was withheld (the ops-agent shape).
	send, _ := NewProposal("Send the reply?", anchor, Op{Opcode: "ask_user"})
	arch, _ := NewProposal("Archive it?", anchor, Op{Opcode: "ask_user"})
	if send.Key("ops", "withheld:send_reply") == arch.Key("ops", "withheld:archive_item") {
		t.Error("two withheld acts routed through one opcode still collide")
	}
}

// TestNewQuestion_AcceptsAChannelOp pins the second thing BOTH consumers hit: the kit modelled
// question-vs-proposal as absence-vs-presence of an act, but the backend models it as WHICH OPCODE.
//
// The reference backend had exactly one channel for "ask the user and route the answer back", and it
// IS an op (`ask_user`). So a pure question must carry that opcode to be deliverable — and
// validate() rejected it with "a question carries no act — use NewProposal", which would have made
// it a PROPOSAL and changed its meaning to the user.
//
// ⭐ Two of two consumers had to violate the invariant to use the type. That is the kit being wrong
// about the world, not the consumers being unusual. A question may now carry a channel op; what
// still distinguishes the two kinds is the CONSUMER's Kind, which is what the user experiences.
func TestNewQuestion_AcceptsAChannelOp(t *testing.T) {
	anchor := ObjectRef{Kind: "project", ID: "p-1"}

	q, err := NewQuestion("Which scope would you cut?", anchor)
	if err != nil {
		t.Fatalf("a plain question was rejected: %v", err)
	}
	if q.Kind != TurnQuestion {
		t.Error("Kind must remain TurnQuestion")
	}

	// The same question, carried over the backend's question CHANNEL.
	viaChannel, err := NewQuestionVia("Which scope would you cut?", anchor,
		Op{Opcode: "ask_user"})
	if err != nil {
		t.Fatalf("⛔ a question carried over the backend's question channel was rejected — but that is "+
			"the ONLY way both production consumers can deliver one: %v", err)
	}
	if viaChannel.Kind != TurnQuestion {
		t.Error("carrying a channel op must NOT turn a question into a proposal — the user would see " +
			"an approve/reject affordance for something that is not an act")
	}
	if viaChannel.Act == nil || viaChannel.Act.Opcode != "ask_user" {
		t.Error("the channel op must be carried through to the transport")
	}
}

// TestReply_ThreeOutcomes pins the reply vocabulary: approve RELEASES the act, reject DROPS it, and redirect HANDS
// IT BACK — the outcome of a redirect is not determined by the reply, which is exactly what makes it
// a third kind rather than a shaded rejection.
func TestReply_ThreeOutcomes(t *testing.T) {
	cases := []struct {
		reply       Reply
		releasesAct bool
		endsTheTurn bool
		handsBack   bool
		wantString  string
	}{
		{ReplyApprove, true, true, false, "approve"},
		{ReplyReject, false, true, false, "reject"},
		{ReplyRedirect, false, true, true, "redirect"},
	}

	for _, c := range cases {
		if got := c.reply.ReleasesAct(); got != c.releasesAct {
			t.Errorf("%s.ReleasesAct() = %v, want %v", c.wantString, got, c.releasesAct)
		}
		if got := c.reply.HandsBackToAgent(); got != c.handsBack {
			t.Errorf("%s.HandsBackToAgent() = %v, want %v", c.wantString, got, c.handsBack)
		}
		if got := c.reply.String(); got != c.wantString {
			t.Errorf("String() = %q, want %q", got, c.wantString)
		}
	}
}

// TestReplyRedirect_IsNotARejection is the distinction that has bitten in production: recording a
// redirect as "rejected" makes a correction-suppression mechanism silence the very re-proposal the
// user just asked for.
func TestReplyRedirect_IsNotARejection(t *testing.T) {
	if ReplyRedirect == ReplyReject {
		t.Fatal("redirect and reject must be distinct values")
	}
	if ReplyRedirect.IsRejection() {
		t.Error("⛔ a redirect reported as a rejection — the user said 'do it differently', not 'do not do it'. " +
			"Recording it as a reject suppresses the re-proposal they asked for.")
	}
	if !ReplyReject.IsRejection() {
		t.Error("reject must report as a rejection")
	}
}

// TestTurn_RenderDialogue is how the MODEL learns it can speak. Dialogue is not a capability, so it
// is not in RenderCapabilities — but something must still reach the prompt, or the option is
// invisible to the model.
func TestTurn_RenderDialogue(t *testing.T) {
	rendered := RenderDialogue()
	for _, want := range []string{"question", "proposal"} {
		if !strings.Contains(strings.ToLower(rendered), want) {
			t.Errorf("the dialogue block does not mention %q — the model cannot know the option exists:\n%s",
				want, rendered)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ⭐ IDENTITY MUST TRAVEL WITH THE TURN.
//
// The discriminator was a METHOD ARGUMENT (t.Key(agent, disc...)), so it did not live on the value
// that has the identity. Consequence, demonstrated below: a deliverer holding ONLY a Turn cannot
// tell two different turns apart — it would have to be told separately, which is the
// "two things that must agree" shape the review standard forbids.
//
// ⚠️ NOT HYPOTHETICAL. This exact collision was LIVE IN PRODUCTION: every question from one ops agent
// rode the one ask_user opcode, so "what date?" and "who does this go to?" produced the same
// key, the backend upserted ON CONFLICT, and the user was never asked the first one.
//
// The fix is a FIELD (Turn.Discriminator). Key still accepts variadic args for the call-site form, so
// no existing caller breaks — but the field is what a generic transport can read.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// TestTurn_DiscriminatorTravelsWithTheValue is the regression: a deliverer given only the Turn must
// be able to derive a distinguishing key.
func TestTurn_DiscriminatorTravelsWithTheValue(t *testing.T) {
	about := ObjectRef{Kind: "signal", ID: "i1", Label: "the Dana thread"}

	askDate, err := NewQuestionVia("What cutover date should I propose?", about,
		Op{Opcode: "ask_user"}, TurnSlot{Field: "answer", Type: "text", Required: true})
	if err != nil {
		t.Fatalf("date ask: %v", err)
	}
	askDate.Discriminator = "asking:value"

	askWho, err := NewQuestionVia("Who should this go to?", about,
		Op{Opcode: "ask_user"}, TurnSlot{Field: "answer", Type: "text", Required: true})
	if err != nil {
		t.Fatalf("recipient ask: %v", err)
	}
	askWho.Discriminator = "asking:recipient"

	// ⭐ THE POINT: a transport that receives ONLY the Turn. It is told nothing else.
	deliver := func(turn Turn) string { return turn.Key("ops") }

	if deliver(askDate) == deliver(askWho) {
		t.Fatalf("⛔ two DIFFERENT turns share key %q — a deliverer holding only the Turn cannot tell "+
			"them apart, so the second silently overwrites the first and the user is never asked it",
			deliver(askDate))
	}
}

// TestTurn_KeyIsStableAcrossRuns pins the other half: the discriminator is an IDENTITY, not a nonce.
// A re-worded turn about the same thing must keep its key so it REFRESHES the open row.
func TestTurn_KeyIsStableAcrossRuns(t *testing.T) {
	about := ObjectRef{Kind: "signal", ID: "i1", Label: "the Dana thread"}

	first, _ := NewQuestionVia("What cutover date should I propose?", about, Op{Opcode: "ask_user"})
	first.Discriminator = "asking:value"
	second, _ := NewQuestionVia("Which date do you want me to use?", about, Op{Opcode: "ask_user"})
	second.Discriminator = "asking:value" // re-worded, SAME ask

	if first.Key("ops") != second.Key("ops") {
		t.Errorf("⛔ a re-worded turn about the same thing changed key (%q → %q) — it would stack beside "+
			"the open row instead of refreshing it, and the user sees the question twice",
			first.Key("ops"), second.Key("ops"))
	}
}

// TestTurn_KeyArgumentStillWorks — the field must not break the existing call-site form.
func TestTurn_KeyArgumentStillWorks(t *testing.T) {
	about := ObjectRef{Kind: "signal", ID: "i1", Label: "x"}
	turn, _ := NewQuestionVia("q", about, Op{Opcode: "ask_user"})

	fromArg := turn.Key("ops", "withheld:send_reply")
	turn.Discriminator = "withheld:send_reply"
	fromField := turn.Key("ops")

	if fromArg != fromField {
		t.Errorf("the field and the argument must produce the SAME key, got %q vs %q", fromArg, fromField)
	}
}
