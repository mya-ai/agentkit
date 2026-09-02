package agentkit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// BIND — the tests define what "stop copying the backend's contract" means.
//
// A consumer migration produced a 335-line declaration in which EVERY operand, type, enum, required
// flag and tier was hand-copied from data the backend already serves over gRPC. The coding agent
// writing it fabricated 5 enum values and dropped 4 — and the kit's own harness passed it.
//
// A mirror of wire-available data is a cache with no invalidation. Bind removes the copy.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// fakeContract is a ContractSource under test control: it can serve, fail, and count its calls.
type fakeContract struct {
	ops   []Action
	err   error
	calls int
}

func (f *fakeContract) BackendOps(context.Context) ([]Action, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.ops, nil
}

// backendDueDate is what a real registry serves for one op — the fields an agent should NEVER retype.
func backendDueDate() Action {
	return Action{
		Opcode: "set_task_due_date", Tier: TierReversible,
		Description: "Set a task's due date.",
		Operands: []Operand{
			{Name: "task_id", Type: "string", Required: true},
			{Name: "due_date", Type: "date", Required: true, Description: "RFC3339"},
		},
	}
}

func backendArchive() Action {
	return Action{
		Opcode: "archive_project", Tier: TierIrreversible,
		Description: "Archive a project.",
		Operands:    []Operand{{Name: "project_id", Type: "string", Required: true}},
	}
}

// TestBind_FetchesTheContract is the core: the agent names WHICH ops it uses and supplies only its
// own facts. Operands, types, required flags, enums and tier arrive from the backend.
func TestBind_FetchesTheContract(t *testing.T) {
	src := &fakeContract{ops: []Action{backendDueDate(), backendArchive()}}

	set, err := Bind[testState](context.Background(), src, []Use[testState]{{
		Opcode: "set_task_due_date", Category: ChangeScalarSet, Input: "due_date",
		Current: dueDateReader,
	}})
	if err != nil {
		t.Fatalf("Bind failed on a well-formed use: %v", err)
	}

	a, ok := set.Lookup("set_task_due_date")
	if !ok {
		t.Fatal("the bound action is missing")
	}
	// ⭐ None of these were written by the agent.
	if a.Tier != TierReversible {
		t.Errorf("Tier must come from the BACKEND, got %v", a.Tier)
	}
	if len(a.Operands) != 2 {
		t.Errorf("operands must come from the backend, got %d", len(a.Operands))
	}
	if a.Description == "" {
		t.Error("the agent-facing Description must come from the backend")
	}
	// And the agent's own facts survived.
	if a.Category != ChangeScalarSet || a.Input != "due_date" {
		t.Error("the agent's premise declaration was lost")
	}
	// Only the ops the agent USES are bound — not everything the backend offers.
	if len(set.Actions()) != 1 {
		t.Errorf("Bind must bind only the declared uses, got %d", len(set.Actions()))
	}
}

// TestBind_RejectsAnUndeclaredOpcode is the check that moves a production failure to startup. An
// agent emitting an opcode the backend does not have loses the whole program at verify time; here it
// cannot even start.
func TestBind_RejectsAnUndeclaredOpcode(t *testing.T) {
	src := &fakeContract{ops: []Action{backendDueDate()}}

	_, err := Bind[testState](context.Background(), src, []Use[testState]{{
		Opcode: "adopt_todos", Category: ChangeAlwaysMints, // the backend has no such op
	}})
	if err == nil {
		t.Fatal("an opcode the backend does not declare must be rejected AT BIND — otherwise it fails " +
			"in production, where the whole program is discarded at verify")
	}
	if !strings.Contains(err.Error(), "adopt_todos") {
		t.Errorf("the error must name the opcode, got: %v", err)
	}
}

// TestBind_RejectsAnInputThatIsNotAnOperand catches the typo that would otherwise mint forever: the
// comparison reads an absent arg, sees "", and the empty-value rule always mints.
func TestBind_RejectsAnInputThatIsNotAnOperand(t *testing.T) {
	src := &fakeContract{ops: []Action{backendDueDate()}}

	_, err := Bind[testState](context.Background(), src, []Use[testState]{{
		Opcode: "set_task_due_date", Category: ChangeScalarSet, Input: "dedline", // typo
		Current: dueDateReader,
	}})
	if err == nil {
		t.Fatal("an Input naming no real operand must be rejected at BIND — it would read an absent " +
			"arg and re-mint the same ask on every run")
	}
	if !strings.Contains(err.Error(), "dedline") {
		t.Errorf("the error must name the bad Input, got: %v", err)
	}
}

