package agentkit

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// CAPABILITIES — the agent-side seam onto an op registry.
//
// THE PROBLEM. In the codebase this kit came from, the op REGISTRY already existed and was good: an OpSpec
// carries the opcode, its agent-facing Description, its reversibility Tier, and typed Operands; Verify
// checks a program before anything runs and Replay executes it deterministically. What was MISSING is the
// seam that lets an AGENT reason over one.
//
// So a production agent HAND-MIRRORED the registry in a local operand table, guarded by
// a drift test, and its own comment admitted why:
//
//	"The production worker can't import the registry package (it pulls the service's storage layer
//	 and its SQL driver across the worker↔service boundary), so this local mirror exists; the test
//	 keeps it honest."
//
// That is a workaround for a BOUNDARY problem (a worker must not import a service's internals), and the
// cost was real: every capability add/remove edited N hand-maintained parallel lists (the verdict field,
// the mirror, its drift test, and the apply switch). Each new agent re-solved the same plumbing.
//
// THE FIX IS NOT A BETTER MIRROR. It is:
//
//	⭐ AN AGENT RECEIVES ITS CAPABILITIES AS DATA — it does not import a package to learn them.
//
// The wire already carries exactly this (a backend's list-ops RPC can serve opcode/label/description/
// tier/max-rung/operands). This file takes that data — from an RPC, a fake, or a literal — and owns the
// three things every tool-using agent otherwise hand-rolls:
//
//	1. RENDER — capabilities → a tool-list the LLM chooses from. GENERATED FROM THE SPECS, never
//	            hand-written, so it cannot drift.
//	2. VERIFY — the LLM's calls checked against the specs (the model WILL invent opcodes and operands).
//	3. GATE   — each call routed by its TIER: reversible may run live; above the ceiling it can only be
//	            PROPOSED. Safety is STRUCTURAL, in code, never "please don't" in the prompt.
//
// BOUNDARY: agentkit must NOT import your backend. So Capability is a kit-owned, generic shape and your
// registry's own op spec maps onto it at the seam (the mapping lives in the consumer, not here). The kit
// never learns what a "project" is.
//
// This closes the WRITE consumer's case ("investigate → propose a multi-op write program") and gives a
// hand-maintained mirror a seam to migrate onto.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// Capability is one thing an agent can DO, described well enough for a model to choose it correctly.
//
// It is deliberately the same shape a backend's list-ops RPC already serves — a capability is not a new
// concept, it is the existing op spec seen from the agent's side of the wire.
type Capability struct {
	// Name is the stable key the model emits (an opcode, e.g. "set_disposition").
	Name string
	// Description is the agent-facing SEMANTICS: what this does and WHEN to use it. Load-bearing — a bare
	// name plus arg types is NOT enough for a model to choose the right capability; it guesses instead.
	// (A well-built op registry makes the same point about its own Description field.)
	Description string
	// Tier is the reversibility class — the SAFETY FLOOR for this capability's autonomy. It is what
	// GateProgram routes on, and it is why an irreversible capability cannot be made live by a prompt.
	Tier Tier
	// Args are the typed operands the model must fill.
	Args []CapabilityArg
}

// CapabilityArg is one operand: what it means, its type, whether it is required, and (for enums) the
// allowed values. Rendering EnumValues is not cosmetic — without them the model invents values and every

// CapabilityArg is an alias for Operand: one declared input to an act.
//
// ⭐ THEY WERE TWO NAMES FOR ONE SHAPE, AND THE COMPILER SAID SO. The types were
// field-for-field identical, staticcheck flagged the conversion between them as
// redundant, and a converter existed purely to copy one into the other. A reader
// grepping "how do I declare an argument?" found two answers and no rule for
// choosing.
//
// ⚠️ It was not merely redundant, it was a defect: `List` was added to one and not
// the other, so a multi-valued arg was rendered to the model as a single choice and
// then REJECTED when the model correctly emitted an array. Two copies of a struct
// are two places to remember, and the copy that is forgotten fails silently.
//
// The alias keeps existing code compiling; Operand is the name to reach for. See
// Operand (action.go) for the field documentation.
type CapabilityArg = Operand

// ToolCall is one call the model emitted: a capability name + its args. This is what the model returns —
// DATA, never an executed effect. Go verifies it, gates it, and (only then) applies it.
//
// ⭐ Args is map[string]any, NOT map[string]string — and that is a hard-won correction.
//
// It was map[string]string, and the first real program a model wrote blew up on it:
//
//	{"name": "set_estimate", "args": {"item_id": "ITEM", "minutes": 20}}
//	                                                     ^^^^^^^^^^^^^ a JSON number
//
// The model was RIGHT (an int operand should be an int) and the seam was WRONG — and because the parse
// failed, an otherwise PERFECT program was thrown away (it had sharpened the action, dated it, estimated
// it, and drafted the reply). A string-only contract makes typed operands unexpressible and punishes
// correct behaviour. Models are also inconsistent — the same operand may arrive as 20 or "20" — so the
// seam ACCEPTS BOTH and normalizes at the accessors below.
type ToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ArgString renders an arg as a string, whatever JSON type it arrived as. Consumers read args through this
// (never by asserting the type themselves) so a model emitting 20 vs "20" is a non-event.
func (c ToolCall) ArgString(name string) string {
	v, ok := c.Args[name]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64: // encoding/json decodes every JSON number as float64
		// Render whole numbers without a trailing ".0" — "20", not "20.000000".
		if t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}

