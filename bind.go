package agentkit

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// BIND — the contract is the BACKEND's; stop copying it.
//
// THE PROBLEM, measured on a real migration. A consumer declared its ~20 proposable ops in 335 lines
// where every operand, type, enum value, required flag, description and tier was hand-copied from
// data its backend ALREADY SERVES over gRPC. The coding agent writing it fabricated 5 enum values
// and dropped 4 — and the kit's own harness passed the result. Two production bugs trace to exactly
// this class of copy (a fabricated enum vocabulary, a wrong object-type).
//
// ⭐ A MIRROR OF WIRE-AVAILABLE DATA IS A CACHE WITH NO INVALIDATION. The fix is not "add more fields
// to the declaration" — it is to stop writing the fields the backend owns:
//
//	set, err := agentkit.Bind(ctx, backendOps, []agentkit.Use[State]{{
//	    Opcode:  "set_task_due_date",             // WHICH op — the agent's only identity claim
//	    Category: agentkit.ChangeScalarSet,       // the premise — the agent's judgment
//	    Input:   "due_date",
//	    Current: func(ctx context.Context, s *State, a agentkit.Action) (string, error) {
//	        return s.Task(a.Arg("task_id")).DueDate, nil
//	    },
//	}})
//
// Operands, types, enums, required flags, tier and description all arrive from the source. What the
// agent declares is what only the agent knows: WHICH ops it uses, and how each one's premise works.
//
// WHAT THIS BUYS BEYOND FEWER LINES:
//   - an opcode the backend does not have is rejected AT BIND, not in production (where the whole
//     program is discarded at verify)
//   - an Input naming no real operand is rejected at bind, instead of re-minting the same ask forever
//   - the tier is AUTHORITATIVE, so an agent cannot understate one and have the gate auto-run an act
//     the backend considers irreversible
//
// ⚠️ NOT EVERY ACT IS A BACKEND OP. Verified against the two production agents: one declares 11
// capabilities against a 24-op backend registry with ZERO overlap — its acts are service RPCs and
// in-memory work, not registry ops. So the contract source is per-ACT, not per-agent: use BindMixed
// and declare those locally.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// ContractSource serves the backend's op contract — what it will actually accept.
//
// Implement it over whatever your backend already exposes (e.g. a list-ops RPC, which
// serves opcode, description, object type, tier and args with types/required/enums). The kit never
// learns what a "project" is; it only consumes this shape.
type ContractSource interface {
	BackendOps(ctx context.Context) ([]Action, error)
}

// Use is one op an agent USES: which op, plus the facts only the agent knows.
//
// ⭐ Everything absent here is deliberate — operands, types, enums, required flags, tier and
// description belong to the backend and are fetched. If you find yourself wanting to add one, the
// question is whether your ContractSource is serving it.
type Use[S any] struct {
	// Opcode names the backend op. The agent's only identity claim.
	Opcode string

	// Category, Input, TerminalStates declare the PREMISE — how the kit derives WillChange. This is
	// the agent's judgment about the op's effect, not a fact the backend can supply.
	Category       ChangeCategory
	Input          string
	TerminalStates []string

	// Run PERFORMS the act. Nil is a POSITIVE statement — "this act is propose-only" — not an
	// omission, so a withheld act needs no tripwire handler that must never be called.
	// ⚠️ Rejected at bind for an act the backend's tier can never run live (dead code that reads live).
	Run RunAct[S]
	// Ask is the wording shown to the human. Nil means "never surfaced".
	// ⭐ Rejected at bind when the tier ALWAYS withholds this act: the gate would withhold it and the
	// user would never be asked, so the act vanishes silently. That is a real production bug this
	// check turns into a startup error — and it is only checkable because Run and Ask are TYPED
	// fields on one declaration. The kit cannot enforce a rule about a field it cannot see.
	Ask AskFor

	// Current reads the state the premise compares against. Nil for AlwaysMints/Merge.
	Current StateReader[S]
	// CurrentField is the per-operand reader for ChangeMultiField.
	CurrentField FieldReader[S]

	// AssertTier, when set, is CHECKED against the backend's tier rather than trusted. Use it only to
	// make a safety expectation explicit in code ("I believe this is irreversible"); a mismatch is a
	// bind error. Leave it zero to simply accept the backend's tier.
	AssertTier Tier

	// Structured names operands whose value the backend parses as JSON, so they can never be offered
	// as a free-text slot. ⚠️ Agent-declared because the contract cannot express it — a wire `type`
	// is string|date|number|enum, and JSON-ness survives only in human-readable description prose.
	Structured []string

	// Extra carries agent-private facts the kit never interprets (priority, anchor arg, invalidation
	// triggers). Retrieve it with ExtraOf.
	Extra any
}