// TestBind_FailsClosedWhenTheContractIsUnavailable pins the cold-start rule both designers argued
// for: a proposing agent with NO contract can only guess, and guessing means emitting args the
// backend rejects — losing whole programs silently.
func TestBind_FailsClosedWhenTheContractIsUnavailable(t *testing.T) {
	src := &fakeContract{err: errors.New("Unimplemented: registry not wired")}

	_, err := Bind[testState](context.Background(), src, []Use[testState]{{
		Opcode: "set_task_due_date", Category: ChangeScalarSet, Input: "due_date", Current: dueDateReader,
	}})
	if err == nil {
		t.Fatal("⛔ Bind must FAIL CLOSED with no contract and no cache — an agent that starts without " +
			"its contract emits invalid args and loses whole programs at verify")
	}
}

// TestBind_ServesTheCacheWhenTheBackendIsDown is the warm case: a backend redeploy must not take the
// agent down with it.
func TestBind_ServesTheCacheWhenTheBackendIsDown(t *testing.T) {
	src := &fakeContract{ops: []Action{backendDueDate()}}
	cached := NewCachedContract(src, time.Hour)

	uses := []Use[testState]{{
		Opcode: "set_task_due_date", Category: ChangeScalarSet, Input: "due_date", Current: dueDateReader,
	}}
	if _, err := Bind[testState](context.Background(), cached, uses); err != nil {
		t.Fatalf("first bind: %v", err)
	}

	src.err = errors.New("connection refused") // the backend goes away
	if _, err := Bind[testState](context.Background(), cached, uses); err != nil {
		t.Errorf("a WARM agent must survive a backend outage on its cached contract, got: %v", err)
	}
}

// TestCachedContract_RefreshesAfterTheTTL is the attack a designer raised against last-known-good:
// an indefinite cache fails CLOSED on a value the backend added — the agent rejects output the
// backend now accepts. A TTL bounds how long that can last.
func TestCachedContract_RefreshesAfterTheTTL(t *testing.T) {
	src := &fakeContract{ops: []Action{backendDueDate()}}
	cached := NewCachedContract(src, time.Hour)

	if _, err := cached.BackendOps(context.Background()); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := cached.BackendOps(context.Background()); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if src.calls != 1 {
		t.Errorf("within the TTL the cache must serve, got %d upstream calls", src.calls)
	}

	cached.expireForTest()
	if _, err := cached.BackendOps(context.Background()); err != nil {
		t.Fatalf("post-TTL fetch: %v", err)
	}
	if src.calls != 2 {
		t.Errorf("⛔ after the TTL the cache must REFRESH (got %d calls) — an indefinite cache rejects "+
			"enum values a deploy added, failing closed on valid model output", src.calls)
	}
}

