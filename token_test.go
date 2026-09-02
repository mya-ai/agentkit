package agentkit

import (
	"regexp"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// THE TOKEN CONTRACT.
//
// A typed token is a promise to TWO readers at once: the token-aware client (which resolves the id to a
// live chip) and every dumb reader (a log, a digest, an email, an LLM prompt, an older client — which
// must still see clean prose). Both halves are tested here, because breaking either is silent:
//
//   - break the FE's parse → the user sees literal `{{task:8396…|Prep the deck}}` braces
//   - break the fallback   → a log line reads `{{task:…}}` and an LLM prompt is fed template syntax
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// ⭐ THE WIRE CONTRACT WITH THE FRONTEND.
//
// TokenRE has a twin wherever your UI renders these tokens. If your frontend parses them, the two
// regexes MUST agree — this test pins the Go side against the shape a frontend would use:
//
//	const TYPED_TOKEN = /\{\{[a-zA-Z][\w.]*:[^{}|]*\|([^{}]*)\}\}/g;
//
// A token WE emit and THEY reject renders as raw braces to the user — silent, and ugly. So this test pins
// the two regexes together on the shapes that matter. If you change one, this fails until you change both.
func TestTokenRE_MatchesTheFrontendRegex(t *testing.T) {
	// The FE's regex, transcribed. Go's RE2 syntax is compatible for this pattern.
	feRE := regexp.MustCompile(`\{\{[a-zA-Z][\w.]*:[^{}|]*\|([^{}]*)\}\}`)

	cases := []struct {
		name  string
		s     string
		match bool
	}{
		{"a uuid-valued task token", "{{task:33333333-4444-4444-4444-555555555555|Draft the launch checklist}}", true},
		{"a project token", "{{project:66666666-7777-7777-7777-888888888888|Platform Hardening}}", true},
		{"a domain-kind token (the PM's)", "{{taskState:completed|Done}}", true},
		{"a dotted kind", "{{pm.taskState:started|In progress}}", true},
		{"punctuation in the fallback", "{{task:abc|Re: Dana's reply — the cutover?}}", true},

		{"NO fallback → both must reject", "{{task:abc}}", false},
		{"kind starting with a digit → both must reject", "{{1task:abc|X}}", false},
		{"empty braces", "{{}}", false},

		// ⚠️ An EXTRA pipe does NOT reject — both regexes match, and the value/fallback split lands on the
		// FIRST pipe: {{task:a|b|X}} parses as value="a", fallback="b|X". They AGREE, which is all this test
		// asserts — but it is exactly why Token() SANITIZES the fallback (TestToken_SanitizesTheFallback):
		// an unsanitized `|` in a title silently changes what the fallback IS, rather than failing loudly.
		{"an extra pipe still matches (and both agree)", "{{task:a|b|X}}", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, want := TokenRE.MatchString(tc.s), feRE.MatchString(tc.s)
			if got != want {
				t.Fatalf("⛔ Go/FE REGEX DRIFT on %q: Go=%v FE=%v.\n"+
					"   A token Go emits and the FE rejects renders as literal {{…}} braces to the user — "+
					"silent, and ugly. A token the FE accepts and Go can't parse escapes our guards.\n"+
					"   The twin is the token regex in your own frontend renderer.", tc.s, got, want)
			}
			if got != tc.match {
				t.Errorf("match = %v, want %v", got, tc.match)
			}
		})
	}
}

// ⭐ THE FALLBACK SANITIZER — a real, live risk, not a hypothetical.
//
// Task titles are USER-AUTHORED and arbitrary. A `|` or a brace inside the fallback truncates the token's
// own grammar, and the FE renders a mangled fragment. The FE's own comment says the same: "`label` may
// contain spaces/punctuation but not `{`, `}`, or `|`."
func TestToken_SanitizesTheFallback(t *testing.T) {
	got := Token(TokenTask, "id-1", "Fix the a|b {parser}")

	for _, bad := range []string{"|", "{", "}"} {
		// The token's OWN delimiters are legal; what must not survive is the char inside the fallback.
		fb := strings.TrimSuffix(strings.SplitN(got, "|", 2)[1], "}}")
		if strings.Contains(fb, bad) {
			t.Errorf("⛔ the fallback still contains %q: %q\n"+
				"   It would truncate the token and the FE would render a mangled fragment. Titles are "+
				"user-authored — 'Fix the a|b parser' exists in the wild.", bad, fb)
		}
	}
	if !TokenRE.MatchString(got) {
		t.Errorf("the sanitized token no longer parses: %q", got)
	}
	if !strings.Contains(got, "Fix the a/b (parser)") {
		t.Errorf("the fallback lost its meaning: %q — sanitizing must SUBSTITUTE, not delete", got)
	}
}

