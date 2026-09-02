package agentkit

import (
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ⭐ TOKENIZE — make the sentence NAME what it acts on, whoever wrote it.
//
// ## The bug this exists to kill
//
// Measured on a real deployment, the large majority of pending proposals could not be blessed:
//
//	"I'll add your existing todo 'Draft the launch checklist' to THIS PROJECT — approve."   ×4, each a different project
//	"I'll mark it complete — your task 'Review and patch vulnerabilities'."          ← on WHAT board?
//	"I'll mark this task done — approve."                                            ← WHICH task?
//
// Four identical Approve buttons, each acting on an invisible object. That is a blank cheque.
//
// ## Why a prompt cannot fix it
//
// The sentences come from BOTH sides. Some are hardcoded Go ("…to this project so it's tracked here"), with
// the project's real name sitting on the very struct the builder was handed — and discarded. Others are
// written by the model and passed through verbatim. A prompt cannot reach the first; Go alone cannot reach
// the second.
//
// ## The answer: GO REWRITES THE SENTENCE
//
// Go already knows everything: the agent's gather holds every id→name pair, and the op's args name the
// objects. So Go does not ASK the model to name things — it NAMES THEM ITSELF, after the fact, whatever the
// model wrote.
//
//	Prompt = quality.  Go = correctness.
//
// A prompt line ("name the task in quotes") makes the model's raw sentence better, which makes pass 2 hit
// more often. But it is not load-bearing: pass 3 GUARANTEES the object is named even if the model ignores
// the prompt entirely. That is why the model is never asked to emit tokens itself — a model-written token
// has four failure modes (typo'd id, invented id, name-only, half-token), each of which needs a Go repair,
// and once you have written the repair you have written the tokenizer.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// Deictics are the vague references a sentence uses INSTEAD of naming its object — "this project", "it".
// Pass 3 replaces them with the object's token.
//
// A CLOSED ALLOWLIST, not a regex over prose. We are rewriting a sentence a human will read, and a false
// positive is a mangled sentence. Every entry is a phrase observed in a real proposal — the Go-authored
// fallbacks and the model's output both produce them.
//
// ⭐ MATCHED BY KIND, which is what makes a two-object sentence work: in "I'll add X to this project",
// "this project" must resolve to the PROJECT, not to the task the sentence is anchored on. Without the
// kind, the first object eats the first deictic and the destination stays invisible — the live bug,
// half-fixed.
var kindDeictics = map[string][]string{
	TokenProject:   {"this project", "the project"},
	TokenTask:      {"this task", "this todo"},
	TokenGoal:      {"this goal"},
	TokenSignal:    {"this email", "this signal", "this thread"},
	TokenMilestone: {"this milestone"},
}

// genericDeictics refer to no kind in particular — tried only after the kind-specific ones.
//
// "it" is the riskiest and therefore last, matched only as a whole word. But it must be here: the live bug
// "I'll mark it complete — your task X" is exactly this case.
var genericDeictics = []string{"this item", "it"}

// TokenizeQuestion makes a question SELF-NAMING and CORRECT — whoever authored it.
//
// Four passes, each a distinct failure mode:
//
//  1. REPAIR     — a token the model emitted. Resolvable → its fallback is REWRITTEN to the LIVE label
//     (the model's may be stale or invented). Unresolvable → STRIPPED TO PROSE. A chip
//     pointing at a ghost is worse than no chip: the user clicks it.
//  2. SUBSTITUTE — a QUOTED-EXACT title ("Draft the launch checklist") or a bare id that matches a known object
//     becomes its token. This converts BOTH the Go-authored strings and the model's own
//     quoting, with no change to either author.
//  3. ANCHOR     — if the sentence STILL does not name its object: replace the first deictic ("this
//     project", "it") with the object's token; if there is none, APPEND it. Ugly-but-correct
//     beats ambiguous. ⭐ THIS IS THE GUARANTEE — every proposal names its object, always.
//  4. SWEEP      — remove any malformed `{{…}}` wreckage. A half-emitted token would reach the user as raw
//     braces, which is WORSE than the ambiguity we set out to fix.
//
// `also` are the other objects the sentence may name (a move's destination). Only the anchor and `also` are
// substitution candidates — never the whole Labeler, or a task called "Draft" would carpet-bomb any sentence
// containing the word.
//
// Idempotent: running it twice changes nothing.
func TokenizeQuestion(l Labeler, q string, anchor ObjectRef, also ...ObjectRef) string {
	if l == nil {
		l = noLabels
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}

	known := append([]ObjectRef{anchor}, also...)

	q = repairTokens(l, q)
	q = substituteNames(q, known)
	q = anchorQuestion(q, anchor, also...)
	// Sweep ONLY the wreckage — a good token must survive (StripAllBraces would delete every token,
	// including the ones we just carefully built).
	return SweepMalformedTokens(q)
}

// repairTokens re-resolves every token the model emitted against the live world (pass 1).
func repairTokens(l Labeler, q string) string {
	for _, t := range ParseTokens(q) {
		old := Token(t.Kind, t.Value, t.Fallback)

		label, ok := l.Resolve(t.Kind, t.Value)
		if !ok {
			// The id resolves to NOTHING. Strip the token to its prose — never render a chip to a ghost.
			// (This is also the signature of a model that invented an id, and dropping it is the honest
			// response: we keep the words, we refuse the false link.)
			q = strings.ReplaceAll(q, old, t.Fallback)
			continue
		}
		// The id is real — but the model's FALLBACK may be stale or wrong. Go's label is authoritative.
		q = strings.ReplaceAll(q, old, Token(t.Kind, t.Value, label))
	}
	return q
}

// substituteNames turns a quoted-exact title, or a bare id, into its token (pass 2).
//
// QUOTED-ONLY and EXACT-ONLY, deliberately. Unquoted substring matching would carpet-bomb the sentence: a
// task called "Ship it" would tokenize the word "it" in "I'll mark it complete". Quoting is the signal that
// the author meant THE OBJECT and not the words.
//
// Longest label first, so a title that is a prefix of another cannot win.
func substituteNames(q string, known []ObjectRef) string {
	refs := make([]ObjectRef, 0, len(known))
	for _, r := range known {
		if !r.IsZero() && r.Label != "" {
			refs = append(refs, r)
		}
	}
	sort.SliceStable(refs, func(i, j int) bool { return len(refs[i].Label) > len(refs[j].Label) })

	for _, r := range refs {
		if HasTokenFor(q, r.Kind, r.ID) {
			continue // already named (idempotence)
		}
		tok := RefToken(r)

		// The quoting styles a Go builder or a model actually produces.
		for _, quoted := range []string{
			`"` + r.Label + `"`,
			`'` + r.Label + `'`,
			`“` + r.Label + `”`,
			`‘` + r.Label + `’`,
		} {
			if strings.Contains(q, quoted) {
				q = strings.Replace(q, quoted, tok, 1)
				break
			}
		}
		// A bare id the model pasted into its prose — turn a leak into a feature.
		if !HasTokenFor(q, r.Kind, r.ID) && strings.Contains(q, r.ID) {
			q = strings.Replace(q, r.ID, tok, 1)
		}
	}
	return q
}

// anchorQuestion guarantees the sentence names every object it is about (pass 3).
//
// ⭐ THIS IS THE INVARIANT. Passes 1 and 2 improve a sentence; this one makes it CORRECT. After it, a
// proposal cannot be ambiguous — not because the model cooperated, but because Go insisted.
//
// It names the ANCHOR (what the ops act on) AND the other known objects (a move's destination). Both
// matter: "I'll add Draft the launch checklist to THIS PROJECT" still fails the user — naming the task but not the
// destination is exactly half a sentence, and the four indistinguishable asks all named their task.
//
// A deictic is matched to the object it plainly refers to ("this project" → the project), so a sentence
// with two vague references resolves both correctly.
func anchorQuestion(q string, anchor ObjectRef, also ...ObjectRef) string {
	for _, r := range append([]ObjectRef{anchor}, also...) {
		q = nameObject(q, r)
	}
	return q
}

// nameObject ensures ONE object is named in the sentence: replace the deictic that refers to it, else
// append. A no-op if it is already named (idempotence).
func nameObject(q string, r ObjectRef) string {
	if r.IsZero() || r.Label == "" || HasTokenFor(q, r.Kind, r.ID) {
		return q
	}
	tok := RefToken(r)

	lower := strings.ToLower(q)
	for _, d := range deicticsFor(r.Kind) {
		if i := indexWord(lower, d); i >= 0 {
			return q[:i] + tok + q[i+len(d):]
		}
	}

	// No deictic refers to it, and it still is not named — say what it is about. Ugly beats ambiguous.
	return strings.TrimRight(q, " ") + " (" + tok + ")"
}

// deicticsFor returns the vague phrases that plainly refer to THIS kind of object, most specific first,
// then the kind-agnostic ones.
//
// Matching by kind is what lets "I'll add X to this project" resolve "this project" to the PROJECT rather
// than to the task the sentence is anchored on. Without it, the first object would eat the first deictic
// and the destination would stay invisible — which is the live bug, half-fixed.
func deicticsFor(kind string) []string {
	specific := kindDeictics[kind]
	return append(append([]string{}, specific...), genericDeictics...)
}

// indexWord finds `word` on whole-word boundaries — so "it" does not match inside "item" or "with".
func indexWord(hay, word string) int {
	from := 0
	for {
		i := strings.Index(hay[from:], word)
		if i < 0 {
			return -1
		}
		i += from
		if isWordBoundary(hay, i, len(word)) {
			return i
		}
		from = i + 1
	}
}

func isWordBoundary(s string, i, n int) bool {
	if i > 0 && isWordChar(s[i-1]) {
		return false
	}
	if end := i + n; end < len(s) && isWordChar(s[end]) {
		return false
	}
	return true
}

func isWordChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// Build is THE choke point. Every proposal from every agent passes through it, so no agent CAN ship an ask
// that does not name what it acts on.
//
// It resolves every id to its LIVE label (the agent never writes a name) and tokenizes the question against
// those labels.
//
// It does NOT assemble the backend request — the dedupe key (DedupeKey), the archetype (DeriveArchetype) and
// the storable refs (StorableRefs) are separate, exported, and applied by the agent's own backend mapper,
// where the rest of the request is built. Build owns exactly one thing: THE SENTENCE NAMES ITS OBJECT.
func Build(l Labeler, p Proposal) Proposal {
	if l == nil {
		l = noLabels
	}

	// Resolve the labels Go knows and the model does not get to write.
	p.Object = resolveRef(l, p.Object)
	p.Context = resolveRef(l, p.Context)
	objs := make([]ObjectRef, 0, len(p.Objects))
	for _, r := range p.Objects {
		if rr := resolveRef(l, r); !rr.IsZero() {
			objs = append(objs, rr)
		}
	}
	p.Objects = objs

	p.Question = TokenizeQuestion(l, p.Question, p.Object, p.Objects...)
	return p
}

// resolveRef fills in a ref's LIVE label. A ref whose id the agent cannot resolve is dropped: it would
// otherwise become a chip pointing at nothing.
func resolveRef(l Labeler, r ObjectRef) ObjectRef {
	if r.IsZero() {
		return ObjectRef{}
	}
	return Ref(l, r.Kind, r.ID)
}

// StorableRefs returns the refs an agent may safely PERSIST as structured references — dropping any whose
// id would abort the write. See storableRef: a non-UUID id does not skip a chip, it takes out the whole
// proposal, silently.
//
// The sentence is unaffected: a dropped ref's token still carries its own label.
func StorableRefs(rs ...ObjectRef) []ObjectRef {
	out := make([]ObjectRef, 0, len(rs))
	for _, r := range rs {
		if storableRef(r) {
			out = append(out, r)
		}
	}
	return out
}