// TestCachedContract_HandsOutACopy guards the same defect Capabilities() was fixed for, one layer
// down: BackendOps returned the LIVE backing slice on all three paths (hit, miss, outage-fallback),
// under a mutex released at return.
//
// ⭐ Why this is worse than it sounds. The contract is the agent's copy of what the BACKEND accepts.
// A caller that filters it per tenant ("this tenant may not archive") or normalizes an operand
// mutates the shared cache for every subsequent agent in the process — and the corruption is
// COHERENT and SILENT: Bind then verifies against the mutated contract, so the gate agrees with
// itself while both disagree with the backend. The failure surfaces as valid model output being
// rejected, arbitrarily far from the mutation.
func TestCachedContract_HandsOutACopy(t *testing.T) {
	// ⚠️ A DEDICATED fixture, not the shared backendDueDate(): every reference field Action owns must
	// be NON-EMPTY, or each guard below passes vacuously against a nil slice and proves nothing. (The
	// shared fixture sets none of the four beyond Operands — the first version of this test was
	// checking four fields that did not exist.)
	populated := Action{
		Opcode: "set_task_due_date", Tier: TierReversible,
		Description:    "Set a task's due date.",
		TerminalStates: []string{"completed"},
		Structured:     []string{"payload"},
		UserFills:      []string{"due_date"},
		Args:           map[string]string{"preset": "eod"},
		Operands: []Operand{
			{Name: "due_date", Type: "enum", Required: true, EnumValues: []string{"today", "tomorrow"}},
		},
	}
	src := &fakeContract{ops: []Action{populated}}
	cached := NewCachedContract(src, time.Hour)

	first, err := cached.BackendOps(context.Background())
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if len(first) == 0 || len(first[0].Operands) == 0 || len(first[0].Operands[0].EnumValues) == 0 ||
		len(first[0].TerminalStates) == 0 || len(first[0].Structured) == 0 ||
		len(first[0].UserFills) == 0 || first[0].Args == nil {
		t.Fatalf("the fixture must populate EVERY reference field or the guards below are vacuous, got %+v", first[0])
	}

	// A caller adapting the contract — the realistic case, not a contrived one. Every REFERENCE field
	// Action owns is mutated, because a copy that covers only the obvious two still aliases the rest.
	first[0].Opcode = "mutated_by_caller"
	first[0].Operands[0].Required = !first[0].Operands[0].Required
	if len(first[0].Operands[0].EnumValues) > 0 {
		first[0].Operands[0].EnumValues[0] = "mutated_enum"
	}
	if len(first[0].TerminalStates) > 0 {
		first[0].TerminalStates[0] = "mutated_terminal"
	}
	if len(first[0].Structured) > 0 {
		first[0].Structured[0] = "mutated_structured"
	}
	if len(first[0].UserFills) > 0 {
		first[0].UserFills[0] = "mutated_userfill"
	}
	if first[0].Args != nil {
		first[0].Args["mutated_key"] = "mutated"
	}

	second, err := cached.BackendOps(context.Background()) // served from cache, within the TTL
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if second[0].Opcode == "mutated_by_caller" {
		t.Errorf("⛔ the cache handed out its backing slice: a caller's edit corrupted the contract "+
			"for every subsequent agent (opcode is now %q)", second[0].Opcode)
	}
	if second[0].Operands[0].Required == first[0].Operands[0].Required {
		t.Error("⛔ the Operands slice is shared: mutating an operand through one caller's copy " +
			"changed the cached contract — Bind would then verify against a contract the backend " +
			"never published")
	}
	if slices.Contains(second[0].Operands[0].EnumValues, "mutated_enum") {
		t.Error("⛔ Operand.EnumValues is shared — the field whose drift already caused a prod bug")
	}
	if slices.Contains(second[0].TerminalStates, "mutated_terminal") {
		t.Error("⛔ Action.TerminalStates is shared")
	}
	if slices.Contains(second[0].Structured, "mutated_structured") {
		t.Error("⛔ Action.Structured is shared")
	}
	if slices.Contains(second[0].UserFills, "mutated_userfill") {
		t.Error("⛔ Action.UserFills is shared")
	}
	if _, aliased := second[0].Args["mutated_key"]; aliased {
		t.Error("⛔ Action.Args is shared — a map, so it aliases even through a slice copy")
	}
}

// TestBindMixed_LocalActsNeedNoBackendOp is the fix for the finding that Bind alone would have
// REJECTED an entire agent.
//
// ⭐ Verified against the real consumers: one ops agent declared 11 capabilities while the backend
// op-set had 24 — the overlap was ZERO. Its acts were service RPCs and in-memory accumulation, not
// backend ops. A per-AGENT contract source rejects it outright; the source must be per-ACT.
func TestBindMixed_LocalActsNeedNoBackendOp(t *testing.T) {
	src := &fakeContract{ops: []Action{backendDueDate()}}

	set, err := BindMixed[testState](context.Background(), src,
		[]Use[testState]{{ // fetched from the backend
			Opcode: "set_task_due_date", Category: ChangeScalarSet, Input: "due_date", Current: dueDateReader,
		}},
		[]Local[testState]{{ // LOCAL: the agent owns this contract; the backend has never heard of it
			Action: Action{
				Opcode: "write_draft", Category: ChangeAlwaysMints, Tier: TierReversible,
				Description: "Draft the reply in full.",
				Operands:    []Operand{{Name: "body", Type: "string", Required: true}},
			},
		}},
	)
	if err != nil {
		t.Fatalf("an agent whose acts are mostly LOCAL must bind: %v", err)
	}
	if len(set.Actions()) != 2 {
		t.Fatalf("both the fetched and local acts must be present, got %d", len(set.Actions()))
	}
	if _, ok := set.Lookup("write_draft"); !ok {
		t.Error("the local act is missing — Bind must not require every act to be a backend op")
	}
}

