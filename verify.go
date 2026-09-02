package agentkit

import "context"

// This file implements "verification is first-class, strength-ordered execution > rules >
// LLM-judge." The kit ships the COMPOSITION (a chain that runs cheaper, more-grounded verifiers FIRST and
// only reaches the LLM critic if they pass) + a ready RulesFunc adapter, so an agent can put a deterministic
// pre-check ahead of its LLM judge instead of every agent hand-rolling one (which would be the bespoke-
// workaround anti-pattern). Motivated by a real eval's `did not parse` (a truncated verdict) — a rules
// check (well-formed + has ≥1 actionable element) catches that deterministically, cheaply, before burning
// an LLM reviewer call.

// RulesFunc is a deterministic verifier body: pure function of the result, no LLM/clock/RNG. It returns ok
// + addressable feedback (empty when ok). It is the "rules" tier of the strength ladder. Wrap it with
// RulesVerifier to get a Verifier[V].
type RulesFunc[V any] func(v V) (ok bool, feedback []string)

// rulesVerifier adapts a RulesFunc to Verifier[V] (it ignores ctx — rules are pure/synchronous).
type rulesVerifier[V any] struct{ fn RulesFunc[V] }

func (r rulesVerifier[V]) Verify(_ context.Context, v V) (bool, []string, error) {
	ok, fb := r.fn(v)
	return ok, fb, nil
}

// RulesVerifier wraps a deterministic RulesFunc as a Verifier[V]. Prefer this (and execution-based
// verifiers) over an LLM critic where the property is checkable — an ungrounded LLM critic can DEGRADE
// results (Huang 2024 / Kamoi 2024), while a rules check is free, deterministic, and never wrong about a
// structural fact (e.g. "the verdict is well-formed and has at least one action").
func RulesVerifier[V any](fn RulesFunc[V]) Verifier[V] { return rulesVerifier[V]{fn: fn} }

// VerifierChain runs verifiers in order and SHORT-CIRCUITS on the first that rejects (returns its
// feedback) or errors — so the strength order is the call order: put the cheapest, most-grounded
// verifiers first (rules, then execution, then an LLM critic LAST). The result is approved only if ALL
// pass. This is the strength-ordering made real: a cheap rules reject never pays for the LLM judge, and
// a structurally-broken result (the eval's truncated verdict) is caught before the critic ever runs.
//
// An empty chain approves (no verifier = nothing to object) — callers that want "must be verified" pass at
// least one verifier. nil entries are skipped.
func VerifierChain[V any](verifiers ...Verifier[V]) Verifier[V] {
	return chain[V]{verifiers: verifiers}
}

type chain[V any] struct{ verifiers []Verifier[V] }

func (c chain[V]) Verify(ctx context.Context, v V) (bool, []string, error) {
	for _, ver := range c.verifiers {
		if ver == nil {
			continue
		}
		ok, fb, err := ver.Verify(ctx, v)
		if err != nil {
			return false, nil, err // an errored verifier is terminal (e.g. an LLM judge throttle)
		}
		if !ok {
			return false, fb, nil // first rejection wins; cheaper verifiers gate the expensive ones
		}
	}
	return true, nil, nil
}
