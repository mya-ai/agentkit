package agentkit

import (
	"encoding/json"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// The CAPABILITY seam — TDD. These tests are written BEFORE the implementation and define it.
//
// WHY THIS IS FRAMEWORK, NOT AGENT (the merge-blocking test: "Is this solving an AGENT-SPECIFIC problem,
// or re-solving a FRAMEWORK problem locally?"):
//
//	"this agent can set a disposition"                             → AGENT identity  (the consumer)
//	"an agent is handed a set of capabilities, chooses among them,
//	 and Go verifies + tier-gates + applies-or-proposes them"      → FRAMEWORK       (here)
//
// THE REAL PROBLEM THIS SOLVES. In the codebase this kit came from, the op REGISTRY already existed and was
// good (an op spec carries Description + Tier + typed Operands; Verify + Replay exist). What was missing is
// the AGENT-SIDE SEAM onto it. The reference planning agent hand-mirrored the registry in a local
// hand-mirrored operand table, guarded by a drift test, and its own comment admitted WHY:
//
//	"The production worker can't import the registry package (it pulls the service's storage layer
//	 and its SQL driver across the worker↔service boundary), so this local mirror exists; the test
//	 keeps it honest."
//
// That is a workaround for a BOUNDARY problem (a worker must not import a service's internals) — and it is
// exactly the anti-pattern that should be merge-blocking. The fix is NOT a better mirror. It is:
//
//	⭐ AN AGENT RECEIVES ITS CAPABILITIES AS DATA, NOT BY IMPORTING A PACKAGE.
//
// The wire already carries it (a backend's list-ops RPC can serve opcode/label/description/tier/
// max-rung/operands). So this seam takes a []Capability — from an RPC, a fake, or a literal — and owns
// the three things EVERY tool-using agent otherwise hand-rolls:
//
//	1. RENDER   the capabilities into a tool-list the LLM can choose from (no hand-written prompt lists)
//	2. VERIFY   the LLM's calls against the specs (unknown opcode / unknown operand / missing required)
//	3. GATE     each call by its TIER — reversible runs; irreversible can only be PROPOSED (safety
//	            is STRUCTURAL, in code, never the prompt)
//
// The kit must NOT import your backend, so Capability is a kit-owned, generic shape; your registry's own
// op spec maps onto it at the seam. This is what closes the WRITE consumer's case ("investigate → propose
// a multi-op write program") and gives a hand-maintained mirror a seam to migrate onto.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// testCaps is a small capability set standing in for a real registry (the shape a backend's list-ops
// RPC serves). Deliberately spans the tiers so the gate is exercised end-to-end.
func testCaps() []Capability {
	return []Capability{
		{
			Name:        "set_disposition",
			Description: "Set the GTD verdict on an item. Use when you have judged what the item is.",
			Tier:        TierReversible, // a mutation with a real undo → may run live
			Args: []CapabilityArg{
				{Name: "item_id", Description: "The item to set.", Type: "string", Required: true},
				{Name: "disposition", Description: "The GTD verb.", Type: "enum", Required: true,
					EnumValues: []string{"do", "schedule", "delegate", "reference", "archive"}},
			},
		},
		{
			Name:        "draft_reply",
			Description: "Prepare (but do NOT send) a reply. Use when the item needs a response you can write.",
			Tier:        TierReversible, // DRAFTING is reversible; SENDING would not be
			Args: []CapabilityArg{
				{Name: "item_id", Description: "The item.", Type: "string", Required: true},
				{Name: "body", Description: "The drafted reply.", Type: "string", Required: true},
			},
		},
		{
			Name:        "send_reply",
			Description: "SEND a reply to a person. Irreversible from the recipient's point of view.",
			Tier:        TierExternalReversible, // above the ceiling → PROPOSE ONLY, never live
			Args: []CapabilityArg{
				{Name: "item_id", Description: "The item.", Type: "string", Required: true},
			},
		},
	}
}

// ── 1. RENDER — the capabilities become a tool-list the LLM can choose from ──────────────────────
//
// The load-bearing property: the prompt is GENERATED FROM THE SPECS, never hand-written. That is what
// kills the drift (the "N hand-maintained parallel lists" problem). If a capability is added or removed, the
// prompt changes by construction.

func TestRenderCapabilities_IsGeneratedFromTheSpecs(t *testing.T) {
	got := RenderCapabilities(testCaps())

	// The agent must see, for every capability: what it is called, what it DOES (the semantics — a bare
	// opcode is not enough for the model to choose correctly), and what to fill in.
	for _, want := range []string{
		"set_disposition", "Set the GTD verdict",
		"draft_reply", "Prepare (but do NOT send)",
		"send_reply",
		"item_id", "disposition",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered capabilities missing %q\n--- got ---\n%s", want, got)
		}
	}

	// Enum values must be rendered — otherwise the model invents verbs ("todo", "urgent") and every call
	// fails verification. (This is a real failure mode: the hand-maintained mirror existed partly to catch
	// model-invented operands.)
	for _, v := range []string{"do", "schedule", "delegate", "reference", "archive"} {
		if !strings.Contains(got, v) {
			t.Errorf("rendered capabilities missing enum value %q", v)
		}
	}

	// Required-ness must be visible, or the model omits required args and the program is dead on arrival.
	if !strings.Contains(strings.ToLower(got), "required") {
		t.Errorf("rendered capabilities never mark an arg required\n--- got ---\n%s", got)
	}
}

