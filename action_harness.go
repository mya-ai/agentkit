package agentkit

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// THE ACTION HARNESS — because the declarations are written by a CODING AGENT.
//
// A declarative design only pays off if the writer can also TEST what it wrote. Testing a
// hand-written comparator means inventing cases and hoping you covered the empty value; testing a
// DECLARATION is mechanical, because the category says which cases apply:
//
//	state where the effect ALREADY holds  → WillChange == false
//	state where it does NOT               → WillChange == true
//	empty input (ScalarSet)               → true   (Add undetermined)
//	the reader errors                     → true   (fail open)
//
// ⭐ So the kit generates that table FROM the declaration: VerifyActionSet tests an ENTIRE ActionSet
// in one call, and an agent adding an action gets its tests essentially for free.
//
// ⚠️ These helpers do NOT re-implement the rule they test — they call the same generated WillChange.
// A harness with its own copy would pass while the real path is broken. The INDEPENDENT check is the
// parity test against the backend's own gate.
//
// This ships in-package, following actor_inmem.go. (There is no agentkit/agenttest package.)
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// TestingT is the subset of *testing.T the harness needs, so the kit does not import "testing" into
// production code paths and consumers can wrap it.
//
// ⚠️ RENAMED FROM `TB`. That name was borrowed from testing.TB and only reads to someone who already
// knows the stdlib idiom — while the symbol SHIPS in a production file (this one, not a _test.go).
// So nothing in the name told a reader that VerifyActionSet belongs in a test, and nothing stops it
// linking into a production binary. `TestingT` says the obligation out loud.
type TestingT interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// FakeState is a settable stand-in for a consumer's gathered state, so a "current value" needs no
// store, no DB and no network. Key it however your readers read it.
type FakeState map[string]string

// Reader returns a StateReader over one key of this fake — the usual way to wire a FakeState into an
// ActionSet built for tests.
func (f FakeState) Reader(key string) StateReader[FakeState] {
	return func(_ context.Context, s *FakeState, _ Action) (string, error) {
		if s == nil {
			return "", nil
		}
		return (*s)[key], nil
	}
}

// VerifyActionSet runs the whole formal contract over EVERY declared action: the declaration is
// well-formed, the generated rule obeys its category's derivation, the empty-value consequence holds,
// and a failing reader fails OPEN.
//
// ⭐ One call tests an entire ActionSet. Call it from your agent's package test:
//
//	func TestAgentActions(t *testing.T) { agentkit.VerifyActionSet(t, agent.Actions()) }
//
// It does NOT verify your BUSINESS semantics (is this the right action to propose?) — only that the
// declarations are internally coherent and the derived rules behave.
func VerifyActionSet[S any](t TestingT, set *ActionSet[S]) {
	t.Helper()

	if set == nil {
		t.Fatalf("agentkit: VerifyActionSet got a nil set")
		return
	}
	if err := set.Validate(); err != nil {
		t.Errorf("agentkit: the ActionSet does not validate — fix the declarations first:\n%v", err)
		return
	}

	verifyDeclarations(t, set)
}

// VerifyActionDeclarations is the READER-FREE half of the contract: every check that can be made
// from the declarations alone — operand coherence, the Input naming a real operand, terminal-state
// case-folding, and that each action can be emitted validly when fully populated.
//
// ⭐ Use this when your BACKEND owns the premise gate. An agent whose service already enforces
// "would this change state?" server-side (and unbypassably) has no reason to write agent-side state
// readers — they would be dead production code. VerifyActionSet requires them, because Validate
// requires them; this entry point does not, so the declaration checks stay reachable.
//
// A real consumer migrating onto this library wrote ~60 lines of test-only readers purely to unlock
// the harness. That is the friction this exists to remove.
func VerifyActionDeclarations[S any](t TestingT, set *ActionSet[S]) {
	t.Helper()

	if set == nil {
		t.Fatalf("agentkit: VerifyActionDeclarations got a nil set")
		return
	}
	for _, a := range set.Actions() {
		if a.Opcode == "" {
			t.Errorf("agentkit: an action has an empty Opcode — it can never be emitted")
			continue
		}
		switch a.Category {
		case ChangeScalarSet, ChangeMembership:
			if a.Input == "" {
				t.Errorf("agentkit: %s is category %s but names no Input — the kit cannot know WHICH "+
					"operand carries the value being compared", a.Opcode, a.Category)
			} else if !hasOperand(a, a.Input) {
				t.Errorf("agentkit: %s: Input %q is not one of its operands — the comparison would read "+
					"an absent arg and mint forever", a.Opcode, a.Input)
			}
		case ChangeTerminal:
			if len(a.TerminalStates) == 0 {
				t.Errorf("agentkit: %s is category terminal but declares no TerminalStates", a.Opcode)
			}
		}
	}
	verifyDeclarations(t, set)
}

