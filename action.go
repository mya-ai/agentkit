package agentkit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ACTIONSET — what the agent may PROPOSE, declared exactly like what it may DO.
//
//	ToolSet    — what the agent DOES:     Add(capability, handler)  → the handler PERFORMS it
//	ActionSet  — what it may PROPOSE:     Add(action, stateReader)  → the kit decides IF IT CHANGES
//
// Same shape, so learning one teaches the other. Two DIFFERENT checks fall out of one declaration,
// and only the second stops an agent re-proposing:
//
//	1. VerifyAction  — is this opcode declared, are its operands real and present?   FAILS CLOSED
//	2. WillChange    — would applying it actually CHANGE anything?                   FAILS OPEN
//
// ⭐ A DEDUPE KEY CANNOT DO THE SECOND. A key catches a REPEAT (the same ask sent twice); it cannot
// catch an ask that was never WARRANTED. "Set the deadline to Aug 31" on a project whose deadline IS
// Aug 31 is perfectly well-formed, has a stable key, and is pure noise.
//
// ⭐ THE STATE LOGIC IS A DECLARATION, NOT A COMPARATOR. You state the action's EFFECT — its Category
// plus which operand it writes — and the kit GENERATES the rule. You supply only the one thing the
// kit cannot know: the CURRENT value. The kit never sees your project, your task, or your database;
// it gets a string back and compares.
//
// The formalism is a STRIPS effect spec: each op declares (Pre, Add, Del), and
//
//	WillChange(op, s)  ≡  ¬fixpoint(op, s)  ≡  ¬( Add ⊆ s ∧ Del ∩ s = ∅ )
//
// Every Category below is a DERIVATION of that line, not a rule of thumb — see each one's doc. This
// is why the subtle cases cannot be forgotten: they are consequences, not conventions.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// ChangeCategory is the SHAPE of an action's effect. It is what lets the kit derive WillChange, so
// picking it correctly is the whole job of declaring an action.
//
// ⭐ Choosing between these is a CLASSIFICATION, which is a far easier judgment than writing a
// comparator — and a wrong choice is caught by a test, where a wrong comparator is silent.
type ChangeCategory int