// Bind fetches the contract and binds the declared uses against it, returning a ready ActionSet.
//
// It FAILS CLOSED: an unreachable contract with no cache is an error, never a degraded start. An
// agent that starts without its contract emits args the backend rejects and loses whole programs at
// verify — worse than not starting.
func Bind[S any](ctx context.Context, src ContractSource, uses []Use[S]) (*ActionSet[S], error) {
	return BindMixed(ctx, src, uses, nil)
}

// BindMixed is Bind plus LOCAL acts — ones the agent owns outright, whose contract the backend has
// never heard of (a service RPC, in-memory work, a draft the agent composes).
//
// ⭐ This exists because a per-AGENT contract source would reject an entire real agent: one of the
// two production consumers has ZERO overlap between its 11 capabilities and the backend's 24 ops.
// The contract source is per-ACT.
//
// A local act carries its own full declaration (operands, types, tier) exactly as before — the
// agent is the authority for an act only it knows about.
func BindMixed[S any](ctx context.Context, src ContractSource, uses []Use[S], local []Local[S]) (*ActionSet[S], error) {
	set := NewActionSet[S]()

	var backend map[string]Action
	if len(uses) > 0 {
		if src == nil {
			return nil, errors.New("agentkit: Bind needs a ContractSource to bind backend ops " +
				"(pass only local acts to BindMixed if the agent has no backend ops)")
		}
		ops, err := src.BackendOps(ctx)
		if err != nil {
			// FAIL CLOSED — see the doc above.
			return nil, fmt.Errorf("agentkit: cannot bind without the backend contract (an agent that "+
				"guesses its operands loses whole programs at verify): %w", err)
		}
		backend = make(map[string]Action, len(ops))
		for _, o := range ops {
			backend[o.Opcode] = o
		}
	}

	var errs []error
	for _, u := range uses {
		spec, ok := backend[u.Opcode]
		if !ok {
			errs = append(errs, fmt.Errorf("agentkit: %q: the backend does not declare this opcode "+
				"(the agent would emit it and lose the whole program at verify); backend has: %s",
				u.Opcode, strings.Join(sortedOpcodes(backend), ", ")))
			continue
		}
		if u.AssertTier != 0 && normalizeTier(u.AssertTier) != normalizeTier(spec.Tier) {
			errs = append(errs, fmt.Errorf("agentkit: %q: asserted tier %s but the backend says %s — "+
				"understating a tier lets the gate auto-run an act the backend says can never run live",
				u.Opcode, normalizeTier(u.AssertTier), spec.Tier))
			continue
		}

		// The backend's contract, plus the agent's premise. Nothing is copied.
		act := spec
		act.Category = u.Category
		act.Input = u.Input
		act.TerminalStates = u.TerminalStates
		act.Structured = u.Structured
		act.Extra = u.Extra

		if err := checkRunAsk(act.Opcode, spec.Tier, u.Run != nil, u.Ask != nil); err != nil {
			errs = append(errs, err)
			continue
		}
		act.Ask = u.Ask

		if u.CurrentField != nil {
			set.AddMultiField(act, u.CurrentField)
		} else {
			set.Add(act, u.Current)
		}
	}

	for _, l := range local {
		if err := checkRunAsk(l.Opcode, l.Tier, l.Run != nil, l.Ask != nil); err != nil {
			errs = append(errs, err)
			continue
		}
		// ⭐ CARRY Ask ONTO THE ACTION. Local EMBEDS Action and also declares its own Ask/Run, so
		// `l.Ask` is Local's field and `l.Action.Ask` stays nil — the outer field SHADOWS the embedded
		// one. Storing l.Action bare therefore dropped the wording, and ActionSet.Validate (which reads
		// a.Ask) then REJECTED a correctly-declared propose-only act:
		//
		//	"send_it" is above the auto-ceiling (tier irreversible) but has no Ask
		//
		// i.e. the kit refused the exact declaration it documents. Copy it across at the seam.
		act := l.Action
		if act.Ask == nil && l.Ask != nil {
			act.Ask = l.Ask
		}
		if l.CurrentField != nil {
			set.AddMultiField(act, l.CurrentField)
			continue
		}
		set.Add(act, l.Current)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	// The same wiring-time checks a hand-built set gets — Input naming a real operand, a category
	// with no reader, a duplicate opcode.
	if err := set.Validate(); err != nil {
		return nil, err
	}
	return set, nil
}

// RunAct performs an act the tier allows to run live. Same contract as ToolSet's RunFunc: nil means
// performed OR deliberately skipped; an error is collected and the run continues.
type RunAct[S any] func(ctx context.Context, state *S, act Action) error

// AskFor renders the wording shown to the human for a withheld act.
type AskFor func(act Action) string

// checkRunAsk is the structural guarantee that a withheld act cannot vanish, and that a handler
// cannot exist for an act that can never run.
//
// ⭐ THE FIRST HALF IS A REAL PRODUCTION BUG AS A STARTUP ERROR. An agent declared two propose-tier
// capabilities; the gate correctly withheld them into Plan.Propose, and nothing consumed that half.
// No error, no log — the ask never reached the user. Only a declaration carrying BOTH halves can see
// it, which is why Run and Ask are typed fields rather than an opaque bag.
func checkRunAsk(opcode string, tier Tier, hasRun, hasAsk bool) error {
	withheld := MaxRungFor(tier) == RungAsk
	if withheld && !hasAsk {
		return fmt.Errorf("agentkit: %q is above the auto-ceiling (tier %s) but has no Ask — the gate "+
			"withholds it and the user is NEVER asked, so the act vanishes silently", opcode, tier)
	}
	if withheld && hasRun {
		return fmt.Errorf("agentkit: %q has a Run handler but its tier (%s) can never run live — the "+
			"gate can never dispatch it, so the handler is dead code that reads as live", opcode, tier)
	}
	return nil
}

// Local is an act the AGENT owns outright — the backend has never heard of it, so the agent supplies
// the full contract itself (a service RPC, in-memory work, a draft the agent composes).
//
// ⭐ This is not a lesser form of Use. Verified against the two production agents: one has ZERO
// overlap between its 11 capabilities and the backend's 24 registry ops. For that agent, LOCAL is the
// normal case and fetched ops are the exception.
type Local[S any] struct {
	Action
	Run          RunAct[S]
	Ask          AskFor
	Current      StateReader[S]
	CurrentField FieldReader[S]
}

// ExtraOf returns an action's agent-private Extra, typed.
//
// It is a generic FUNCTION rather than a method so ActionSet needs no second type parameter — the
// kit stays ignorant of what the value is.
func ExtraOf[X any, S any](set *ActionSet[S], opcode string) (X, bool) {
	var zero X
	if set == nil {
		return zero, false
	}
	a, ok := set.Lookup(opcode)
	if !ok {
		return zero, false
	}
	x, ok := a.Extra.(X)
	return x, ok
}

// CachedContract wraps a ContractSource with a TTL, so a backend redeploy does not take the agent
// down and a stale contract cannot outlive the deploy that changed it.
//
// ⚠️ THE TTL IS THE POINT. Last-known-good FOREVER fails in the other direction: an enum value added
// by a deploy would be rejected by the agent's stale copy — refusing output the backend now accepts.
// Bounded staleness is the only safe form of this cache.
type CachedContract struct {
	src ContractSource
	ttl time.Duration

	mu      sync.Mutex
	ops     []Action
	fetched time.Time
}

// NewCachedContract wraps src, refreshing at most once per ttl.
func NewCachedContract(src ContractSource, ttl time.Duration) *CachedContract {
	return &CachedContract{src: src, ttl: ttl}
}

// BackendOps serves the cache within the TTL, refreshes after it, and falls back to the last good
// contract when the upstream is unavailable. With no cache at all it propagates the error, so a COLD
// start still fails closed.
// ⚠️ EVERY return path hands out a COPY — see cloneActions. Returning the backing slice let a caller
// adapting the contract (a per-tenant filter, an operand normalization) corrupt it for every
// subsequent agent in the process, silently and coherently.
func (c *CachedContract) BackendOps(ctx context.Context) ([]Action, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ops != nil && time.Since(c.fetched) < c.ttl {
		return cloneActions(c.ops), nil
	}

	ops, err := c.src.BackendOps(ctx)
	if err != nil {
		if c.ops != nil {
			// WARM: survive the outage on the last good contract. Bounded by the TTL on the next call.
			return cloneActions(c.ops), nil
		}
		return nil, err // COLD: fail closed.
	}
	// Store a copy too: the SOURCE may hand back its own backing slice (a hand-rolled ContractSource
	// over a static op table is the obvious case), which would alias the cache to the source's state.
	c.ops, c.fetched = cloneActions(ops), time.Now()
	return cloneActions(c.ops), nil
}

// cloneActions deep-copies every slice and map an Action owns, so a returned contract shares no
// mutable state with the cache.
//
// ⚠️ A shallow `slices.Clone` is NOT enough and looks like it is: Action owns SIX reference fields
// (Operands, TerminalStates, Structured, Args, UserFills, and each Operand's EnumValues), so a
// shallow copy still points at the cached backing arrays. My first attempt at this function cloned
// only Operands+EnumValues — the two the test happened to exercise — which is the trap this comment
// exists to flag: cover the TYPE's reference fields, not the ones a test reminded you of.
//
// ⭐ MAINTENANCE: adding a slice/map field to Action or Operand means adding it HERE. Nothing
// enforces that automatically — TestCachedContract_HandsOutACopy only catches the fields it probes.
func cloneActions(in []Action) []Action {
	if in == nil {
		return nil
	}
	out := make([]Action, len(in))
	for i, a := range in {
		if a.Operands != nil {
			ops := make([]Operand, len(a.Operands))
			for j, o := range a.Operands {
				o.EnumValues = slices.Clone(o.EnumValues)
				ops[j] = o
			}
			a.Operands = ops
		}
		a.TerminalStates = slices.Clone(a.TerminalStates)
		a.Structured = slices.Clone(a.Structured)
		a.UserFills = slices.Clone(a.UserFills)
		if a.Args != nil {
			a.Args = maps.Clone(a.Args)
		}
		out[i] = a
	}
	return out
}

// expireForTest forces the next BackendOps to refresh. Test-only.
func (c *CachedContract) expireForTest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetched = time.Time{}
}

// ⚠️ IT MUST ACTUALLY SORT — the name promised it and the body did not. Map iteration is randomized
// in Go, so the bind-failure message ("the backend declares: a, b, c") listed the available opcodes in
// a different order every run, which makes the one error a developer reads while wiring an agent
// unstable and undiffable.
func sortedOpcodes(m map[string]Action) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
