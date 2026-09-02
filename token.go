package agentkit

import (
	"regexp"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// TYPED TOKENS — the agent emits the machine FACT; the client renders the human LABEL.
//
//	{{kind:value|fallback}}     e.g.  {{task:3333…-…|Draft the launch checklist}}
//
// ## Why an agent should never type a name
//
// An agent that writes "I'll add your todo to the Platform Hardening project" has RE-TYPED something the system already
// knows — and can therefore get wrong: a typo, a stale name, or (with an LLM in the loop) a confabulation.
// It is also frozen: the sentence is authored once and the object gets renamed the next day.
//
// A token carries the ID. The client resolves it to the CURRENT name — a chip, a link, a localized label,
// whatever that surface wants. The agent cannot get a name wrong because it never writes one.
//
// ## ⭐ The `|fallback` is the safety property — do not omit it
//
// Not every reader parses tokens. A log line, a digest email, a durable insight card, the summarizer's own
// LLM prompt, an older client — all of these see the raw string. The fallback is what makes ONE string
// renderable EVERYWHERE:
//
//	token-aware client → resolves {{task:8396…}} → a live chip
//	everything else    → StripTokens → "I'll add Draft the launch checklist to Platform Hardening"
//
// So a token is never a bet on the reader being clever. It degrades to correct prose, always.
//
// ## Provenance
//
// Lifted from a planning agent's narration code, where it was invented for its live activity log (a
// workflow-state label is user-CONFIGURABLE per object, so the backend could not hardcode "Done"). It was
// documented in that system's API schema and its web client already parsed it. It is lifted here because a
// SECOND agent needed it — duplicating it there would be the bespoke-workaround failure the kit prevents.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// Token kinds the KIT knows about — the object nouns an agent's sentence can point at. These mirror the
// `ref_type` vocabulary a proposal-reference table CHECK-constrains, deliberately: one set of nouns
// across the prose and the structure.
//
// An agent may define its own kinds for DOMAIN values (a `taskState`/`projectStatus` kind is the agent's
// own — it names a user-configurable label, not an object). The kit only claims the object nouns.
const (
	TokenTask      = "task"      // value = the task/todo's UUID (a TODO *is* a task — there is no `todo` kind)
	TokenProject   = "project"   // value = the project's UUID
	TokenGoal      = "goal"      // value = the goal's UUID
	TokenSignal    = "signal"    // value = the signal/email insight's UUID
	TokenMilestone = "milestone" // value = the milestone's id (within a project's metadata)
)

// TokenRE matches `{{kind:value|fallback}}` and captures the FALLBACK as group 1.
//
// ⚠️ THIS IS A CONTRACT WITH THE FRONTEND, not a private detail. Its twin is the token regex in your web
// client's activity renderer. A string this accepts but the front end
// rejects renders as literal `{{…}}` braces to the user — a silent, ugly render bug. TokenMatchesFERegex
// (token_test.go) pins the two together; do not loosen one without the other.
//
// The FE's is `/\{\{[a-zA-Z][\w.]*:[^{}|]*\|([^{}]*)\}\}/g` — kind must START with a letter. We match that
// exactly rather than the looser original, so we can never emit a token the FE will not render.
var TokenRE = regexp.MustCompile(`\{\{[a-zA-Z][\w.]*:[^{}|]*\|([^{}]*)\}\}`)

// anyBracesRE matches ANY `{{…}}` run, including a malformed one TokenRE would not. Used to sweep the
// wreckage of a half-emitted token: `{{task:abc}}` (no fallback) matches nothing in TokenRE, so it would
// survive StripTokens and reach the user as raw braces. See StripAllBraces.
var anyBracesRE = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// tokenUnsafe are the characters a fallback may NOT contain: they are the token's own delimiters, so a
// fallback carrying one truncates the token and mangles the sentence. The FE says the same thing in its
// own words ("`label` may contain spaces/punctuation but not `{`, `}`, or `|`").
//
// This is NOT hypothetical — task titles are user-authored and arbitrary ("Fix the a|b parser").
var tokenUnsafe = strings.NewReplacer("{", "(", "}", ")", "|", "/")

// Token builds `{{kind:value|fallback}}`.
//
// The fallback is SANITIZED (see tokenUnsafe): a `|` or a brace inside it would break the token's own
// grammar and the FE would render a mangled fragment. Whitespace is collapsed so a title with a stray
// newline doesn't split the sentence.
//
// An EMPTY value or fallback returns the fallback alone (or ""): a token that points at nothing is worse
// than no token — the client would render a dead chip. Prose is the correct, safe degradation.
func Token(kind, value, fallback string) string {
	fallback = strings.Join(strings.Fields(tokenUnsafe.Replace(fallback)), " ")
	if strings.TrimSpace(kind) == "" || strings.TrimSpace(value) == "" {
		return fallback
	}
	return "{{" + kind + ":" + value + "|" + fallback + "}}"
}

// StripTokens replaces every well-formed token with its fallback text — the DUMB-READER path. A log line,
// a digest, a durable card, an LLM prompt: none of them parse tokens, and all of them must read clean
// prose. A string with no tokens is returned unchanged.
func StripTokens(s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return TokenRE.ReplaceAllString(s, "$1")
}

// StripAllBraces removes EVERY `{{…}}` run — well-formed tokens included.
//
// ⚠️ Call this only AFTER StripTokens, i.e. on a string that should no longer contain any token you want to
// keep. It is the last-ditch sweep for a reader that renders raw text: whatever braces survive here are
// wreckage. To sweep ONLY the malformed ones while preserving good tokens, use SweepMalformedTokens.
func StripAllBraces(s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return strings.TrimSpace(strings.Join(strings.Fields(anyBracesRE.ReplaceAllString(s, "")), " "))
}

// SweepMalformedTokens removes ONLY the `{{…}}` runs that are not valid tokens, leaving good ones intact.
//
// This is the sweep a token-carrying sentence needs. A model that half-emits a token (`{{task:abc}}` — no
// fallback) produces something NEITHER our regex NOR the frontend's will match, so it survives every other
// pass and reaches the user as raw template syntax — which is WORSE than the ambiguity we set out to fix.
// The fragment carries no readable text, so it is deleted rather than shown.
func SweepMalformedTokens(s string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	out := anyBracesRE.ReplaceAllStringFunc(s, func(m string) string {
		if TokenRE.MatchString(m) {
			return m // a valid token — keep it
		}
		return "" // wreckage
	})
	return strings.TrimSpace(strings.Join(strings.Fields(out), " "))
}

// TokenRef is one parsed token: what kind of object it points at, and which one.
type TokenRef struct {
	Kind     string // task | project | goal | signal | milestone | (an agent's own domain kind)
	Value    string // the id (or, for a domain kind, the enum value)
	Fallback string // the human text a dumb reader shows
}

// tokenPartsRE captures all three parts (kind, value, fallback) — TokenRE only captures the fallback.
var tokenPartsRE = regexp.MustCompile(`\{\{([a-zA-Z][\w.]*):([^{}|]*)\|([^{}]*)\}\}`)

// ParseTokens returns every well-formed token in the string, in order.
//
// This is what lets GO VALIDATE what a model wrote: an id that resolves to nothing must never reach the
// client (it would render a chip pointing at a ghost), so the agent parses, checks each id against the
// context it actually gathered, and strips the ones that don't exist. Guards live in code.
func ParseTokens(s string) []TokenRef {
	if !strings.Contains(s, "{{") {
		return nil
	}
	ms := tokenPartsRE.FindAllStringSubmatch(s, -1)
	out := make([]TokenRef, 0, len(ms))
	for _, m := range ms {
		out = append(out, TokenRef{Kind: m[1], Value: m[2], Fallback: m[3]})
	}
	return out
}

// HasTokenFor reports whether the string already names a specific object — used to decide whether a
// sentence still needs its anchor appended.
func HasTokenFor(s, kind, value string) bool {
	for _, t := range ParseTokens(s) {
		if t.Kind == kind && t.Value == value {
			return true
		}
	}
	return false
}