const (
	// ChangeUnset is the ZERO VALUE, and it is INVALID — Validate rejects it.
	//
	// ⭐ IT EXISTS SO A FORGOTTEN Category CANNOT BE A SILENT DEFAULT. Before it, the zero value was
	// ChangeAlwaysMints, so omitting the field classified the act as "no computable post-state ⇒
	// always mint" — and the proposal RE-MINTED FOREVER, which is the exact defect ActionSet exists to
	// close. Bind, Validate, VerifyActionSet and VerifyActionSetAgainst all passed it.
	//
	// ⚠️ WORSE, the harness DISCARDED the developer's own contrary intent: VerifyActionSetAgainst
	// skips any category that needs no state reader, so supplying a fixpoint state — the act of
	// saying "this must NOT mint here" — was silently ignored for exactly these acts.
	//
	// Same argument the kit already makes for TierReadOnlyExplicit (tier.go:51): "this may auto-run"
	// and "this always asks" must be STATEMENTS, never OMISSIONS. Unlike Tier, ChangeCategory is
	// pinned to no wire contract, so the zero value itself could be made invalid.
	ChangeUnset ChangeCategory = iota

	// ChangeAlwaysMints is for an action with NO COMPUTABLE POST-STATE: a create (there is no prior
	// object to compare against) or feedback such as a question (it has no state effect at all).
	//
	// Derivation: Add is undefined ⇒ there is no fixpoint ⇒ always mint. Needs NO state reader.
	ChangeAlwaysMints

	// ChangeScalarSet is the common case: the action sets ONE field to ONE value ("set the due date
	// to Aug 21"). Declare Input as the operand carrying the value; the reader returns the current
	// value of that field.
	//
	// Derivation: Add = {X = v}, so the state is a fixpoint exactly when s.X == v ⇒
	// WillChange = (s.X != v).
	//
	// ⭐ With one consequence you get for free: an EMPTY proposed value means the user fills it on
	// accept, so Add is UNDETERMINED and the state cannot be a fixpoint of it ⇒ always mint. This is
	// why "set the due date — you pick" is always shown while "set it to Aug 21" is skipped when it
	// already is Aug 21.
	ChangeScalarSet

	// ChangeMembership is a link/join action ("add this task to that project"). Declare Input as the
	// operand naming the other end; the reader returns the currently-linked id ("" if unlinked).
	//
	// Derivation: Add = {linked(a,b)} ⇒ fixpoint iff already linked ⇒ WillChange = ¬linked.
	ChangeMembership

	// ChangeTerminal is a lifecycle action that moves an object into a final state (archive, complete).
	// Declare TerminalStates; the reader returns the object's current status.
	//
	// Derivation: Add = {status ∈ terminal} ⇒ fixpoint iff already terminal ⇒
	// WillChange = ¬isTerminal(s.status).
	ChangeTerminal

	// ChangeMultiField is an action that writes SEVERAL fields independently, each optional
	// (update_goal: status + health + narrative). It changes state iff ANY PROVIDED field changes.
	//
	// ⚠️ NOTE THE ASYMMETRY WITH ChangeScalarSet — this is exactly why the two are different
	// categories and why forcing a multi-field op into ScalarSet would be provably wrong:
	//
	//	ScalarSet:  an EMPTY value = "user input, undecided"  → MINT
	//	MultiField: an EMPTY field = "not being set"          → IGNORED (contributes nothing)
	//
	// Same symbol, opposite meaning, because the Add set differs.
	//
	// ⛔ REGISTER IT WITH AddMultiField, NOT Add — it needs a FieldReader (which receives the operand
	// name), because each field is compared against ITS OWN current value. A single reader shared
	// across N operands silently drops a real change whenever the other provided fields happen to
	// equal the one value it returns. Validate rejects a MultiField action registered via Add.
	ChangeMultiField

	// ChangeFillsSlot is a QUESTION that asks the user to supply a value: "what due date should I
	// use?", "which vendor did you pick?". Declare Input as the field the answer FILLS; the reader
	// returns that field's current value.
	//
	// Derivation: the question's premise is not its own effect — it is whether the slot is still
	// EMPTY. So Add = {X is known} ⇒ fixpoint iff s.X is already set ⇒ WillChange = (s.X == "").
	//
	// ⭐ THIS IS THE CATEGORY THAT MAKES DE-DUPLICATING DIALOGUE POSSIBLE, and it exists because two
	// correct rules collide. A question asking for a value carries an EMPTY Input by construction —
	// that is what makes it a question — so under ChangeScalarSet the empty-value rule fires first
	// and always mints, leaving the fixpoint check unreachable for exactly the shape most in need of
	// de-duplication. The user answers, and the agent asks again next run.
	//
	//	ChangeScalarSet  (a PROPOSAL): empty value ⇒ the user picks  ⇒ ALWAYS MINT
	//	ChangeFillsSlot  (a QUESTION): the value EXISTS ⇒ answered   ⇒ NEVER ASK
	//
	// ⚠️ The carried value is IRRELEVANT here. A proposal's premise is "does the field already equal
	// MY value?"; a question's is "does the field have ANY value yet?" — because the user, not the
	// agent, supplies it.
	ChangeFillsSlot

	// ChangeMerge is a subset/merge write (set_project_metadata) whose post-state is not modelled.
	//
	// Derivation: Add is not computable in v1 ⇒ conservatively always mint. Needs NO state reader.
	// ⚠️ This is a v1 LIMITATION, not a permanent truth — tightening it later changes no consumer.
	ChangeMerge
)

func (c ChangeCategory) String() string {
	switch c {
	case ChangeUnset:
		// ⚠️ NOT "always_mints". ChangeUnset is the INVALID zero value, and naming it after a real
		// category made Validate contradict itself for a forgotten field: "is category always_mints,
		// which compares against current state" — always_mints is precisely the category that does
		// NOT compare against state. The developer went hunting for a missing reader instead of
		// declaring a Category.
		return "unset"
	case ChangeScalarSet:
		return "scalar_set"
	case ChangeMembership:
		return "membership"
	case ChangeTerminal:
		return "terminal"
	case ChangeMultiField:
		return "multi_field"
	case ChangeFillsSlot:
		return "fills_slot"
	case ChangeMerge:
		return "merge"
	default:
		return "always_mints"
	}
}

// needsStateReader reports whether this category's derivation requires reading current state. The
// two "always mint" categories do not — their Add effect is undefined, so there is nothing to compare.
//
// ⚠️ ChangeUnset reports FALSE, but for a different reason: it is the invalid zero value, so there is
// no derivation at all. Reporting true made Validate demand a reader for a category the developer had
// not declared yet, burying the one actionable error ("declares no Category") under a spurious second
// one. The missing Category is the only thing worth telling them.
func (c ChangeCategory) needsStateReader() bool {
	switch c {
	case ChangeUnset, ChangeAlwaysMints, ChangeMerge:
		return false
	default:
		return true
	}
}