// A token that points at nothing is worse than no token: the client renders a dead chip. Degrade to prose.
func TestToken_EmptyValueDegradesToProse(t *testing.T) {
	if got := Token(TokenTask, "", "Draft the launch checklist"); got != "Draft the launch checklist" {
		t.Errorf("Token with no value = %q, want the bare fallback — a token pointing at nothing would "+
			"render as a chip to a ghost", got)
	}
	if got := Token("", "id-1", "X"); got != "X" {
		t.Errorf("Token with no kind = %q, want the bare fallback", got)
	}
}

// The DUMB-READER path: a log, a digest, an email, an LLM prompt.
func TestStripTokens_ReadsAsCleanProse(t *testing.T) {
	s := "I'll add " + Token(TokenTask, "t1", "Draft the launch checklist") +
		" to " + Token(TokenProject, "p1", "Platform Hardening") + " — approve."

	got := StripTokens(s)
	want := "I'll add Draft the launch checklist to Platform Hardening — approve."
	if got != want {
		t.Errorf("StripTokens = %q,\n              want %q\n"+
			"  This is the whole safety property: ONE string, renderable by a token-aware client AND by "+
			"every reader that isn't.", got, want)
	}
	if strings.Contains(got, "{{") {
		t.Error("raw template syntax survived into the dumb-reader path")
	}
}

func TestStripTokens_NoTokensIsUnchanged(t *testing.T) {
	const s = "I'll mark this done — approve."
	if got := StripTokens(s); got != s {
		t.Errorf("a token-free string was altered: %q", got)
	}
}

// ⭐ A MALFORMED token must never reach a human.
//
// `{{task:abc}}` (no fallback) matches NEITHER our TokenRE nor the FE's — so it survives StripTokens and
// renders as raw braces. This is the wreckage of a model that half-complied, and it is a WORSE bug than the
// plain-prose ambiguity we set out to fix.
func TestStripAllBraces_SweepsMalformedTokens(t *testing.T) {
	got := StripAllBraces("I'll mark {{task:abc}} complete — approve.")
	if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
		t.Errorf("⛔ raw braces survived: %q — the user would see template syntax", got)
	}
	if !strings.Contains(got, "I'll mark") || !strings.Contains(got, "complete") {
		t.Errorf("the sentence was destroyed: %q — sweep the fragment, keep the prose", got)
	}
}

// ParseTokens is what lets GO VALIDATE what a model wrote (an id that resolves to nothing must be stripped
// before it reaches a client, or it renders a chip pointing at a ghost).
func TestParseTokens(t *testing.T) {
	s := "I'll add " + Token(TokenTask, "t1", "Prep the deck") +
		" to " + Token(TokenProject, "p1", "Platform Hardening") + "."

	got := ParseTokens(s)
	if len(got) != 2 {
		t.Fatalf("parsed %d tokens, want 2: %+v", len(got), got)
	}
	if got[0].Kind != TokenTask || got[0].Value != "t1" || got[0].Fallback != "Prep the deck" {
		t.Errorf("token[0] = %+v", got[0])
	}
	if got[1].Kind != TokenProject || got[1].Value != "p1" {
		t.Errorf("token[1] = %+v", got[1])
	}

	if !HasTokenFor(s, TokenTask, "t1") {
		t.Error("HasTokenFor missed a token that is present")
	}
	if HasTokenFor(s, TokenTask, "nope") {
		t.Error("HasTokenFor matched a token that is absent")
	}
}
