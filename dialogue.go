package agentkit

import (
	"errors"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// DIALOGUE — the agent speaks to the user and WAITS.
//
// Two kinds, distinguished by what the turn is ABOUT:
//
//	QUESTION  — about INFORMATION the agent lacks. No act. The reply returns as context.
//	PROPOSAL  — about an ACT the agent may not perform alone. The reply RELEASES it, drops it,
//	            or hands it back.
//
// ⭐ DIALOGUE IS NOT A CAPABILITY. An agent does not declare "I can ask" among its acts; speaking to
// the user is how an agent behaves when it cannot proceed alone. It never passes through
// GateProgram — there is no tier on a question. (RenderDialogue is how the MODEL learns the option
// exists, since it is not in the capability list.)
//
// ⚠️ THAT IS THE TARGET, AND IT WAS NOT YET TRUE OF THE ONE ADOPTING CONSUMER. That ops agent still
// declared an `ask_the_user` capability. Migrating it onto Turn deleted 1 of its 5 sites, and the
// reason is structural — it is about WHO ORIGINATES the ask:
//
//	CODE-initiated  (the tier withheld an act) → needs no declaration, and never had one. ✅ holds
//	MODEL-initiated (it judged it needs you)   → emitted as a NAME in the one verified calls[] list,
//	                                             so deleting its spec makes GateProgram reject the
//	                                             WHOLE program (verdict, draft and tags with it)
//
// ⇒ While a model's only output channel is a verified calls[] list, a model-initiated ask MUST be
// declared. Closing the gap needs an EMISSION CHANNEL — `{"calls": [...], "turn": {...}}`, one more
// field on the agent's verdict struct, so speech never enters the act list. NOT BUILT (eval-first: it
// changes what the model must learn). Do not attempt the site deletions without it.
//
// ⭐ AND A PROPOSAL'S ACT IS AN **OP**, NOT A CAPABILITY CALL. These are different vocabularies and
// conflating them is a real bug source:
//
//	Capability + ToolCall   the MODEL's vocabulary. Args are model-filled. Verified by VerifyCall
//	                        against the agent's declared specs; routed by GateProgram's tier gate.
//	Op (below)              the BACKEND's vocabulary. Operands are user-filled at accept time.
//	                        Verified by the backend's own op registry, which is the authority and is
//	                        unbypassable.
//
// A capability the tier withheld BECOMES a proposal — but the agent maps it to its backend's op at
// that seam. The kit CARRIES the op; it cannot verify it (the registry lives in the service, and the
// kit must not import it) and it cannot derive its operands.
//
// WHAT THE KIT OWNS: the turn's shape, its identity (Key), and the reply vocabulary.
// WHAT YOU OWN: the wording, the slots, the op mapping, and DELIVERY (a DB row, MCP, a webhook).
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// TurnKind is what a turn is about.
type TurnKind int

const (
	// TurnQuestion is about INFORMATION. It carries no act; the reply is context for the next run.
	TurnQuestion TurnKind = iota
	// TurnProposal is about an ACT the agent may not perform alone. The reply decides the act's fate.
	TurnProposal
)

func (k TurnKind) String() string {
	if k == TurnProposal {
		return "proposal"
	}
	return "question"
}

// Op is the act a proposal is about, in the BACKEND's vocabulary — an opcode plus its operands.
//
// ⚠️ Deliberately NOT a ToolCall. A ToolCall is what the MODEL emitted (args it filled, verified
// against the agent's capability specs). An Op is what the BACKEND will execute if the user approves
// (operands the user may fill, verified against the backend's registry). The agent maps one to the
// other; the kit knows neither registry.
type Op struct {
	// Opcode is the backend's name for the act. The kit never interprets it.
	Opcode string
	// Args are the operands the agent has already filled. Operands the USER supplies are declared as
	// slots on the turn instead.
	Args map[string]string
}

// TurnSlot is one field the user fills or edits when replying.
//
// ⚠️ Field names an OPERAND OF THE OP, not an arg of a capability — the two vocabularies point in
// opposite directions (the model fills args; the user fills operands). The kit cannot derive slots:
// only the agent knows the op's operand set.
type TurnSlot struct {
	Field    string   // the op operand this fills
	Type     string   // text | date | number | select — the UI control
	Label    string   // what the human is asked for
	Required bool     // an empty required slot means the agent is ASKING, not proposing
	Options  []string // for Type=="select"
	// Default is the agent's DRAFTED value, pre-filled and editable — "here is the date I'd set,
	// approve or change it". Empty on a required slot means the agent genuinely does not know.
	Default string
}

// Turn is one thing the agent says to the user, and waits on.
//
// It is DATA. Building one delivers nothing: the consumer's transport does that.
type Turn struct {
	Kind TurnKind
	// Body is the human sentence. THE AGENT WRITES THIS — the kit never invents wording.
	Body string
	// About is what the turn is anchored to. Without it the turn lands on no object and disappears.
	About ObjectRef
	// Act is the op being proposed. Nil for a question — that is the difference.
	Act *Op
	// Slots are what the user fills or edits. May be empty (a plain yes/no).
	Slots []TurnSlot
	// Discriminator is what distinguishes THIS turn from another turn about the same object and the
	// same op. Set it whenever your opcode is a CHANNEL rather than a specific act.
	//
	// ⭐ IT IS A FIELD, NOT JUST A Key ARGUMENT, BECAUSE IDENTITY MUST TRAVEL WITH THE VALUE. A
	// deliverer that receives only a Turn can then key it correctly; when this lived solely as a Key
	// parameter, the transport had to be told separately — two things that must agree, and the kind of
	// coupling the review standard forbids.
	//
	// ⚠️ THE COLLISION IT PREVENTS WAS LIVE IN PRODUCTION. Every question from one ops agent rode the
	// one `ask_user` opcode, so "what date?" and "who does this go to?" produced the SAME key;
	// the backend upserted ON CONFLICT, so the second silently overwrote the first and the user was
	// never asked it.
	//
	// ⚠️ It is an IDENTITY, not a nonce: STABLE across runs, and NEVER derived from the wording — a
	// re-worded turn about the same thing must REFRESH the open row, not stack beside it. Use the
	// withheld capability's name, or a stable slug for the kind of question:
	//
	//	turn.Discriminator = "withheld:" + call.Name
	//	turn.Discriminator = "asking:value"
	Discriminator string
}

// NewQuestion builds a turn about INFORMATION. No act: the reply returns as context.
func NewQuestion(body string, about ObjectRef, slots ...TurnSlot) (Turn, error) {
	t := Turn{Kind: TurnQuestion, Body: body, About: about, Slots: slots}
	return t, t.validate()
}

// NewQuestionVia builds a question DELIVERED OVER a backend channel op.
//
// ⭐ WHY THIS EXISTS. The kit models question-vs-proposal as absence-vs-presence of an act. Real
// backends model it as WHICH OPCODE: the reference backend had exactly one channel for "ask the user and
// route the answer back", and that channel IS an op (`ask_user`). So a pure question must carry an
// opcode to be deliverable — and NewQuestion rejects that ("a question carries no act"), while
// NewProposal would make it a PROPOSAL and show the user an approve/reject affordance for something
// that is not an act.
//
// BOTH production consumers had to violate the invariant to use the type. Two of two is the kit being
// wrong about the world, not the consumers being unusual.
//
//	turn, err := agentkit.NewQuestionVia(body, ref, agentkit.Op{Opcode: "ask_user"})
//
// ⚠️ The op here is a TRANSPORT detail, not the subject of the turn. Kind stays TurnQuestion, so the
// reply is an ANSWER (context for the next run), never an approval. If the user is meant to bless an
// ACT, that is NewProposal — the difference is what the reply MEANS, and it is not cosmetic.
func NewQuestionVia(body string, about ObjectRef, channel Op, slots ...TurnSlot) (Turn, error) {
	t := Turn{Kind: TurnQuestion, Body: body, About: about, Act: &channel, Slots: slots}
	return t, t.validate()
}

// NewProposal builds a turn about an ACT. The reply releases it, drops it, or hands it back.
//
// act is in the BACKEND's vocabulary (see Op). A capability the tier withheld becomes a proposal by
// the agent mapping it to its backend's op — the kit does not and cannot do that mapping.
func NewProposal(body string, about ObjectRef, act Op, slots ...TurnSlot) (Turn, error) {
	t := Turn{Kind: TurnProposal, Body: body, About: about, Act: &act, Slots: slots}
	return t, t.validate()
}

// validate rejects the turns that cannot reach a human usefully.
func (t Turn) validate() error {
	var errs []error
	if strings.TrimSpace(t.Body) == "" {
		errs = append(errs, errors.New("agentkit: a turn with no body shows the user a blank ask"))
	}
	if t.About.IsZero() {
		errs = append(errs, errors.New("agentkit: a turn with no anchor lands on no object and vanishes from the UI"))
	}
	if t.Kind == TurnProposal && (t.Act == nil || t.Act.Opcode == "") {
		errs = append(errs, errors.New("agentkit: a proposal must carry the act it is about — without one it is a question"))
	}
	// ⚠️ A question MAY carry an op — but only as a delivery CHANNEL (NewQuestionVia). What makes it a
	// question is Kind, which decides what the reply MEANS: an answer, never an approval. Both
	// production consumers deliver questions over a backend op, so rejecting that outright made the
	// type unusable for its actual purpose.
	if t.Kind == TurnQuestion && t.Act != nil && t.Act.Opcode == "" {
		errs = append(errs, errors.New("agentkit: a question's channel op has no opcode — omit the op "+
			"entirely (NewQuestion) or name the channel (NewQuestionVia)"))
	}
	return errors.Join(errs...)
}

// Key is the turn's identity for de-duplication: stable per (agent, object, act) and deliberately
// EXCLUDING the wording.
//
// ⭐ A re-worded turn about the same act is the SAME turn and must REFRESH the open one in place.
// Keying on the body — the obvious mistake — gives a better-worded turn a different key, so it piles
// up beside the stale one.
//
// ⚠️ The kit DERIVES the identity; the delivery ENFORCES it (a unique index, an upsert). A transport
// that ignores the key WILL double-deliver.
// ⭐ PASS A DISCRIMINATOR whenever your opcode is a CHANNEL rather than a specific act. Both
// production consumers were bitten by its absence, from different directions:
//
//	an ops agent:      every tier-withheld act rides one opcode  → send_reply and archive_item shared a key
//	a planning agent:  every open question rides ask_user  → two unrelated questions shared a key
//
// A backend that upserts ON CONFLICT on this key silently OVERWRITES the first with the second, and the
// user is never asked. Pass what actually distinguishes the two — the withheld capability's name, or a
// stable slug for the question:
//
//	turn.Key("ops", "withheld:"+call.Name)
//	turn.Key("triage", "scope_cut")
//
// ⚠️ It must be STABLE across runs (it is an identity, not a nonce) and must NOT be derived from the
// wording — a re-worded ask about the same thing has to REFRESH the open row, not stack beside it.
//
// ⭐ PREFER THE FIELD (Turn.Discriminator) — it travels with the value, so a generic deliverer can key
// the turn from the turn alone. The variadic argument remains for the call-site form and for callers
// that legitimately key one turn two ways.
//
// ⚠️ PRECEDENCE: an explicit argument WINS over the field. Deliberate — the argument is the more
// specific, more local statement, and silently ignoring what a caller passed would be the worse
// failure. They are NOT concatenated: combining them would make the key depend on how it was
// requested, and a key that varies by call site is not an identity.
func (t Turn) Key(agent string, discriminator ...string) string {
	act := "question"
	if t.Act != nil {
		act = t.Act.Opcode
	}
	if len(discriminator) == 0 && t.Discriminator != "" {
		discriminator = []string{t.Discriminator}
	}
	return AskKey(agent, t.About.Kind, t.About.ID, act, discriminator...)
}

// Reply is what the user answers with.
type Reply int

const (
	// ReplyAnswer answers a question. The value returns to the agent as context.
	ReplyAnswer Reply = iota
	// ReplyApprove releases the proposed act.
	ReplyApprove
	// ReplyReject drops the proposed act. "Do not do this."
	ReplyReject
	// ReplyRedirect hands the act BACK to the agent with new information: "do it differently", or
	// "do this instead".
	//
	// ⛔ It is NOT a rejection. The user is not declining — they are redirecting, and what happens
	// next is the AGENT's decision, informed. Recording it as a reject makes a correction-suppression
	// mechanism silence the very re-proposal the user just asked for.
	ReplyRedirect
)

func (r Reply) String() string {
	switch r {
	case ReplyApprove:
		return "approve"
	case ReplyReject:
		return "reject"
	case ReplyRedirect:
		return "redirect"
	default:
		return "answer"
	}
}

// ReleasesAct reports whether this reply lets the proposed act happen. Only approval does.
func (r Reply) ReleasesAct() bool { return r == ReplyApprove }

// IsRejection reports whether the user declined the act. ⭐ A redirect is NOT a rejection.
func (r Reply) IsRejection() bool { return r == ReplyReject }

// HandsBackToAgent reports whether the outcome is the AGENT's to decide, informed by the reply.
// True for a redirect: the user supplied information, not a decision about the act.
func (r Reply) HandsBackToAgent() bool { return r == ReplyRedirect }

// RenderDialogue is the prompt block telling the model it can speak to the user.
//
// ⭐ Dialogue is NOT a capability, so it is absent from RenderCapabilities — but something must still
// reach the prompt or the option is invisible to the model. "Not a capability" is about the GATE,
// not the PROMPT.
//
// The kit owns HOW to speak (this wording, so every agent's turns are well-formed); the agent owns
// WHEN and about what.
func RenderDialogue() string {
	return strings.TrimSpace(`
You may speak to the user when you cannot proceed alone. Two ways, and the difference is what you
are speaking ABOUT:

- question — you need INFORMATION only they have. Ask one focused question; their answer comes back
  to you as context. Use this when nothing is pending on your side.
- proposal — you want to perform an ACT you may not do alone. State what you would do, plainly, so
  they can approve it in one move. They may approve it, reject it, or tell you to do it differently.

Speak only when you genuinely cannot continue, or when the decision is theirs to make. Do as much as
you can FIRST, then ask for the one thing that is left.`) + "\n"
}

// String renders a turn for logs and debugging.
func (t Turn) String() string {
	if t.Act != nil {
		return fmt.Sprintf("%s(%s on %s:%s)", t.Kind, t.Act.Opcode, t.About.Kind, t.About.ID)
	}
	return fmt.Sprintf("%s(%s:%s)", t.Kind, t.About.Kind, t.About.ID)
}