// Operand declares one field an action accepts. Mirrors CapabilityArg deliberately: same idea, other
// vocabulary — a capability's args are filled by the MODEL, an action's operands may be filled by the
// USER on accept (see Action.UserFills).
type Operand struct {
	Name        string
	Description string   // agent-facing: what it means and the expected format
	Type        string   // string | date | number | enum — drives the UI control
	Required    bool     // must be present, UNLESS declared in Action.UserFills
	EnumValues  []string // for Type=="enum"
	// List marks a MULTI-valued operand (the model may emit a JSON array). ⚠️ Without it,
	// CapabilitiesFrom rendered a multi-value operand to the model as "One of: a, b, c" and then
	// VerifyCall REJECTED a correct multi-value emission — the "model was right, the seam was wrong"
	// failure. Mirrors CapabilityArg.List; the bridge carries it.
	List bool
}

// Action is one act the agent may PROPOSE: the backend's opcode, its operands, and — crucially — the
// declared SHAPE of its effect, from which the kit generates WillChange.
//
// ⭐ You state WHAT the action does to state; the kit works out WHETHER it would change anything.
type Action struct {
	// Opcode is the backend's name for the act. The kit never interprets it.
	Opcode string
	// Description is why/when to propose it — read by the model, like a capability's.
	Description string
	// Tier is the reversibility class, same meaning as a capability's.
	Tier Tier

	// Operands are the fields this action accepts.
	Operands []Operand

	// Category is the SHAPE of the effect — what the kit derives the rule from.
	Category ChangeCategory
	// Input names the operand carrying the value being written. Required for ChangeScalarSet and
	// ChangeMembership; ignored by the others.
	Input string
	// TerminalStates lists the statuses that mean "already there" for ChangeTerminal.
	TerminalStates []string

	// Structured names operands the backend parses as JSON, so they can never be a free-text user
	// slot. ⚠️ Agent-declared: a wire `type` is string|date|number|enum, so JSON-ness is not fetchable.
	Structured []string
	// Ask is the wording for a withheld act, carried from the declaration so a consumer surfacing
	// plan.Propose can render it without a second lookup. Nil when the act is never surfaced.
	Ask func(Action) string
	// Extra carries agent-private facts the kit never interprets (priority, anchor arg, invalidation
	// triggers). Read it back with ExtraOf.
	Extra any

	// Args are the operand values, filled when the agent EMITS this action. Empty on a declaration.
	Args map[string]string
	// UserFills names operands the USER supplies on accept. A required operand listed here is
	// legitimately absent at verify time — without this waiver every "you pick the date" ask would
	// fail verification.
	UserFills []string
}

// Arg returns the named operand's emitted value (empty if absent).
func (a Action) Arg(name string) string { return a.Args[name] }

// StateReader returns the CURRENT value the action's effect is compared against — the one thing the
// kit cannot know. S is your agent's per-run state, so the common case is a struct-field read that
// costs nothing (your gather already loaded it).
//
// ⭐ IT RECEIVES THE ACT, so the reader knows WHICH object to read. Without it a reader could only
// consult ambient state, and an agent whose run touches several tasks had to thread a mutable
// "current target" field — forget to set it and you compare the WRONG object, which fails CLOSED
// (silently dropping a legitimate ask). The args carry the target id, so the reader resolves per call:
//
//	func(_ context.Context, s *State, a agentkit.Action) (string, error) {
//	    return s.Task(a.Arg("task_id")).DueDate, nil
//	}
//
// ⚠️ Do NOT add reads here to pre-empt the server. This is a pre-check; the backend's own gate is the
// authority. An error fails OPEN (the turn is kept), so partial adoption is safe.
type StateReader[S any] func(ctx context.Context, state *S, act Action) (string, error)

// FieldReader is StateReader for a MULTI-FIELD action: it receives the OPERAND NAME and returns that
// field's current value.
//
// ⭐ The operand name is what makes ChangeMultiField sound. Each provided field must be compared
// against ITS OWN current value — mirroring a hand-written gate, which calls its storage once per
// column ("status", then "health"). Sharing ONE reader across
// N operands compares every field to the same string, which silently reports "no change" whenever
// the other provided fields happen to equal it. That is a fail-CLOSED drop on a check whose whole
// contract is fail-OPEN.
type FieldReader[S any] func(ctx context.Context, state *S, act Action, field string) (string, error)

// ActionSet is an agent's proposable-action set: each action's declaration plus the state reader its
// category needs. Construct with NewActionSet.
type ActionSet[S any] struct {
	actions []Action
	readers map[string]StateReader[S]
	fields  map[string]FieldReader[S]
}