// TestBind_DetectsContractDrift is what makes the copy unnecessary rather than merely discouraged:
// if an agent DOES restate a field, Bind checks it against the backend instead of trusting it.
func TestBind_DetectsContractDrift(t *testing.T) {
	src := &fakeContract{ops: []Action{backendDueDate()}}

	_, err := Bind[testState](context.Background(), src, []Use[testState]{{
		Opcode: "set_task_due_date", Category: ChangeScalarSet, Input: "due_date", Current: dueDateReader,
		// The agent asserts a tier the backend contradicts. Understating a tier is a SAFETY bug: it
		// would let the gate auto-run an act the backend considers irreversible.
		AssertTier: TierReadOnlyExplicit,
	}})
	if err == nil {
		t.Fatal("⛔ an agent-asserted tier that CONTRADICTS the backend must be rejected — understating " +
			"a tier lets the gate auto-run an act the backend says can never run live")
	}
}

// TestBind_CeilingWithoutAskIsRejected is A's best guarantee, and it is the whole reason Run and Ask
// must be TYPED fields rather than an opaque bag: the kit can only enforce a rule about fields it
// can SEE.
//
// ⭐ IT IS A REAL PRODUCTION BUG AS A STARTUP ERROR. An ops agent declared two propose-tier
// capabilities, the gate correctly withheld them into Plan.Propose — and nothing consumed that half.
// No error, no log; the ask simply never reached the user. Here the same shape cannot start.
func TestBind_CeilingWithoutAskIsRejected(t *testing.T) {
	src := &fakeContract{ops: []Action{backendDueDate(), backendArchive()}}

	t.Run("above the ceiling with no Ask is rejected", func(t *testing.T) {
		_, err := Bind[testState](context.Background(), src, []Use[testState]{{
			// archive_project is TierIrreversible per the backend, so the gate can only ever withhold
			// it — and with no Ask there is nothing to show the human.
			Opcode: "archive_project", Category: ChangeTerminal, TerminalStates: []string{"archived"},
			Current: func(_ context.Context, s *testState, _ Action) (string, error) { return s.status, nil },
		}})
		if err == nil {
			t.Fatal("⛔ an act the tier ALWAYS withholds, with no Ask, must be rejected at bind — the " +
				"gate withholds it and the user is never asked, so the act vanishes silently")
		}
		if !strings.Contains(err.Error(), "archive_project") || !strings.Contains(err.Error(), "Ask") {
			t.Errorf("the error must name the act and the missing field, got: %v", err)
		}
	})

	t.Run("the same act WITH an Ask binds", func(t *testing.T) {
		_, err := Bind[testState](context.Background(), src, []Use[testState]{{
			Opcode: "archive_project", Category: ChangeTerminal, TerminalStates: []string{"archived"},
			Current: func(_ context.Context, s *testState, _ Action) (string, error) { return s.status, nil },
			Ask:     func(Action) string { return "Archive this project?" },
		}})
		if err != nil {
			t.Errorf("a withheld act WITH wording is well-formed: %v", err)
		}
	})

	t.Run("an auto-runnable act needs no Ask", func(t *testing.T) {
		_, err := Bind[testState](context.Background(), src, []Use[testState]{{
			Opcode: "set_task_due_date", Category: ChangeScalarSet, Input: "due_date", Current: dueDateReader,
		}})
		if err != nil {
			t.Errorf("a reversible act may run live, so it needs no wording: %v", err)
		}
	})
}

// TestBind_RunOnAWithheldActIsRejected is the mirror: declaring a handler for an act the tier can
// NEVER run live is dead code whose presence implies it might execute.
func TestBind_RunOnAWithheldActIsRejected(t *testing.T) {
	src := &fakeContract{ops: []Action{backendArchive()}}

	_, err := Bind[testState](context.Background(), src, []Use[testState]{{
		Opcode: "archive_project", Category: ChangeTerminal, TerminalStates: []string{"archived"},
		Current: func(_ context.Context, s *testState, _ Action) (string, error) { return s.status, nil },
		Ask:     func(Action) string { return "Archive it?" },
		Run:     func(context.Context, *testState, Action) error { return nil }, // can never run
	}})
	if err == nil {
		t.Fatal("a Run handler on an act the backend says is irreversible must be rejected — the gate " +
			"can never dispatch it, so the handler is dead code that reads as live")
	}
}

