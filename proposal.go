package agentkit

import (
	"encoding/json"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// THE PROPOSAL — an agent's drafted decision, held out for the human to bless in ONE move.
//
// ## Why this is in the KIT and not in an agent
//
// Two agents in the originating codebase built proposals, and had INDEPENDENTLY re-implemented the same
// machinery: the request assembly, the re-trigger self-note, the dedupe key, the archetype derivation, the
// surface/source constants. A third was coming. That is the bespoke-workaround anti-pattern this package
// exists to prevent — and a shared `Proposal` had already been listed as a thing to REUSE. It was planned
// and never built.
//
// ## Why the kit does NOT import the backend's own proposal type
//
// The kit has answered this twice already (capability.go: "agentkit must NOT import the backend … the kit
// never learns what a 'project' is"; tier.go: the same for Tier/AutonomyRung). A backend's own
// UpsertProposalRequest carries that backend's OWN column vocabulary — tenant, owner, surface, source,
// archetype, dedupe key, all CHECK-constrained in its migrations. A kit that spoke it would BE a client of
// that backend, and would wedge the moment a proposal needs a different sink.
//
// So the kit owns a NEUTRAL Proposal, and each agent keeps a thin mapper to its backend's type. Map at the
// seam — the pattern the kit already prescribes.
//
// ## ⭐ The invariant Build exists to guarantee
//
//	EVERY PROPOSAL NAMES WHAT IT ACTS ON.
//
// A proposal is a decision the user is asked to BLESS. Measured on a real deployment, the large
// majority could not be blessed:
//
//	"I'll add your existing todo 'Draft the launch checklist' to THIS PROJECT — approve."   ×4, each a different project
//	"I'll mark it complete — your task 'Review and patch vulnerabilities'."          ← on WHAT board?
//
// Four identical Approve buttons acting on invisible objects is asking someone to sign a blank cheque.
// Build is the ONE choke point every proposal passes through, so no agent CAN ship an unnamed ask.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// ObjectRef is one object a proposal touches: its id, and the label it has RIGHT NOW.
//
// The label is resolved BY GO at build time (via Labeler), never authored by the model. That is the whole
// trustworthiness argument: the agent never writes a name, so it can never get one wrong — no typo, no
// stale name, no confabulation.
// ⚠️ BUILD ONE WITH Ref(labeler, kind, id) IF YOU TOKENIZE. Writing the struct literal
// ObjectRef{Kind: "task", ID: id} — which this field list openly invites — leaves Label EMPTY, and an
// empty Label makes TokenizeQuestion's naming guarantee SILENTLY NO-OP (proposal_question.go returns
// the question untouched). No error, no log: the ask just never names what it acts on.
//
// ⭐ AN EMPTY Label IS ONLY A BUG IF YOU TOKENIZE. If your agent writes its own sentences and never
// calls TokenizeQuestion, the label is unused and the literal is CORRECT — one of the reference agents
// does exactly this on purpose. That is why this is a documented trap rather than a rejected value: making
// it an error would break a correct agent. Use HasResolvedLabel to assert it where you DO need it.
type ObjectRef struct {
	Kind  string // TokenTask | TokenProject | TokenGoal | TokenSignal | TokenMilestone
	ID    string
	Label string // the LIVE name. Resolved by Go. Never model-authored. See the trap above.
}

// IsZero reports an unset ref (no object).
func (r ObjectRef) IsZero() bool { return r.Kind == "" || r.ID == "" }

// HasResolvedLabel reports whether this ref carries a Go-resolved display name — i.e. whether
// TokenizeQuestion can actually name the object in a sentence.
//
// ⭐ ASSERT THIS IN YOUR TESTS IF YOUR AGENT TOKENIZES. It is the one-line form of the trap above: a
// ref built with Ref() has a label; a struct literal usually does not, and the difference is
// invisible at runtime because the naming step fails OPEN (an unnamed ask still reaches the user, it
// just says "this project" instead of the project's name).
func (r ObjectRef) HasResolvedLabel() bool { return !r.IsZero() && r.Label != "" }

// ProposalOp is one frozen instruction: an opcode + the agent-frozen args. The backend's op-engine
// verifies and replays exactly this. It is deliberately BACKEND-INTERNAL — the client never sees it (the
// web app draws that boundary explicitly), which is why Go, not the client, composes the sentence.
type ProposalOp struct {
	Opcode string
	Args   map[string]string
}

// UserSlot is a value the user fills on accept, bound to one op by index.
type UserSlot struct {
	OpIndex  int
	Operand  string
	Type     string // date | text | number | select
	Label    string
	Required bool
	Options  []string // for type=select
	Default  string   // the agent's DRAFTED value — pre-filled and editable. Empty+Required ⇒ an "input" ask.
}

// Proposal is the kit's neutral, fully-anchored ask.
type Proposal struct {
	// Question is the human sentence — TOKENIZED by Build. Never trust what the model wrote: it may name
	// nothing ("this project"), name it wrongly, or half-emit a token. Build repairs all three.
	Question string

	// Object is THE anchor: what the ops act on. (The proposal's object_type + object_id, plus its label.)
	Object ObjectRef

	Ops       []ProposalOp
	UserSlots []UserSlot

	// Objects are the OTHER objects the ask names — the destination of a move, and anything else the
	// sentence points at. Carries the labels the client needs for chips and links.
	Objects []ObjectRef

	// Context is where this decision LIVES (today: the project). It is NOT part of the action — nothing
	// happens to it — but without it a task-anchored ask lands on no card at all and vanishes from the
	// dashboard. "Which board" is a fact, not an inference.
	Context ObjectRef

	// Metadata is the agent's bag (its re-trigger self-note, lifecycle conditions, format version). The kit
	// carries it opaquely: WHICH keys mean what is agent identity and a wire contract with the backend.
	Metadata map[string]json.RawMessage
}

// Proposal surfaces — where the ask lands. (The backend CHECK-constrains these.)
const (
	SurfaceNeedsYou   = "needs_you"  // this genuinely needs the human. An agent's asks are almost always this.
	SurfaceSuggestion = "suggestion" // FYI; take it or leave it
	SurfaceReview     = "review"
)

// SourceAssistant marks an assistant-generated proposal, as opposed to one a
// human created. (NOT an agent's provenance stamp — that is a different field.
// Sending the stamp here fails the backend's CHECK, which is a silent drop.)
const SourceAssistant = "assistant"

// Proposal archetypes — what KIND of act the ask is. Derived from shape, never hand-set (DeriveArchetype).
const (
	ArchetypeDraft = "draft" // the agent HAS a decision → one-tap Approve (+ optional editable fields)
	ArchetypeInput = "input" // the agent needs info only the user has → a field + Submit; nothing to approve
)

// IsValid reports whether a proposal is well-formed enough to ship: a question, an anchor, and an op.
// A proposal that fails this is something an agent half-emitted; the backend would reject it, so the agent
// drops it instead (a half-formed ask is worse than no ask).
func (p Proposal) IsValid() bool {
	if strings.TrimSpace(p.Question) == "" || p.Object.ID == "" || len(p.Ops) == 0 {
		return false
	}
	return p.Ops[0].Opcode != ""
}

// DeriveArchetype classifies a proposal as the ACT it is, FROM ITS SHAPE:
//
//   - "input" — it has a REQUIRED slot with an EMPTY default: the agent is asking for something only the
//     user knows. There is nothing to approve; the UI renders a field + Submit.
//   - "draft" — everything else: no slots (a pure one-tap approve), or slots carrying a drafted default the
//     user blesses or edits.
//
// Deriving from shape (rather than a hand-set flag per builder) keeps the archetype HONEST — it cannot
// drift from what the proposal actually asks. The one judgment an agent cannot fake is "did I fill in a
// default?".
func DeriveArchetype(p Proposal) string {
	for _, s := range p.UserSlots {
		if s.Required && strings.TrimSpace(s.Default) == "" {
			return ArchetypeInput
		}
	}
	return ArchetypeDraft
}

// DedupeKey is the backend-side idempotency key: stable per (agent, object, op) — and deliberately NOT
// including the question.
//
// ⭐ THIS IS REPLACE-WITH-BETTER. A reworded or freshly-tokenized draft of the SAME ask (same op, same
// object) collides with the open row and REFRESHES it in place, instead of piling a second, staler copy
// beside it. Including the question — the obvious mistake — means a better-worded ask gets a different key
// and accumulates next to the old one. The ask's IDENTITY is "what op on what object", not its wording.
//
// `agent` is the namespace ("triage", "ops") — an agent's asks never collide with another's.
func DedupeKey(agent string, p Proposal) string {
	op := ""
	if len(p.Ops) > 0 {
		op = p.Ops[0].Opcode
	}
	return AskKey(agent, p.Object.Kind, p.Object.ID, op)
}

// AskKey is DedupeKey over the values a consumer already has, without requiring the kit's Proposal
// type. Same key, same rules — this is the callable form.
//
// ⭐ DedupeKey took a Proposal, which NO consumer builds (each hand-builds its backend's request
// type). So the reference agents re-implemented this function instead: one was line-for-line
// identical, another concatenated the same format inline. A helper that takes a type nobody has is a
// helper nobody can call.
//
// `discriminator` distinguishes asks that share an object AND an opcode but are NOT the same ask —
// e.g. an ops agent surfaces every tier-withheld act through the one ask_user opcode, so "send
// this reply?" and "archive this?" would otherwise collide and silently replace one another. Pass ""
// when the op alone identifies the ask (the common case).
func AskKey(agent, objectKind, objectID, op string, discriminator ...string) string {
	key := agent + ":" + objectKind + ":" + objectID + ":" + op
	for _, d := range discriminator {
		if d != "" {
			key += ":" + d
		}
	}
	return key
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ⭐ THE WILL-CHANGE GATE — a proposal is warranted IFF its op would CHANGE state. Don't mint one whose
// effect the world already holds (the op would degenerate to skip).
//
// ## The formal frame (STRIPS-style effect specification)
//
// Model each op as an EFFECT SPEC (Pre, Add, Del) — equivalently a postcondition Q_op. Applying the op does
// nothing exactly when its post-state is already a FIXPOINT of the current state:
//
//	WillChange(op, params, state) = ¬( Add(op,params) ⊆ state ∧ Del(op,params) ∩ state = ∅ )
//
// i.e. the op is a no-op precisely where its own postcondition already holds (wp(skip,Q)=Q). For a scalar
// "set X = v" op this collapses to state.X != v; for a membership op to "the link does not yet exist"; for a
// lifecycle op to "not already terminal." A proposal is minted iff its op is APPLICABLE (Pre ⊆ state) AND
// CHANGING (¬fixpoint).
//
// ## The bug this exists to kill
//
// An agent proposes "I'll add your todo to this project — approve." The user approves; the todo is now IN
// the project. Next run the agent proposes it AGAIN — nothing checked that applying it would change nothing.
// In one measured case this produced FIVE identical add-to-collection asks for one item (four accepted,
// one still pending),
// each re-minted after the last was applied. A dedupe KEY cannot stop it: the backend's idempotency index is
// scoped to PENDING rows, so once an ask is accepted it leaves the index and the next identical ask inserts
// fresh. Only asking "would this op change state?" — a fact, not a string — closes it.
//
// ## Deterministic (has an effect spec) vs. feedback (does not)
//
// Most ops are DETERMINISTIC: their effect is a state fact (add_to_project → membership; complete_task →
// status; set_task_due_date → the due date). WillChange compares the op's effect to current state. A few ops
// are FEEDBACK (ask_user) or CREATES (create_task/create_goal): they have NO computable post-state to
// compare against (prose, or a not-yet-existent target), so WillChange is always true — always mint; their
// retirement is the semantic judge's / the PULL backstop's job, never this gate's.
//
// ## Why the seam, not the check, lives in the kit
//
// The kit cannot read the world — it "never learns what a 'project' is" (capability.go). So it owns the GATE
// (drop the no-ops) and the SEAM (ChangeEvaluator); the agent/backend owns the concrete effect-vs-state read
// (per-op field compares, subsuming the terminal cases of a premise-death check). Mirror of
// DeriveArchetype: the kit decides the RULE, the consumer supplies the FACT. It gives the MINT decision one
// uniform home (was ad hoc per agent); it does NOT replace the retire-hooks / read-time filters that handle
// ALREADY-PENDING proposals — those are complementary nets on the same "proposal = function of state" idea.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// ChangeEvaluator answers, for a would-be proposal, whether applying its op would CHANGE state — so an op
// that would change nothing (its effect already holds) is dropped rather than minted. The kit cannot answer
// this (it holds no state); the consumer (your backend) implements it by reading current state and comparing
// to the op's declared effect.
//
// Contract: return FALSE only for a DETERMINISTIC op whose effect ALREADY holds (the todo is already in the
// project; the task's due date already equals the proposed one). A FEEDBACK op (ask_user), a CREATE op,
// or any op with no computable post-state MUST return true (always mint). An error returns true (fail-open:
// never silently drop an ask because a state read hiccuped) — the caller keeps the proposal.
type ChangeEvaluator interface {
	WillChange(op ProposalOp, object ObjectRef) (bool, error)
}

// FilterNoOpProposals drops the proposals whose op would change nothing (per the evaluator), keeping the
// rest. A nil evaluator is a pass-through (no state to consult → keep everything). Fail-OPEN by contract: an
// evaluator error keeps the proposal (an ask wrongly shown is recoverable; an ask wrongly dropped is silent).
//
// STATUS: this is the AGENT-SIDE pre-send optimization (skip the round-trip for an obviously-moot ask). The
// ENFORCEMENT belongs in the backend's own upsert path, which should be live and unconditional — so this
// filter is not load-bearing on its own. It ships with a standalone table test (proposal_test.go) as a
// validated kit primitive; wiring it into a consumer's pre-send path is the consumer's follow-up, not a
// correctness gap here.
func FilterNoOpProposals(eval ChangeEvaluator, ps []Proposal) []Proposal {
	if eval == nil {
		return ps
	}
	out := ps[:0]
	for _, p := range ps {
		if len(p.Ops) == 0 {
			out = append(out, p) // nothing to evaluate — let IsValid handle a malformed ask
			continue
		}
		changes, err := eval.WillChange(p.Ops[0], p.Object)
		if err != nil || changes {
			out = append(out, p) // fail-open, or genuinely still-warranted (the op would change state)
		}
	}
	return out
}

// MaxProposals caps how many asks one run may surface — the generic flood guard.
//
// An agent that surfaces twelve decisions has not helped; it has moved its backlog onto the human. The rest
// re-surface next run (the dedupe key makes that free), so nothing is lost — only deferred.
//
// This is the GENERIC cap. An agent may ALSO have a domain-specific one it applies first (the PM ranks its
// project-definition asks and keeps only the top one) — that ranking is identity and stays in the agent.
func MaxProposals(ps []Proposal, n int) []Proposal {
	if n <= 0 || len(ps) <= n {
		return ps
	}
	return ps[:n]
}