// NewActionSet returns an empty set. S is your agent's per-run state type.
func NewActionSet[S any]() *ActionSet[S] {
	return &ActionSet[S]{
		readers: make(map[string]StateReader[S]),
		fields:  make(map[string]FieldReader[S]),
	}
}

// Add declares one proposable action and the state reader its category needs. Returns the set, so
// declarations chain.
//
// Pass nil for read is CORRECT for ChangeAlwaysMints and ChangeMerge — there is nothing to compare.
// Every other category requires one, and Validate says so at wiring time.
func (as *ActionSet[S]) Add(a Action, read StateReader[S]) *ActionSet[S] {
	as.actions = append(as.actions, a)
	if read != nil {
		as.readers[a.Opcode] = read
	}
	return as
}

// AddMultiField declares a ChangeMultiField action, whose reader receives the OPERAND NAME so each
// field is compared against its own current value.
//
// ⛔ Use this — not Add — for ChangeMultiField. Validate rejects the wrong pairing in both
// directions, because sharing one reader across N operands silently drops real changes (see
// FieldReader).
func (as *ActionSet[S]) AddMultiField(a Action, read FieldReader[S]) *ActionSet[S] {
	as.actions = append(as.actions, a)
	if read != nil {
		as.fields[a.Opcode] = read
	}
	return as
}

// Actions returns the declarations — for RenderActions, so the model's action list is generated from
// the SAME declaration the state logic hangs off. Never hand-write an action list.
// ⭐ NIL IS A SUPPORTED STATE meaning "the empty set". Bind fails CLOSED, but a consumer whose
// proposals are only PART of its work legitimately degrades to "no agent-side validation; the backend
// still verifies at accept" rather than killing the run. Every method below tolerates a nil receiver
// so that path needs no hand-written guard funnel — one production consumer had already written one.
func (as *ActionSet[S]) Actions() []Action {
	if as == nil {
		return nil
	}
	return as.actions
}

// Lookup finds a declared action by opcode.
//
// ⭐ Exported because every consumer needs it and both hand-wrote it: a migrating agent wrote four
// O(n) scans over Actions() (~45 lines) purely because the set exposed no index. If the declaration
// is the single source, reading it back must not require rebuilding one.
func (as *ActionSet[S]) Lookup(opcode string) (Action, bool) {
	if as == nil {
		return Action{}, false // a nil set declares nothing — see the nil contract on Actions
	}
	for _, a := range as.actions {
		if a.Opcode == opcode {
			return a, true
		}
	}
	return Action{}, false
}

// VerifyAction reports whether an emitted action is WELL-FORMED: the opcode is declared, every
// supplied operand is real and in-range, and every required operand is present or user-filled.
//
// ⚠️ FAILS CLOSED — a malformed action must not reach a human (the malformed-ask-reaches-a-human class). This is the opposite
// default from WillChange, deliberately: "might be moot" is not "is broken".
//
// This is the agent-side FAST REJECT. The backend's own verifier at write time is the authority;
// passing here guarantees nothing, but failing here saves a round-trip and a rejected write.
func (as *ActionSet[S]) VerifyAction(act Action) error {
	if as == nil {
		// ⚠️ Still fails CLOSED when degraded: an opcode from a set that declares nothing is not
		// verifiable, and a malformed ask must never reach a human on the strength of an outage.
		return fmt.Errorf("agentkit: cannot verify %q — no actions are declared (a nil set means the "+
			"contract was unreachable; the backend still verifies at accept)", act.Opcode)
	}
	spec, ok := as.Lookup(act.Opcode)
	if !ok {
		return fmt.Errorf("agentkit: action %q is not declared (the model invented an opcode); declared: %s",
			act.Opcode, strings.Join(as.opcodes(), ", "))
	}

	userFills := make(map[string]bool, len(act.UserFills))
	for _, name := range act.UserFills {
		if !hasOperand(spec, name) {
			return fmt.Errorf("agentkit: %s: user-fill %q is not an operand of this action", act.Opcode, name)
		}
		userFills[name] = true
	}

	for name, val := range act.Args {
		o, ok := operandOf(spec, name)
		if !ok {
			return fmt.Errorf("agentkit: %s: unknown operand %q (the model invented one)", act.Opcode, name)
		}
		if err := checkOperandType(act.Opcode, o, val); err != nil {
			return err
		}
	}

	for _, o := range spec.Operands {
		if !o.Required || userFills[o.Name] {
			continue // the user supplies it on accept
		}
		if strings.TrimSpace(act.Args[o.Name]) == "" {
			return fmt.Errorf("agentkit: %s: missing required operand %q (absent and not user-filled)",
				act.Opcode, o.Name)
		}
	}
	return nil
}