// VocabularyOracle answers "what values does the BACKEND actually accept for this operand?" — nil or
// empty means "no opinion, skip it".
//
// The kit cannot know this (it holds no op registry), so the consumer supplies it — typically by
// reading the real registry in a test, where crossing the service boundary is allowed even though
// production code may not.
type VocabularyOracle func(opcode, operand string) []string

// VerifyActionSetVocabulary checks every declared enum against what the backend really accepts.
//
// ⭐ WHY THIS IS NOT OPTIONAL. VerifyAction fails CLOSED on enums, so a drifted vocabulary makes your
// agent REJECT values the backend would have accepted — silently discarding legitimate model output.
// It is the one declaration error that degrades behaviour while every other check stays green.
//
// Found the hard way: a consumer's ActionSet carried 5 invented and 4 missing enum values, and BOTH
// VerifyActionSet and VerifyActionSetAgainst passed it. They checked operand NAMES and never the
// VOCABULARY.
//
//	agentkit.VerifyActionSetVocabulary(t, set, func(opcode, operand string) []string {
//	    h, ok := registry.Lookup(opcode)     // the real registry, read in a test
//	    if !ok { return nil }
//	    return h.Spec.EnumValuesFor(operand)
//	})
func VerifyActionSetVocabulary[S any](t TestingT, set *ActionSet[S], truth VocabularyOracle) {
	t.Helper()

	if set == nil || truth == nil {
		return
	}
	for _, a := range set.Actions() {
		for _, o := range a.Operands {
			want := truth(a.Opcode, o.Name)
			if len(want) == 0 {
				continue // the oracle has no opinion about this operand
			}
			if !sameVocabulary(o.EnumValues, want) {
				t.Errorf("agentkit: %s operand %q: ENUM VOCABULARY DRIFT\n  declared: %v\n  backend:  %v\n"+
					"VerifyAction fails CLOSED on enums, so a declared value the backend does not have "+
					"(or a missing one) makes this agent reject output the backend would accept.",
					a.Opcode, o.Name, o.EnumValues, want)
			}
		}
	}
}

// sameVocabulary compares two enum sets ignoring order (a registry and a declaration need not agree
// on ordering, only on membership).
func sameVocabulary(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(want))
	for _, w := range want {
		seen[w]++
	}
	for _, g := range got {
		if seen[g] == 0 {
			return false
		}
		seen[g]--
	}
	return true
}

// verifyDeclarations is the shared body of the checks that need no consumer state.
func verifyDeclarations[S any](t TestingT, set *ActionSet[S]) {
	t.Helper()

	for _, a := range set.Actions() {
		// Every declared action must survive verification of its OWN fully-populated form. If it
		// cannot, the model can never emit a valid instance of it.
		if err := set.VerifyAction(populate(a)); err != nil {
			t.Errorf("agentkit: action %q cannot be emitted validly even when fully populated — its "+
				"declaration is self-contradictory: %v", a.Opcode, err)
		}

		// The empty-value consequence, for the categories it applies to.
		if a.Category == ChangeScalarSet || a.Category == ChangeMembership {
			blank := a
			blank.Args = map[string]string{a.Input: ""}
			changes, err := set.WillChange(context.Background(), zeroOf[S](), blank)
			if err != nil {
				t.Errorf("agentkit: %s: WillChange errored on an empty input: %v", a.Opcode, err)
			} else if !changes {
				t.Errorf("agentkit: %s: an EMPTY %q was treated as a no-op. The user fills it on accept, "+
					"so there is no post-state to compare and the ask must always be shown",
					a.Opcode, a.Input)
			}
		}

		// The always-mint categories must never consult state.
		if !a.Category.needsStateReader() {
			changes, err := set.WillChange(context.Background(), zeroOf[S](), populate(a))
			if err != nil {
				t.Errorf("agentkit: %s: WillChange errored: %v", a.Opcode, err)
			} else if !changes {
				t.Errorf("agentkit: %s: category %s must ALWAYS mint (its post-state is not computable)",
					a.Opcode, a.Category)
			}
			continue
		}

		// ⭐ EXERCISE THE READER. An adversarial review found this harness reported NOTHING on a set
		// full of defects, because both paths it ran returned before ever calling the reader — so the
		// two most valuable rows of its own doc table ("effect already holds → false" and "reader
		// errors → true") were advertised and never executed. A harness that cannot fail is worse than
		// none: it reads as coverage.
		verifyReaderIsExercised(t, set, a)
	}
}