// ── 2. VERIFY — the LLM's calls are checked against the specs ────────────────────────────────────
//
// The model WILL invent opcodes and operands. Verification is what makes that safe, and it must happen
// in CODE (never "please only use these ops" in the prompt).

func TestVerifyCalls_RejectsWhatTheModelInvents(t *testing.T) {
	caps := testCaps()

	tests := []struct {
		name    string
		call    ToolCall
		wantErr string
	}{
		{
			name:    "unknown capability — the model invented an opcode",
			call:    ToolCall{Name: "delete_everything", Args: map[string]any{}},
			wantErr: "unknown capability",
		},
		{
			// ⚠️ PRESENT-BUT-BLANK IS THE SAME FAILURE AS ABSENT, and only the absent
			// case was tested — dropping the emptiness half of this check survived a
			// full mutation run. An empty required value passes verification and lands
			// at the write, which is the blown-up write this gate exists to prevent.
			name: "required arg present but EMPTY — same failure as absent",
			call: ToolCall{Name: "set_disposition", Args: map[string]any{
				"item_id": "", "disposition": "do",
			}},
			wantErr: "missing required arg",
		},
		{
			name: "required arg present but WHITESPACE only",
			call: ToolCall{Name: "set_disposition", Args: map[string]any{
				"item_id": "   ", "disposition": "do",
			}},
			wantErr: "missing required arg",
		},
		{
			name: "unknown arg — the model invented an operand",
			call: ToolCall{Name: "set_disposition", Args: map[string]any{
				"item_id": "i1", "disposition": "do", "vibe": "urgent",
			}},
			wantErr: "unknown arg",
		},
		{
			name:    "missing required arg — the program would be dead on arrival",
			call:    ToolCall{Name: "set_disposition", Args: map[string]any{"item_id": "i1"}},
			wantErr: "missing required arg",
		},
		{
			name: "bad enum — the model invented a GTD verb",
			call: ToolCall{Name: "set_disposition", Args: map[string]any{
				"item_id": "i1", "disposition": "urgent",
			}},
			wantErr: "not a valid value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyCall(caps, tc.call)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil (the call was accepted!)", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyCalls_AcceptsAValidCall(t *testing.T) {
	err := VerifyCall(testCaps(), ToolCall{
		Name: "set_disposition",
		Args: map[string]any{"item_id": "i1", "disposition": "schedule"},
	})
	if err != nil {
		t.Fatalf("a valid call was rejected: %v", err)
	}
}

// ⭐ REGRESSION (found by the tool-call eval, and it was a real bug in THIS seam, not the model).
//
// ToolCall.Args was typed map[string]string. But a model emitting a well-formed program writes a NUMBER
// for a number:
//
//	{"name": "set_estimate", "args": {"item_id": "ITEM", "minutes": 20}}
//	                                                     ^^^^^^^^^^^^^ a JSON number, not "20"
//
// …and the whole structured call failed to parse — killing an otherwise PERFECT program (the model had
// sharpened the action, dated it, estimated it, and drafted the reply with a [DATE] slot). The model was
// right and the seam was wrong: forcing every arg to a string makes a typed `int`/`bool` operand
// unexpressible, so the kit would have punished correct behaviour forever.
//
// Args is now map[string]any and the seam normalizes: it accepts a JSON number/bool/string alike and
// compares enums on the rendered value. The lesson is the reason the eval exists — an untested contract is
// a guess, and this one was wrong on the first real program the model wrote.
func TestVerifyCalls_AcceptsTypedArgs_NotJustStrings(t *testing.T) {
	caps := []Capability{{
		Name: "set_estimate", Description: "How long, in minutes.", Tier: TierReversible,
		Args: []CapabilityArg{
			{Name: "item_id", Description: "The item.", Type: "string", Required: true},
			{Name: "minutes", Description: "Whole minutes.", Type: "int", Required: true},
		},
	}}

	// A JSON number — what a well-behaved model actually emits for an int operand.
	if err := VerifyCall(caps, ToolCall{
		Name: "set_estimate",
		Args: map[string]any{"item_id": "i1", "minutes": float64(20)}, // encoding/json decodes numbers as float64
	}); err != nil {
		t.Errorf("a numeric arg was rejected — the seam is forcing strings and would punish a correct program: %v", err)
	}

	// A string is still fine (models are inconsistent; both must work).
	if err := VerifyCall(caps, ToolCall{
		Name: "set_estimate",
		Args: map[string]any{"item_id": "i1", "minutes": "20"},
	}); err != nil {
		t.Errorf("a string arg was rejected: %v", err)
	}
}

// The whole point of the fix: a real program the model emitted must round-trip through the actual JSON the
// LLM returns. This pins the wire contract, not just the Go types.
func TestToolCall_UnmarshalsTheJSONAModelActuallyWrites(t *testing.T) {
	raw := `{"name":"set_estimate","args":{"item_id":"ITEM","minutes":20}}`
	var call ToolCall
	if err := json.Unmarshal([]byte(raw), &call); err != nil {
		t.Fatalf("a real model program failed to unmarshal: %v", err)
	}
	if call.ArgString("minutes") != "20" {
		t.Errorf("ArgString(minutes) = %q, want \"20\" — the seam must render a numeric arg for the consumer",
			call.ArgString("minutes"))
	}
	if n, ok := call.ArgInt("minutes"); !ok || n != 20 {
		t.Errorf("ArgInt(minutes) = (%d, %v), want (20, true)", n, ok)
	}
}

// ── 3. GATE — the TIER decides run-live vs propose. THIS IS THE SAFETY INVARIANT. ────────────────
//
// "Safety is STRUCTURAL, in trusted code, never the prompt." An irreversible capability must be
// PHYSICALLY UNABLE to execute in the loop — not merely discouraged. The gate is what makes
// "model proposes, code disposes" true rather than aspirational.

func TestGate_ReversibleRunsLive_IrreversibleCanOnlyBeProposed(t *testing.T) {
	caps := testCaps()

	program := []ToolCall{
		{Name: "set_disposition", Args: map[string]any{"item_id": "i1", "disposition": "do"}},
		{Name: "draft_reply", Args: map[string]any{"item_id": "i1", "body": "Hi Dana…"}},
		{Name: "send_reply", Args: map[string]any{"item_id": "i1"}}, // ← above the ceiling
	}

	plan, err := GateProgram(caps, program, RungAuto)
	if err != nil {
		t.Fatalf("GateProgram: %v", err)
	}

	// The two reversible calls may run directly.
	if len(plan.Apply) != 2 {
		t.Fatalf("Apply = %d calls, want 2 (set_disposition + draft_reply)", len(plan.Apply))
	}
	// ⭐ The irreversible one is CAPTURED as a proposal — never executed.
	if len(plan.Propose) != 1 {
		t.Fatalf("Propose = %d calls, want 1 (send_reply)", len(plan.Propose))
	}
	if plan.Propose[0].Name != "send_reply" {
		t.Errorf("proposed %q, want send_reply", plan.Propose[0].Name)
	}
	// And it must NOT have leaked into the apply set — the whole point.
	for _, c := range plan.Apply {
		if c.Name == "send_reply" {
			t.Fatal("SAFETY VIOLATION: an external/irreversible capability reached the APPLY set")
		}
	}
}

// A caller may demand that NOTHING runs live (rung=ask) — e.g. an agent that has not yet earned autonomy
// (the doctrine's "shadowing earns authority"). Then even reversible calls become proposals.
func TestGate_RungAsk_ProposesEverything(t *testing.T) {
	caps := testCaps()
	program := []ToolCall{
		{Name: "set_disposition", Args: map[string]any{"item_id": "i1", "disposition": "do"}},
	}

	plan, err := GateProgram(caps, program, RungAsk)
	if err != nil {
		t.Fatalf("GateProgram: %v", err)
	}
	if len(plan.Apply) != 0 {
		t.Errorf("Apply = %d, want 0 — rung=ask means nothing runs live", len(plan.Apply))
	}
	if len(plan.Propose) != 1 {
		t.Errorf("Propose = %d, want 1", len(plan.Propose))
	}
}

// GateProgram must VERIFY before it gates — an invalid call must never reach either set. (Otherwise a
// model-invented opcode would sail through into Apply and blow up at the write.)
func TestGate_VerifiesFirst(t *testing.T) {
	_, err := GateProgram(testCaps(), []ToolCall{
		{Name: "set_disposition", Args: map[string]any{"item_id": "i1"}}, // missing required
	}, RungAuto)
	if err == nil {
		t.Fatal("GateProgram accepted an invalid call — verification must run BEFORE gating")
	}
	if !strings.Contains(err.Error(), "missing required arg") {
		t.Errorf("error = %q, want a verification error", err)
	}
}

// ArgStringList reads a list arg from a JSON array, a comma string, or a single scalar — and a list arg's
// EnumValues validate EACH element, not the stringified whole.
func TestArgStringList_And_ListEnumValidation(t *testing.T) {
	// JSON array (the normal case).
	c := ToolCall{Name: "tag", Args: map[string]any{"tags": []any{"a", "b", ""}}}
	if got := c.ArgStringList("tags"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("ArgStringList(array) = %v, want [a b] (blank dropped)", got)
	}
	// A single scalar → a one-element list.
	c2 := ToolCall{Name: "tag", Args: map[string]any{"tags": "a"}}
	if got := c2.ArgStringList("tags"); len(got) != 1 || got[0] != "a" {
		t.Errorf("ArgStringList(scalar) = %v, want [a]", got)
	}
	// A comma-separated string → split.
	c3 := ToolCall{Name: "tag", Args: map[string]any{"tags": "a, b"}}
	if got := c3.ArgStringList("tags"); len(got) != 2 {
		t.Errorf("ArgStringList(csv) = %v, want 2 entries", got)
	}

	caps := []Capability{{
		Name: "tag", Description: "d", Tier: TierReversible,
		Args: []CapabilityArg{{Name: "tags", Type: "enum", List: true, EnumValues: []string{"a", "b"}}},
	}}
	// Every element in the closed set → OK.
	if err := VerifyCall(caps, ToolCall{Name: "tag", Args: map[string]any{"tags": []any{"a", "b"}}}); err != nil {
		t.Errorf("valid list-enum rejected: %v", err)
	}
	// One element outside the set → rejected (not the stringified whole).
	if err := VerifyCall(caps, ToolCall{Name: "tag", Args: map[string]any{"tags": []any{"a", "z"}}}); err == nil {
		t.Error("VerifyCall accepted an out-of-set list element — each element must be validated")
	}
}

// TestGateProgram_RejectsAForgottenTier moves the kit's headline safety promise onto the path that
// consumers actually call.
//
// ⛔ THE PROMISE: "an irreversible act can never auto-run" (doc.go:5, tier.go, capability.go).
// ⛔ THE REALITY, before this: the guard lived in ToolSet.Validate — an OPT-IN method that
// `grep '\.Validate()' pkg/*/` shows NO production consumer calls. Meanwhile GateProgram is a free
// function over []Capability that never validated anything:
//
//	caps := []Capability{{Name: "delete_everything"}}   // Tier forgotten
//	GateProgram(caps, ...) -> err=<nil> Apply=1 Propose=0
//
// ⭐ The kit correctly diagnosed the zero-value trap, wrote the guard, and left it OFF THE EXECUTION
// PATH. A guard nobody calls is prose. This is the same lesson as the unfireable test guard: the
// check has to sit where the work happens.
//
// GateProgram now rejects a bare zero Tier. Read-only acts declare TierReadOnlyExplicit — so "this
// may auto-run" is a statement, never an omission.
func TestGateProgram_RejectsAForgottenTier(t *testing.T) {
	t.Run("a forgotten Tier is rejected AT THE GATE", func(t *testing.T) {
		caps := []Capability{{Name: "delete_everything", Description: "destroys everything"}}
		plan, err := GateProgram(caps, []ToolCall{{Name: "delete_everything"}}, RungAuto)
		if err == nil {
			t.Fatalf("⛔ a forgotten Tier made a destructive act AUTO-RUN (Apply=%d) — the kit's headline "+
				"promise must hold at the entry point, not in an opt-in validator", len(plan.Apply))
		}
		if !strings.Contains(err.Error(), "delete_everything") || !strings.Contains(err.Error(), "Tier") {
			t.Errorf("the error must name the capability and the missing field, got: %v", err)
		}
	})

	t.Run("an EXPLICIT read-only act still auto-runs", func(t *testing.T) {
		caps := []Capability{{Name: "search", Description: "looks up", Tier: TierReadOnlyExplicit}}
		plan, err := GateProgram(caps, []ToolCall{{Name: "search"}}, RungAuto)
		if err != nil {
			t.Fatalf("an explicitly read-only act must pass: %v", err)
		}
		if len(plan.Apply) != 1 {
			t.Errorf("it must auto-run, got Apply=%d Propose=%d", len(plan.Apply), len(plan.Propose))
		}
	})

	t.Run("an irreversible act is still withheld, not rejected", func(t *testing.T) {
		caps := []Capability{{Name: "send", Description: "sends", Tier: TierIrreversible}}
		plan, err := GateProgram(caps, []ToolCall{{Name: "send"}}, RungAuto)
		if err != nil {
			t.Fatalf("a properly declared irreversible act must GATE, not error: %v", err)
		}
		if len(plan.Propose) != 1 {
			t.Errorf("want Propose=1, got Apply=%d Propose=%d", len(plan.Apply), len(plan.Propose))
		}
	})
}

// ⭐ THE TRAP THIS PINS. ArgInt originally handled only float64 and string. Args
// normally arrive from json.Unmarshal, where every number is a float64 — so that
// version passed every test that went through the wire, and SILENTLY REJECTED a
// hand-built map[string]any{"n": 812}. A required int vanished into ok=false.
//
// A newcomer hit it writing their first test with a literal int, and the symptom
// (a required arg simply missing) points nowhere near the cause. The asymmetry is
// the tell: it accepted the STRING "812" while rejecting the INT 812.
func TestArgInt_AcceptsEveryIntegerSpelling(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		want int
		ok   bool
	}{
		{"native int — the one that used to fail", 812, 812, true},
		{"int64", int64(812), 812, true},
		{"int32", int32(812), 812, true},
		{"uint", uint(812), 812, true},
		{"float64 (what json.Unmarshal produces)", float64(812), 812, true},
		{"float32", float32(812), 812, true},
		{"json.Number", json.Number("812"), 812, true},
		{"numeric string (models emit these)", "812", 812, true},
		{"padded numeric string", "  812  ", 812, true},
		{"prose is not a number", "eight hundred", 0, false},
		{"absent", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{}
			if tc.v != nil {
				args["n"] = tc.v
			}
			got, ok := ToolCall{Args: args}.ArgInt("n")
			if got != tc.want || ok != tc.ok {
				t.Errorf("ArgInt(%#v) = (%d, %v), want (%d, %v)", tc.v, got, ok, tc.want, tc.ok)
			}
		})
	}
}