// WillChange reports whether applying this action would actually CHANGE state — the check that stops
// an agent re-proposing what is already true.
//
// ⭐ The rule is GENERATED from the action's declared Category (see ChangeCategory); you never wrote
// it. The only thing consulted from your side is the state reader, and only for the categories whose
// derivation needs one.
//
// ⚠️ FAILS OPEN: a reader error returns (true, nil) — an ask wrongly shown is recoverable, an ask
// wrongly dropped is silent. An UNDECLARED action is a different matter and returns an error: silently
// minting would hide the invented-opcode case VerifyAction exists to catch.
func (as *ActionSet[S]) WillChange(ctx context.Context, state *S, act Action) (bool, error) {
	if as == nil {
		return false, fmt.Errorf("agentkit: cannot evaluate %q — no actions are declared (a nil set "+
			"means the contract was unreachable)", act.Opcode)
	}
	spec, ok := as.Lookup(act.Opcode)
	if !ok {
		return false, fmt.Errorf("agentkit: action %q is not declared — cannot evaluate its effect", act.Opcode)
	}

	// ⛔ An undeclared Category is a WIRING BUG, not a derivation. It must ERROR rather than fall
	// through to always-mint — that fall-through silently restored the exact defect ChangeUnset was
	// introduced to close (the old zero value was ChangeAlwaysMints, so a forgotten field re-proposed
	// the same ask forever). Bind calls Validate, so production consumers never reach this; a
	// hand-built set can, and it must be told.
	if spec.Category == ChangeUnset {
		return false, fmt.Errorf("agentkit: action %q declares no Category (the invalid zero value), "+
			"so its effect cannot be derived — call ActionSet.Validate at wiring time, and state the "+
			"shape of the effect on the declaration", act.Opcode)
	}

	// No computable post-state ⇒ no fixpoint ⇒ always mint. No reader consulted.
	if !spec.Category.needsStateReader() {
		return true, nil
	}

	// MULTI-FIELD: each provided field is compared against ITS OWN current value, via a reader that
	// receives the operand name. Changes iff ANY PROVIDED field differs. ⚠️ Unlike ScalarSet, an EMPTY
	// field means "not being set" and contributes nothing — it is NOT undecided user input.
	if spec.Category == ChangeMultiField {
		readField := as.fields[act.Opcode]
		if readField == nil {
			return true, nil // wiring mistake (Validate catches it); fail OPEN rather than drop a turn
		}
		for _, o := range spec.Operands {
			v, present := act.Args[o.Name]
			if !present || strings.TrimSpace(v) == "" {
				continue // not being set
			}
			current, err := readField(ctx, state, act, o.Name)
			if err != nil {
				return true, nil // fail open
			}
			if valuesDiffer(o.Type, v, current) {
				return true, nil
			}
		}
		return false, nil
	}

	read := as.readers[act.Opcode]
	if read == nil {
		// Declared as needing state but wired without a reader. Validate catches this at wiring time;
		// at run time we fail OPEN rather than drop a turn on a wiring mistake.
		return true, nil
	}

	switch spec.Category {
	case ChangeScalarSet, ChangeMembership:
		proposed := strings.TrimSpace(act.Arg(spec.Input))
		if proposed == "" {
			// Add is UNDETERMINED (the user fills it on accept) ⇒ the state cannot be a fixpoint of it.
			return true, nil
		}
		current, err := read(ctx, state, act)
		if err != nil {
			return true, nil // fail open
		}
		return valuesDiffer(operandTypeOf(spec, spec.Input), proposed, current), nil

	case ChangeFillsSlot:
		// A question's premise is whether the slot it fills is still EMPTY. The value the ask carries
		// is deliberately IGNORED — the user supplies it, so it cannot be part of the premise.
		current, err := read(ctx, state, act)
		if err != nil {
			return true, nil // fail open
		}
		return strings.TrimSpace(current) == "", nil

	case ChangeTerminal:
		current, err := read(ctx, state, act)
		if err != nil {
			return true, nil // fail open
		}
		// ⚠️ CASE-INSENSITIVE + TRIMMED, deliberately. A status arrives as either an upper-case
		// lifecycle value (COMPLETED, ARCHIVED) or a lower-case workflow-state type (completed), and a
		// case-sensitive compare here would silently never fire — re-minting an ask for an object that
		// is already terminal, forever. (Mirrors a hand-written resolver's case-insensitive statusIs.)
		return !containsStatus(spec.TerminalStates, current), nil

	default:
		return true, nil
	}
}