// verifyReaderIsExercised checks the declaration-level properties the kit CAN decide without knowing
// S — and it is deliberately narrow, because the interesting assertion (does a fixpoint state report
// "no change"?) needs a real state only the consumer can build. That is what VerifyActionSetAgainst
// is for; this is the floor.
func verifyReaderIsExercised[S any](t TestingT, _ *ActionSet[S], a Action) {
	t.Helper()

	if a.Category == ChangeTerminal {
		// A terminal state must survive the case/whitespace normalization the comparison applies —
		// the same status arrives upper-case from a lifecycle column and lower-case from a
		// workflow-state type, and a status that only matches one spelling re-mints forever.
		// ⚠️ THIS CHECK USED TO BE VACUOUS: it asked containsStatus(a.TerminalStates, ToUpper(s)) for
		// each s already IN that list, and containsStatus compares with EqualFold — always true. It
		// asserted nothing while reading like a real guard.
		//
		// What it MEANT to catch is a declared state the comparison can never match. Since the compare
		// trims and folds case, the reachable defects are an EMPTY state and one carrying surrounding
		// whitespace (a copy-paste of `"completed "`): the ask then re-mints forever, because no status
		// read back can equal it.
		for _, st := range a.TerminalStates {
			if strings.TrimSpace(st) == "" {
				t.Errorf("agentkit: %s: declares an EMPTY terminal state — it can never match a status, "+
					"so the ask re-mints on every review", a.Opcode)
				continue
			}
			if st != strings.TrimSpace(st) {
				t.Errorf("agentkit: %s: terminal state %q carries surrounding whitespace — declare it "+
					"clean; the value is compared against a status column that never has any", a.Opcode, st)
			}
		}
	}
}

// VerifyActionSetAgainst is VerifyActionSet plus the assertion that needs a real state: for each
// action you supply a state where its effect ALREADY HOLDS, and the harness asserts WillChange
// reports NO change — then that a failing reader still fails OPEN.
//
// ⭐ This is the row VerifyActionSet cannot run on its own. The kit cannot synthesize a value of your
// state type S, so "the effect already holds" is a state only you can build. Supplying it is what
// turns the harness from a declaration-hygiene check into a correctness one — and it is cheap,
// because it is the same state your gather already produces.
//
//	agentkit.VerifyActionSetAgainst(t, set, map[string]*State{
//	    "set_task_due_date": {Task: Task{DueDate: "2026-08-21"}},  // matches the action's own value
//	})
//
// Actions absent from fixpoints are checked exactly as VerifyActionSet checks them.
func VerifyActionSetAgainst[S any](t TestingT, set *ActionSet[S], fixpoints map[string]*S) {
	t.Helper()
	VerifyActionSet(t, set)
	if set == nil {
		return
	}

	for _, a := range set.Actions() {
		state, ok := fixpoints[a.Opcode]
		if !ok {
			continue
		}
		// ⚠️ SUPPLYING A FIXPOINT IS AN ASSERTION, so honour it even for a category that needs no
		// reader. This used to `continue` on !needsStateReader(), which silently DISCARDED the
		// developer's own contrary intent: writing a fixpoint state means "this must NOT mint here",
		// and for an always-mint category that belief may be WRONG — which is what the test should say.
		//
		// ⛔ Do NOT call WillChange here and branch on the result. It returns true UNCONDITIONALLY for
		// these categories, so the check is unfalsifiable — it can only ever report the error, which
		// makes it an assertion about the category itself, not about the state supplied. Say that
		// directly, and distinguish the two cases, because they call for different fixes:
		switch a.Category {
		case ChangeMerge:
			// A v1 APPROXIMATION, not a mistake. Add is not modelled for a merge write, so the kit
			// conservatively always mints; a real fixpoint may well exist and simply cannot be proven
			// yet. Tightening ChangeMerge later is explicitly anticipated, so a fixpoint here is a
			// reasonable belief the kit cannot honour — say so without calling it an error.
			t.Errorf("agentkit: %s: a fixpoint was supplied, but Category merge always mints in v1 "+
				"(its Add effect is not modelled yet), so the no-op cannot be derived. Drop the "+
				"fixpoint, or model the merge so it becomes checkable.", a.Opcode)
		case ChangeAlwaysMints:
			// A genuine contradiction: this category asserts there IS no computable post-state, so
			// claiming a fixpoint means the category is wrong (a create is usually really a scalar-set
			// or membership write) or the state is not actually a fixpoint.
			t.Errorf("agentkit: %s: a fixpoint was supplied, but Category always_mints declares the "+
				"act has NO computable post-state — the two cannot both be true. Either the category "+
				"is wrong, or this state is not a fixpoint.", a.Opcode)
		}
		if !a.Category.needsStateReader() {
			continue
		}

		changes, err := set.WillChange(context.Background(), state, populate(a))
		if err != nil {
			t.Errorf("agentkit: %s: WillChange errored on its fixpoint state: %v", a.Opcode, err)
			continue
		}
		if changes {
			t.Errorf("agentkit: %s: the state you supplied should make this action a NO-OP, but "+
				"WillChange reported a change. Either the state does not actually match the action's "+
				"effect, or the comparison is broken — a raw string compare on a typed field, a shared "+
				"reader across multi-field operands, or a missing terminal state all look like this.",
				a.Opcode)
		}
	}
}