// TestParseTier_OwnsTheInverse closes a SAFETY seam the kit refused to own.
//
// ⛔ The kit exported Tier.String() and no inverse, so EVERY ContractSource had to hand-roll the
// mapping from a wire string back to a Tier — and the kit's zero value is TierReadOnly, which
// AUTO-RUNS. The first consumer got it right and left a friction report in a comment:
// "an unrecognised tier that fell through to the zero value would
// silently promote an irreversible op to 'safe to run live', which is precisely the failure the tier
// system exists to prevent."
//
// ⭐ They defaulted to the SAFEST tier. The next author writing the obvious switch with no default
// gets the zero value — and a destructive act runs unattended. That is the kit asking a consumer for
// something it could compute, on the one axis where a mistake is unrecoverable.
func TestParseTier_OwnsTheInverse(t *testing.T) {
	t.Run("round-trips every tier", func(t *testing.T) {
		for _, want := range []Tier{TierReadOnly, TierReversible, TierExternalReversible, TierIrreversible} {
			got, err := ParseTier(want.String())
			if err != nil {
				t.Errorf("ParseTier(%q): %v", want.String(), err)
				continue
			}
			if normalizeTier(got) != normalizeTier(want) {
				t.Errorf("ParseTier(%q) = %v, want %v", want.String(), got, want)
			}
		}
	})

	t.Run("⭐ an UNKNOWN tier fails to the SAFEST, never the zero value", func(t *testing.T) {
		got, err := ParseTier("some_tier_a_future_backend_added")
		if err == nil {
			t.Error("an unrecognized tier must ERROR — silently accepting it is how a contract change " +
				"becomes an unattended destructive act")
		}
		if got != TierIrreversible {
			t.Errorf("the returned tier must be the SAFEST (%v), not the zero value (%v) — a caller that "+
				"ignores the error must still fail closed, got %v", TierIrreversible, TierReadOnly, got)
		}
	})

	t.Run("read-only round-trips to the EXPLICIT spelling", func(t *testing.T) {
		// Otherwise parsing a legitimate read-only tier produces a bare zero, which ToolSet.Validate
		// rejects as "you forgot to declare a tier".
		got, _ := ParseTier("read_only")
		if got != TierReadOnlyExplicit {
			t.Errorf("got %v, want TierReadOnlyExplicit — a parsed read-only tier is a STATEMENT, and "+
				"must not be indistinguishable from a forgotten field", got)
		}
	})
}

// TestBindMixed_LocalAskSurvivesTheEmbedding is the regression for a shadowing bug the kit inflicted
// on itself: Local EMBEDS Action (which has an Ask field) and ALSO declares its own Ask, so `l.Ask`
// resolves to Local's and `l.Action.Ask` stays nil.
//
// ⚠️ BindMixed stored l.Action bare, so the wording was dropped at the seam — and ActionSet.Validate,
// which reads a.Ask, then REJECTED the declaration as "above the auto-ceiling but has no Ask". The
// kit refused the exact shape its own docs tell you to write. The one production consumer escaped
// only by happening to set Ask on the INNER Action.
func TestBindMixed_LocalAskSurvivesTheEmbedding(t *testing.T) {
	local := []Local[testState]{{
		Action: Action{
			Opcode: "send_it", Category: ChangeAlwaysMints, Tier: TierIrreversible,
			Description: "Send the thing.",
			Operands:    []Operand{{Name: "to", Type: "string", Required: true}},
		},
		Ask: func(Action) string { return "Send it?" }, // ⭐ on Local, NOT on the embedded Action
	}}

	set, err := BindMixed[testState](context.Background(), nil, nil, local)
	if err != nil {
		t.Fatalf("⛔ a propose-only local act declaring Ask on Local must bind: %v", err)
	}
	if err := set.Validate(); err != nil {
		t.Fatalf("⛔ ...and must VALIDATE — Validate reads Action.Ask, so the seam has to carry it: %v", err)
	}
	act, ok := set.Lookup("send_it")
	if !ok {
		t.Fatal("the act must be declared")
	}
	if act.Ask == nil {
		t.Error("⛔ Action.Ask is nil — the wording was lost at the embedding, so a consumer surfacing " +
			"the withheld act has no sentence to put to the user")
	} else if got := act.Ask(act); got != "Send it?" {
		t.Errorf("Ask returned %q, want %q", got, "Send it?")
	}
}