// Validate reports every declaration mistake that would make this set misbehave at run time. Call it
// once at wiring time: a declarative design earns its keep by catching a wrong declaration BEFORE a run.
func (as *ActionSet[S]) Validate() error {
	if as == nil {
		return nil // nothing declared, so nothing to be wrong
	}
	var errs []error
	seen := make(map[string]bool, len(as.actions))

	for _, a := range as.actions {
		if a.Opcode == "" {
			errs = append(errs, errors.New("agentkit: an action has an empty Opcode — it can never be emitted"))
			continue
		}
		if seen[a.Opcode] {
			errs = append(errs, fmt.Errorf("agentkit: duplicate action %q — lookup takes the FIRST "+
				"declaration, so the other is unreachable", a.Opcode))
		}
		seen[a.Opcode] = true

		// ⭐ THE SAME SAFETY CHECKS Bind AND ToolSet APPLY. Four independent reviews found the same
		// declaration mistake caught on one path and silent on another: an irreversible act with no
		// Ask, and a forgotten Tier, both passed here while Bind and ToolSet.Validate rejected them.
		// A guard is only worth what its WEAKEST reachable path enforces.
		// ⭐ A FORGOTTEN Category IS THE WORST DEFAULT IN THE KIT. Before ChangeUnset existed the zero
		// value was ChangeAlwaysMints, so an omitted field silently declared "no computable post-state
		// ⇒ always mint" — and the ask RE-MINTED ON EVERY REVIEW, which is precisely what ActionSet is
		// for. It passed Bind, Validate and both harness entry points.
		// ⭐ THE DESCRIPTION IS THE PROMPT. RenderActions shows it to the model verbatim, so an empty one
		// renders as a bare opcode with no guidance — the model then either never emits the act or
		// invents its own trigger conditions. It is the one field whose absence is invisible in Go and
		// load-bearing at runtime, and it was the only one Validate ignored.
		if strings.TrimSpace(a.Description) == "" {
			errs = append(errs, fmt.Errorf("agentkit: action %q has an EMPTY Description — that field IS "+
				"THE PROMPT (RenderActions shows it to the model), so an empty one offers a nameless act "+
				"and the model either ignores it or invents when to use it. Say what it does and WHEN to "+
				"use it", a.Opcode))
		}

		if a.Category == ChangeUnset {
			errs = append(errs, fmt.Errorf("agentkit: action %q declares no Category — the zero value is "+
				"INVALID because the old default (always-mint) made a forgotten field re-propose the same "+
				"ask forever. State the shape of the effect: ChangeScalarSet / ChangeMembership / "+
				"ChangeTerminal / ChangeMultiField / ChangeFillsSlot / ChangeAlwaysMints / ChangeMerge",
				a.Opcode))
		}

		if a.Tier == TierReadOnly {
			errs = append(errs, fmt.Errorf("agentkit: action %q declares no Tier — the zero value is "+
				"read-only, which AUTO-RUNS, so a forgotten field would make a destructive act live. "+
				"State it: TierReadOnlyExplicit / TierReversible / TierExternalReversible / "+
				"TierIrreversible", a.Opcode))
		} else if err := checkRunAsk(a.Opcode, a.Tier, false, a.Ask != nil); err != nil {
			errs = append(errs, err)
		}

		// ⭐ The reader must match the CATEGORY's arity. A multi-field action needs a FieldReader (it
		// receives the operand name); everything else needs a StateReader. Mispairing them is the
		// silent-drop bug: one reader shared across N operands compares every field to one value.
		if a.Category == ChangeMultiField {
			if as.fields[a.Opcode] == nil {
				if as.readers[a.Opcode] != nil {
					errs = append(errs, fmt.Errorf("agentkit: action %q is category multi_field but was "+
						"registered with Add — use AddMultiField. A single reader shared across its operands "+
						"compares every field to ONE value and silently reports 'no change' when a field "+
						"genuinely changed", a.Opcode))
				} else {
					errs = append(errs, fmt.Errorf("agentkit: action %q is category multi_field but no "+
						"FieldReader was supplied — WillChange could never decide it", a.Opcode))
				}
			}
		} else {
			if as.fields[a.Opcode] != nil {
				errs = append(errs, fmt.Errorf("agentkit: action %q is category %s but was registered with "+
					"AddMultiField — use Add; only multi_field compares per operand", a.Opcode, a.Category))
			}
			if a.Category.needsStateReader() && as.readers[a.Opcode] == nil {
				errs = append(errs, fmt.Errorf("agentkit: action %q is category %s, which compares against "+
					"current state, but no state reader was supplied — WillChange could never decide it",
					a.Opcode, a.Category))
			}
		}

		switch a.Category {
		case ChangeScalarSet, ChangeMembership, ChangeFillsSlot:
			if a.Input == "" {
				errs = append(errs, fmt.Errorf("agentkit: action %q is category %s but names no Input — "+
					"the kit cannot know WHICH operand carries the value being compared", a.Opcode, a.Category))
			} else if !hasOperand(a, a.Input) {
				errs = append(errs, fmt.Errorf("agentkit: action %q: Input %q is not one of its operands — "+
					"the comparison would read an absent arg and always mint", a.Opcode, a.Input))
			}
		case ChangeTerminal:
			if len(a.TerminalStates) == 0 {
				errs = append(errs, fmt.Errorf("agentkit: action %q is category terminal but declares no "+
					"TerminalStates — the kit cannot know what 'already there' means", a.Opcode))
			}
		}
	}
	return errors.Join(errs...)
}