// SimulateTurns answers the question that today can only be answered by running against real data:
// ⭐ "given THIS state, which of these proposals would my agent actually make?"
//
// It runs each action through both checks — VerifyAction (fails closed) then WillChange (fails open) —
// and returns only what SURVIVES, plus a per-action reason for everything dropped. The
// duplicate-proposal class becomes a unit test instead of a production observation.
func SimulateTurns[S any](set *ActionSet[S], state *S, actions []Action) (surviving []Action, dropped map[string]string) {
	dropped = make(map[string]string)
	if set == nil {
		return nil, dropped
	}

	for i, a := range actions {
		// ⚠️ KEY BY (index, opcode), NOT opcode alone. Keying by opcode silently OVERWROTE the reason
		// when a batch held several actions of the same opcode — which is the common multi-task case
		// (three set_task_due_date asks, two survive, one is a no-op), and precisely the case
		// StateReader takes an Action argument to support. The simulation then under-reported drops,
		// so a developer modelling "would my agent spam the user?" got a clean answer for a batch that
		// was not clean.
		key := fmt.Sprintf("%d:%s", i, a.Opcode)
		if err := set.VerifyAction(a); err != nil {
			dropped[key] = "malformed: " + err.Error()
			continue
		}
		changes, err := set.WillChange(context.Background(), state, a)
		if err != nil {
			dropped[key] = "undeclared: " + err.Error()
			continue
		}
		if !changes {
			dropped[key] = fmt.Sprintf("no-op: applying %s would not change state", a.Opcode)
			continue
		}
		surviving = append(surviving, a)
	}
	return surviving, dropped
}

// indexPart / opcodePart split a SimulateTurns drop key ("<index>:<opcode>").
func indexPart(key string) string {
	if i, _, ok := strings.Cut(key, ":"); ok {
		return i
	}
	return ""
}

func opcodePart(key string) string {
	if _, op, ok := strings.Cut(key, ":"); ok {
		return op
	}
	return key
}

// DropReason finds why an action was dropped, by opcode.
//
// ⭐ USE THIS instead of indexing the map. SimulateTurns keys drops by "<index>:<opcode>" so several
// actions of the SAME opcode in one batch each keep their own reason (keying by opcode alone silently
// overwrote all but the last — the multi-task case). This helper hides that key format, so a test
// asking "why was set_task_due_date dropped?" still reads naturally.
//
// With several drops for one opcode it returns the FIRST, in batch order. Use the map directly when
// you need them all.
func DropReason(dropped map[string]string, opcode string) string {
	best, found := "", -1
	for k, v := range dropped {
		idx, op, ok := strings.Cut(k, ":")
		if !ok || op != opcode {
			continue
		}
		n, err := strconv.Atoi(idx)
		if err != nil {
			continue
		}
		if found == -1 || n < found {
			best, found = v, n
		}
	}
	return best
}

// Explain renders what an ActionSet would do with a batch, for a test failure message or a debug run.
// Deterministic ordering, so it is safe to assert against.
func Explain(surviving []Action, dropped map[string]string) string {
	var b strings.Builder
	for _, a := range surviving {
		fmt.Fprintf(&b, "ASK    %s\n", a.Opcode)
	}
	// ⚠️ Keys are "<index>:<opcode>" (so several actions of one opcode each keep their reason), but the
	// INDEX is bookkeeping — a reader wants the opcode. Sort NUMERICALLY by index so drops read in the
	// order the model emitted them, then print the opcode alone.
	keys := make([]string, 0, len(dropped))
	for k := range dropped {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ai, _ := strconv.Atoi(indexPart(keys[i]))
		aj, _ := strconv.Atoi(indexPart(keys[j]))
		if ai != aj {
			return ai < aj
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "DROP   %s — %s\n", opcodePart(k), dropped[k])
	}
	return b.String()
}

// populate fills an action's required operands with placeholder values, so a DECLARATION can be
// exercised as if the model had emitted it.
func populate(a Action) Action {
	out := a
	out.Args = make(map[string]string, len(a.Operands))
	for _, o := range a.Operands {
		switch {
		case len(o.EnumValues) > 0:
			out.Args[o.Name] = o.EnumValues[0]
		case o.Type == "date":
			out.Args[o.Name] = "2026-01-02T00:00:00Z"
		case o.Type == "number":
			out.Args[o.Name] = "1"
		default:
			out.Args[o.Name] = "placeholder"
		}
	}
	return out
}

// zeroOf returns a pointer to the zero value of S, for the checks that must not consult state.
func zeroOf[S any]() *S {
	var s S
	return &s
}