// ArgStringList reads a list-valued arg as []string, whatever shape the model chose. It accepts a JSON
// array (`["a","b"]` — the normal case), a single scalar (`"a"` → `["a"]`, since a model asked for a list
// often emits one bare value), or a comma-separated string (`"a, b"` → `["a","b"]`). Empty/blank entries
// are dropped; absent → nil. This is a framework accessor (not an agent's private parser) because "read a
// list arg" is generic — any capability with a multi-valued arg needs it (agentkit rule: generalizable
// machinery lives in the kit, never re-solved locally).
func (c ToolCall) ArgStringList(name string) []string {
	v, ok := c.Args[name]
	if !ok || v == nil {
		return nil
	}
	var out []string
	appendClean := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if e != nil {
				appendClean(fmt.Sprint(e))
			}
		}
	case []string:
		for _, s := range t {
			appendClean(s)
		}
	case string:
		// A single scalar or a comma-separated list — split on commas so "a, b" becomes two entries.
		for _, part := range strings.Split(t, ",") {
			appendClean(part)
		}
	default:
		appendClean(fmt.Sprint(t))
	}
	return out
}

// ArgInt reads an integer arg. ok=false when absent or non-numeric — the caller decides whether that is
// fatal (a required int) or fine (an optional one).
//
// ⚠️ IT MUST ACCEPT NATIVE Go INTEGERS, NOT ONLY JSON ONES. Args usually arrive from json.Unmarshal, where
// every number is a float64 — so a version handling only float64 and string passes every test that goes
// through the wire. It then SILENTLY REJECTS a hand-built map[string]any{"n": 812} in someone's test, and
// a required int vanishes into ok=false. That asymmetry (accepts "812", rejects 812) is exactly the trap a
// newcomer cannot debug, so every integer kind is handled explicitly.
func (c ToolCall) ArgInt(name string) (int, bool) {
	v, ok := c.Args[name]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case int:
		return t, true
	case int8:
		return int(t), true
	case int16:
		return int(t), true
	case int32:
		return int(t), true
	case int64:
		return int(t), true
	case uint:
		return int(t), true
	case uint8:
		return int(t), true
	case uint16:
		return int(t), true
	case uint32:
		return int(t), true
	case uint64:
		return int(t), true
	case float32:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// Plan is the gated result of a model-emitted program: what may run live, and what may only be proposed.
//
// The split is the whole point. It is not advisory — a call in Propose has NOT run and CANNOT run from
// here; the consumer must route it through a human-gated path (a draft-and-hold proposal).
type Plan struct {
	// Apply are the calls whose tier permits live execution at the requested rung. Go runs these.
	Apply []ToolCall
	// Propose are the calls whose tier exceeds the ceiling. They are CAPTURED, never executed.
	Propose []ToolCall
}

// RenderCapabilities turns a capability set into the tool-list the model chooses from.
//
// ⭐ THE LOAD-BEARING PROPERTY: this is GENERATED FROM THE SPECS. No agent hand-writes a list of its ops
// in its prompt. Add or remove a capability and the prompt changes by construction — which is exactly what
// kills the "N hand-maintained parallel lists" problem and its drift test.
//
// Ordering is stable (by name) so a prompt diff is meaningful and caching is not defeated by map order.
func RenderCapabilities(caps []Capability) string {
	sorted := make([]Capability, len(caps))
	copy(sorted, caps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	for _, c := range sorted {
		fmt.Fprintf(&b, "- %s — %s\n", c.Name, c.Description)
		for _, a := range c.Args {
			req := "optional"
			if a.Required {
				req = "REQUIRED"
			}
			kind := a.Type
			if a.List {
				kind = "list of " + a.Type // tell the model to emit a JSON array
			}
			fmt.Fprintf(&b, "    · %s (%s, %s) — %s", a.Name, kind, req, a.Description)
			if len(a.EnumValues) > 0 {
				// Without the allowed values the model invents its own and every call fails verify. For a
				// list the phrasing is "each one of" so the model knows the set constrains ELEMENTS.
				if a.List {
					fmt.Fprintf(&b, " Each one of: %s.", strings.Join(a.EnumValues, ", "))
				} else {
					fmt.Fprintf(&b, " One of: %s.", strings.Join(a.EnumValues, ", "))
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// VerifyCall checks one model-emitted call against the capability specs.
//
// The model WILL invent opcodes and operands — that is not a bug to prompt away, it is a fact to verify
// against. Every rejection here is a failure that would otherwise land as a silently-dropped proposal or a
// blown-up write (both real, both observed: the hand-maintained mirror existed to catch invented operands).
func VerifyCall(caps []Capability, call ToolCall) error {
	spec, ok := lookupCapability(caps, call.Name)
	if !ok {
		return fmt.Errorf("unknown capability %q (the model invented an opcode)", call.Name)
	}

	// Every arg the model sent must exist on the spec.
	for name := range call.Args {
		if _, ok := lookupArg(spec, name); !ok {
			return fmt.Errorf("%s: unknown arg %q (the model invented an operand)", call.Name, name)
		}
	}

	// Every REQUIRED arg must be present and non-empty. A required arg present-but-blank is the same
	// failure as absent — it would land as a missing-required at the write.
	//
	// Args are read through ArgString, which normalizes whatever JSON type the model chose (20 vs "20").
	// Never type-assert an arg directly here: that is what made the seam reject a correct program.
	for _, a := range spec.Args {
		_, present := call.Args[a.Name]
		if a.List {
			// A list arg: read as []string and validate each element (never the stringified whole).
			vals := call.ArgStringList(a.Name)
			if a.Required && len(vals) == 0 {
				return fmt.Errorf("%s: missing required arg %q", call.Name, a.Name)
			}
			if len(a.EnumValues) > 0 {
				for _, el := range vals {
					if !contains(a.EnumValues, el) {
						return fmt.Errorf("%s: %q is not a valid value for %q (allowed: %s)",
							call.Name, el, a.Name, strings.Join(a.EnumValues, ", "))
					}
				}
			}
			continue
		}
		v := call.ArgString(a.Name)
		if a.Required && (!present || strings.TrimSpace(v) == "") {
			return fmt.Errorf("%s: missing required arg %q", call.Name, a.Name)
		}
		// Enums are closed sets. An unset optional enum is fine; a SET one must be legal.
		if len(a.EnumValues) > 0 && present && v != "" && !contains(a.EnumValues, v) {
			return fmt.Errorf("%s: %q is not a valid value for %q (allowed: %s)",
				call.Name, v, a.Name, strings.Join(a.EnumValues, ", "))
		}
	}
	return nil
}

// GateProgram VERIFIES then TIER-GATES a whole model-emitted program.
//
// ⭐ THIS IS THE SAFETY INVARIANT ("safety is STRUCTURAL, in trusted code, never the prompt").
// A capability above the requested rung's ceiling is CAPTURED into Plan.Propose — it is physically unable
// to reach Plan.Apply, so it cannot be executed by this path however the model phrases itself. That is what
// makes "the model proposes, code disposes" true rather than aspirational.
//
// Verification runs FIRST, over the WHOLE program: an invalid call fails the program rather than being
// silently dropped. (Silently dropping one op of a multi-op program is how you get a half-applied write —
// the reference agent's mirror describes exactly that failure, where dropping a slot orphaned a required
// operand and doomed the proposal at verify.)
func GateProgram(caps []Capability, program []ToolCall, rung AutonomyRung) (Plan, error) {
	for _, call := range program {
		if err := VerifyCall(caps, call); err != nil {
			return Plan{}, err
		}
	}

	var plan Plan
	for _, call := range program {
		spec, _ := lookupCapability(caps, call.Name) // verified above

		// ⭐ THE ZERO-TIER GUARD LIVES HERE, not only in ToolSet.Validate. Tier's zero value is
		// TierReadOnly, which AUTO-RUNS — so a forgotten `Tier:` field silently declares a destructive
		// act safe to run live. That guard was added to Validate first, and `grep '\.Validate()'`
		// across every consumer returns NOTHING: the check sat off the path that actually gates.
		//
		// A guard nobody calls is prose. This one is on the only entry point, so the kit's headline
		// promise — "an irreversible act can never auto-run" — holds for a struct-literal caller who
		// never heard of Validate. Read-only acts say so with TierReadOnlyExplicit.
		if spec.Tier == TierReadOnly {
			return Plan{}, fmt.Errorf("agentkit: capability %q declares no Tier — the zero value is "+
				"read-only, which AUTO-RUNS, so a forgotten field would make a destructive act live. "+
				"State it: TierReadOnlyExplicit / TierReversible / TierExternalReversible / "+
				"TierIrreversible", spec.Name)
		}
		// The tier's ceiling vs what the caller asked for. Live execution requires BOTH that the caller
		// permits it (rung) AND that the capability's own tier allows it (MaxRungFor). The floor wins.
		if rung == RungAuto && MaxRungFor(spec.Tier) == RungAuto {
			plan.Apply = append(plan.Apply, call)
			continue
		}
		plan.Propose = append(plan.Propose, call)
	}
	return plan, nil
}

func lookupCapability(caps []Capability, name string) (Capability, bool) {
	for _, c := range caps {
		if c.Name == name {
			return c, true
		}
	}
	return Capability{}, false
}

func lookupArg(c Capability, name string) (CapabilityArg, bool) {
	for _, a := range c.Args {
		if a.Name == name {
			return a, true
		}
	}
	return CapabilityArg{}, false
}

func contains(vals []string, v string) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}