// opcodes returns the declared opcodes, for error messages that name the valid alternatives.
func (as *ActionSet[S]) opcodes() []string {
	out := make([]string, 0, len(as.actions))
	for _, a := range as.actions {
		out = append(out, a.Opcode)
	}
	return out
}

// RenderActions is the prompt block listing what the agent may PROPOSE. Generated from the same
// declarations the state logic hangs off, so the model's list cannot drift from what the kit verifies.
func RenderActions(actions []Action) string {
	if len(actions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Acts you may PROPOSE (the user approves them; you never perform them yourself):\n")
	for _, a := range actions {
		fmt.Fprintf(&b, "- %s", a.Opcode)
		if a.Description != "" {
			fmt.Fprintf(&b, " — %s", a.Description)
		}
		b.WriteString("\n")
		for _, o := range a.Operands {
			req := ""
			if o.Required {
				req = ", required"
			}
			fmt.Fprintf(&b, "    %s (%s%s)", o.Name, o.Type, req)
			if o.Description != "" {
				fmt.Fprintf(&b, ": %s", o.Description)
			}
			if len(o.EnumValues) > 0 {
				fmt.Fprintf(&b, " [one of: %s]", strings.Join(o.EnumValues, ", "))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func operandOf(a Action, name string) (Operand, bool) {
	for _, o := range a.Operands {
		if o.Name == name {
			return o, true
		}
	}
	return Operand{}, false
}

func hasOperand(a Action, name string) bool {
	_, ok := operandOf(a, name)
	return ok
}

func containsString(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// valuesDiffer is the comparison the fixpoint law reduces to — TYPE-AWARE, not a raw string compare.
//
// ⭐ WHY THIS EXISTS. A raw `proposed != current` re-mints forever on any value with more than one
// spelling: "2026-08-21" vs "2026-08-21T00:00:00Z" is the SAME DATE, and 5000 vs "5000.00" the same
// NUMBER, but both compare unequal as strings. That is the duplicate-proposal class this gate exists to
// kill, re-introduced by the mechanism meant to prevent it.
//
// It is the KIT's job, not the consumer's: the kit declares the Type, hard-codes the comparison, and
// offers no normalization seam — so a consumer literally cannot fix it without abandoning the
// category. The gate this mirrors does it correctly in SQL (`IS DISTINCT FROM $1::timestamptz`).
//
// ⚠️ Falls back to an exact compare for any type the kit does not own, and for values that do not
// parse — never guess at a value whose shape was not declared.
func valuesDiffer(declaredType, proposed, current string) bool {
	switch normalizeTypeName(declaredType) {
	case "date":
		p, errP := parseDate(proposed)
		c, errC := parseDate(current)
		if errP == nil && errC == nil {
			return !p.Equal(c)
		}
	case "number":
		p, errP := strconv.ParseFloat(strings.TrimSpace(proposed), 64)
		c, errC := strconv.ParseFloat(strings.TrimSpace(current), 64)
		if errP == nil && errC == nil {
			return p != c
		}
	}
	// ⚠️ TRIM BOTH SIDES. WillChange trimmed only the PROPOSED value, so a stored string carrying
	// incidental whitespace ("done " from a paste, a trailing newline) never equalled the clean value
	// the model emits — the premise said "this changes something" forever and the ask RE-MINTED every
	// single review. Fixed HERE rather than at the two call sites so a third one cannot reintroduce it.
	//
	// ⭐ Trimming is safe for a PREMISE CHECK even though it is not a general string equality: the
	// question is "would applying this op change the state?", and leading/trailing whitespace is not a
	// change a user asked for. The op still writes the untrimmed proposed value.
	return strings.TrimSpace(proposed) != strings.TrimSpace(current)
}

// normalizeTypeName folds the SYNONYMS the kit's own two operand types document for one concept.
//
// ⚠️ THE BUG THIS FIXES WAS LIVE. CapabilityArg.Type documents "int"; Operand.Type documents
// "number"; and only "number" got numeric comparison. A developer writing the spelling documented on
// the sibling type re-proposed an act that changes nothing, forever, with every other check green:
//
//	Type="number" current=40.0 proposed=40 -> WillChange=false   ← correct
//	Type="int"    current=40.0 proposed=40 -> WillChange=true    ← the bug
//
// ⚠️ It does NOT close the set. Type is the CONSUMER's vocabulary — an unrecognized name still falls
// back to an exact compare. The guarantee is "every spelling the kit itself documents works", not
// "only these exist".
func normalizeTypeName(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "number", "int", "integer", "float", "float64", "decimal":
		return "number"
	case "date", "datetime", "timestamp", "time":
		return "date"
	default:
		return t
	}
}

// operandTypeOf returns the declared Type of a named operand ("" if undeclared).
func operandTypeOf(a Action, name string) string {
	if o, ok := operandOf(a, name); ok {
		return o.Type
	}
	return ""
}

// checkOperandType validates a supplied value against its declared Type — a total, cheap check.
//
// ⭐ WHY THE KIT VALIDATES THESE AT ALL, given the backend is the authority: the declared Type is a
// promise the kit makes to the reader, and an unvalidated `Type: "date"` is worse than an untyped
// field because it reads as meaningful. An adversarial review drove `set_budget(amount="not-a-number")`
// and `set_review_date(date="tomorrow-ish")` straight through to a rendered ask — a malformed
// proposal in front of a human, which is precisely what VerifyAction's fail-CLOSED contract exists
// to prevent.
//
// ⚠️ Deliberately NOT exhaustive: an unrecognized Type is ACCEPTED, because Type is the consumer's
// vocabulary and the kit must not close a set it does not own. Only the types the kit itself names
// are enforced. An empty value is allowed here (required-ness is a separate check) except for enums,
// where empty is never a member.
func checkOperandType(opcode string, o Operand, val string) error {
	// ⚠️ NORMALIZE FIRST. Reading o.Type raw meant the SYNONYMS this kit documents — "int" for
	// "number", "datetime" for "date" — fell to the accept-unknown default, so VerifyAction passed
	// amount="not-a-number". normalizeTypeName exists exactly to fold them, and this switch was the
	// one place that forgot to call it: the fail-CLOSED guarantee held for half the spellings.
	switch normalizeTypeName(o.Type) {
	case "enum":
		if val != "" && !containsString(o.EnumValues, val) {
			return fmt.Errorf("agentkit: %s: operand %q=%q is not one of [%s]",
				opcode, o.Name, val, strings.Join(o.EnumValues, ", "))
		}
	case "date":
		if strings.TrimSpace(val) == "" {
			return nil
		}
		if _, err := parseDate(val); err != nil {
			return fmt.Errorf("agentkit: %s: operand %q=%q is not a valid date "+
				"(want RFC3339 or YYYY-MM-DD, e.g. 2026-09-30)", opcode, o.Name, val)
		}
	case "number":
		if strings.TrimSpace(val) == "" {
			return nil
		}
		if _, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err != nil {
			return fmt.Errorf("agentkit: %s: operand %q=%q is not a valid number", opcode, o.Name, val)
		}
	}
	return nil
}

// parseDate accepts the two date shapes that travel through this system: full RFC3339 and a bare
// calendar date. Both are returned as a time, so a comparison can normalize rather than string-match.
func parseDate(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}

// containsStatus is containsString for STATUS values: case-insensitive and whitespace-trimmed.
//
// ⭐ Statuses are the one place an exact byte compare is wrong. The same object's status arrives as
// an upper-case lifecycle value ("ARCHIVED") or a lower-case workflow-state type ("archived")
// depending on which column it was read from, so a strict compare silently never fires and the ask
// is re-minted forever. Mirrors a hand-written resolver's case-insensitive statusIs.
func containsStatus(xs []string, v string) bool {
	v = strings.TrimSpace(v)
	for _, x := range xs {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}
