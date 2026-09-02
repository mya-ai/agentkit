package agentkit

import "github.com/google/uuid"

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ⭐ THE LABELER — the ONE thing an agent must supply for the kit to name what a proposal acts on.
//
//	"Given a kind and an id, what is that object called RIGHT NOW?"
//
// That is the whole seam. No ReviewContext, no itemContext, no backend client, no domain type — the kit
// never learns what a "project" is. A planning agent satisfies it by indexing the objects it ALREADY
// gathered; an ops agent satisfies it with a single entry (the one item it is working); a portfolio agent
// satisfies it from its own gather. None of them needs new machinery.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// Labeler resolves an object id to the name it has RIGHT NOW.
//
// ⚠️ NO context.Context, NO error, NO I/O — and that is deliberate. Resolution must be FREE: the agent's
// gather already fetched every object it is about to talk about (the kit's re-gather-fresh contract). A
// Labeler that did I/O would be an N+1 inside apply, and would make composing a sentence a failure path.
//
// ⭐ ok=false IS LOAD-BEARING, not an error case. It means "I do not know this id" — and the kit's response
// is a BEHAVIOR, not a log line: the id is never turned into a chip. A chip pointing at a ghost is worse
// than plain prose, because the user clicks it. (It is also the exact signature of a model that invented an
// id, which is why the honest answer is to drop it rather than render it.)
//
// ⚠️ THE METHOD IS `Resolve`, NOT `Label` — renamed because the name described the RETURN VALUE while
// the job is an EXISTENCE CHECK. Both call sites in the kit branch on `ok` and use `label` only as
// display text (proposal_label.go:63, proposal_question.go:113). A reader who thinks this "gets a
// label" misses that it can fail, and that failing is the point.
type Labeler interface {
	Resolve(kind, id string) (label string, ok bool)
}

// LabelerFunc adapts a plain function.
type LabelerFunc func(kind, id string) (string, bool)

// Resolve implements Labeler.
func (f LabelerFunc) Resolve(kind, id string) (string, bool) { return f(kind, id) }

// MapLabeler builds a Labeler from a kind → id → label map — the shape an agent already has after its
// gather (index the tasks once, the project once). Nil-safe: an absent kind or id is simply not known.
func MapLabeler(m map[string]map[string]string) Labeler {
	return LabelerFunc(func(kind, id string) (string, bool) {
		byID, ok := m[kind]
		if !ok {
			return "", false
		}
		label, ok := byID[id]
		return label, ok && label != ""
	})
}

// noLabels is the empty Labeler — knows nothing. Used when an agent passes nil.
var noLabels = LabelerFunc(func(string, string) (string, bool) { return "", false })

// Ref resolves an id into an ObjectRef carrying its LIVE label.
//
// An unresolvable id yields the ZERO ref — the caller drops it. This is the single most important line in
// the file: it is what guarantees an invented or stale id never becomes a chip.
func Ref(l Labeler, kind, id string) ObjectRef {
	if l == nil {
		l = noLabels
	}
	if kind == "" || id == "" {
		return ObjectRef{}
	}
	label, ok := l.Resolve(kind, id)
	if !ok {
		return ObjectRef{}
	}
	return ObjectRef{Kind: kind, ID: id, Label: label}
}

// RefToken renders an ObjectRef as a typed token — or as bare prose when there is nothing to point at.
// (Token already degrades an empty value to the fallback alone, so an unlabelled ref never emits `{{}}`.)
func RefToken(r ObjectRef) string { return Token(r.Kind, r.ID, r.Label) }

// storableRef reports whether a ref can safely be PERSISTED as a structured reference.
//
// ⚠️ THIS GUARD EXISTS BECAUSE A NON-UUID ID DESTROYS THE ENTIRE PROPOSAL — silently. The backend's
// reference table types the id column as UUID, and the reference write shares the TRANSACTION with the
// proposal row. So one bad id does not "skip a chip": it aborts the insert and the whole ask vanishes with
// no error the user or the agent will ever see.
//
// Verified against the live database:
//
//	INSERT ... ref_type='milestone', ref_id='m1'
//	⛔ ERROR: invalid input syntax for type uuid: "m1"   → the transaction rolls back → the proposal is gone
//
// And it is REACHABLE: the PM's milestone ids are model-supplied when present, so a model that writes "m1"
// would take out the proposal it was trying to make. This is the identical silent-drop class as the
// migration that widened the reference type CHECK — one column over.
//
// A ref that fails this is still usable for the SENTENCE (the token carries its own label); it just does
// not become a structured, clickable reference. Losing the chip is a scratch; losing the ask is fatal.
func storableRef(r ObjectRef) bool {
	if r.IsZero() {
		return false
	}
	return uuid.Validate(r.ID) == nil
}
