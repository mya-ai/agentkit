# Design discipline

This page answers one question: **what makes a change to agentkit's core acceptable?**
[CONTRIBUTING.md](../CONTRIBUTING.md) at the repo root covers the mechanics — running the checks,
commit style, licensing. This is the part that decides whether a design is right, and it is blunt on
purpose. Every rule below is a failure that actually shipped.

## The standing question

> **Does this change make it easier or harder for a developer who does not work on this framework to
> add or change a capability?**

Harder is a blocked merge, however elegant the justification. The most common job a developer has is
adding a tool, and that path *is* the product. A change that makes the internals tidier and the
tool-adding path longer has made the framework worse.

Judge every proposed addition the same way: *should agentkit provide this?* Usage is the prompt to
ask the question, never the answer. A symbol with no consumer may be missing a consumer, not missing
a purpose. Four verdicts are available — keep and document, cut the duplicate, evict it to the
consumer, or abandon it and record why so nobody re-derives it.

## No bespoke workarounds

An agent built on the kit may contain its **own identity** and nothing more. It may not contain a
local re-solving of a problem the framework should solve. The test:

> Is this solving an **agent-specific** problem, or **re-solving a framework problem locally**?

| ALLOWED — agent identity, bespoke is correct | BLOCKED — bespoke machinery, lift it into the kit |
|---|---|
| gather, prompts, doctrine | a hand-rolled loop, retry, or throttle handler |
| the verdict / output schema | a private structured-call or text-call wrapper |
| domain apply rules, business rules | a copied proposal builder |
| intents, op semantics | a local dedupe, single-writer, or lease hack |
| the agent's own eval cases | a duplicated tool dispatch or schema-from-handler |
| | a local "is this JSON well-formed / non-empty" guard |

If two agents would each need it, it is framework. The fix for a workaround is to lift the machinery
into the kit with the agent as its first consumer — never to merge the workaround and clean it up
later. Without this rule every agent accretes a private copy of the loop, and the framework stops
being one.

⛔ This cuts both ways. Do not pull a consumer's *domain* into the kit either. The kit never learns
what a "project" or an "opcode" is; if a proposed helper needs that knowledge, it belongs in the
agent permanently.

## The review standard

Assume the developer reading your code is a coding agent: about fifty lines of context, no memory of
the last session, orients by grep, works by adapting the nearest similar block. It does not read a
file top to bottom, and it will confidently write the mistake a human would catch by feel.

⛔ **A comment saying "keep these in sync" is a FAILED design, not a mitigation.**

| Ask of every feature | Failure looks like |
|---|---|
| Can it be used from one contiguous block? | the declaration is split across files |
| Is a wrong edit caught by the compiler or a test? | only a comment, or nothing |
| Does one grep answer "what can this agent do?" | you must cross-reference two files |
| Do the defaults fail **closed**? | a forgotten field means "run it live" |
| Is the failure **loud**? | verify passes, the gate passes, the call silently vanishes |
| Does every name state its contract? | you have to guess whether an error aborts the run |

The kit's own `Tier` is the worked example. Its zero value is `TierReadOnly`, which auto-runs — so a
forgotten `Tier:` field would silently declare a destructive act safe to execute live. Renumbering
was not available (consumers pin the int values against their own registries), so the guard moved to
declaration time: `GateProgram` rejects a bare zero tier outright, and `TierReadOnlyExplicit` exists
so that "this may auto-run" is always a statement and never an omission.

Note *where* that guard sits. It was first added to `ToolSet.Validate` — and a grep across every
consumer for `.Validate()` returned nothing. The check sat off the path that actually gates. A guard
nobody calls is prose; it belongs on the entry point everyone reaches.

### How to review a change

1. **Land cold.** "If I arrived with no memory, what would I open, and what would I do?" Then do it.
2. **Probe, don't read.** Run it. Feed it the newcomer's mistake — a forgotten field, an empty set, a
   duplicate name — and watch what happens.
3. **Count.** Edit sites, declaration sites, dependencies, symbols with no consumer. Numbers settle
   arguments that adjectives do not.
4. **Verify every claim, including your own.** Grep or run before writing it down.
5. **State the failure mode** in one mechanical sentence.
6. **Prove the fix**: a test that fails first, or an existing consumer that gets smaller.

⭐ **A finding is not real until a command demonstrates it.**

## The two ways a design goes wrong

Six design drafts were retracted while building the dialogue feature. Every one failed in one of two
ways, and the second is the subtler.

**1. Shape → model.** Reasoning off the plumbing and letting it define the concept. `Plan.Propose` is
a `[]ToolCall`, so a draft concluded "a proposal is just a withheld capability" — which makes it
impossible for an agent to *ask* anything, because a question has no act to withhold. The data
structure was mistaken for the idea.

**2. Legacy → framework.** Citing an existing workaround as evidence *for* a design. A workaround is
what the framework exists to replace; quoting it as proof reasons in a circle.

⛔ State what the thing **is** from first principles, then check the code against it — never the
reverse. Agents built on the kit are illustrations, never premises.

This is not only a design-time error. The same reversal produced three shipped defects on one branch:
a consumer's in-code comment ("this forces X") was quoted as evidence for a redesign and was simply
false; a `grep -v _test` produced a "dead guard" finding about code that was live; and a
*prescriptive* rule was read as *descriptive*, so a live violation was argued to be "not debt". In
each case an artifact of how things are was promoted to evidence for how they should be.

⭐ **The check that catches all three: before promoting a finding to a root cause, run the command
that would DISPROVE it.** Ask for falsification, not confirmation.

## The dependency budget

Importing agentkit costs **one** external dependency (`github.com/google/uuid`, for id generation).
`scripts/check-deps.sh` caps it at five in CI (it sits at one) and hard-fails on a banned import: it lists every non-stdlib package reachable from the
module, fails on a budget overrun, and hard-fails on a banned import — a cloud SDK, a gRPC stack, a
metrics stack, a host application's packages.

The number is what the extraction bought. Before it, `go list -deps` on this code reported **537
packages, 341 of them non-stdlib** (95 from an AWS SDK, 60 from gRPC). Three imports caused nearly
all of it, and the largest — an in-house LLM package, roughly 513 of the 537 — was **structural, not
sloppiness**. `RunOnce` *is* a structured LLM call; a loop primitive that cannot call a model is not a
loop primitive. The fix was to invert the seam, not delete the feature: the kit declares
`StructuredRequest` and `StructuredCaller`, and the consumer implements them. See
[providers.md](providers.md).

The other half was a genuine misplacement. A queue handler was plumbing living in a framework; it was
evicted, not moved. The kit ships no queue integration, no handler, no fan-out. An enqueue, a cohort
lister, or a message-envelope type in the core is a blocked merge, because anything that dispatches
work has to import infrastructure and the kit must not.

⛔ **Do not weaken `check-deps.sh` to land a change.** Adding a dependency is a design decision: raise
the budget deliberately with a stated reason, or put the dependency behind an interface the consumer
implements. Silently raising the number to make CI green is exactly the failure the script exists to
catch.

## Documentation lands with the code

A wrong doc is worse than none. Two guards enforce this rather than trusting review:
`readme_anchors_test.go` checks that every `file.go:NN` anchor in
[reference.md](reference.md) lands on the line declaring the symbol its row names, and
`readme_identity_test.go` checks the documented signatures against source. They run under `go test`.

That guard was written after five of eight anchors in another doc had already drifted — one by 88
lines, each landing on an unrelated comment. An earlier version only checked the line's *shape*
("does this look like a declaration?"), and a comment line looks exactly like a legitimate doc-comment
anchor, so it passed all five. Only asserting that the line declares the **named symbol** fails them.

If you move code and an anchor goes stale, fix the anchor. Do not delete the guard, and do not let it
skip on a missing file — a drift guard that quietly stops guarding is the worst of both.

Do not describe a planned symbol as if it exists. The BUILT / PARTIAL / PLANNED labels in the
reference are load-bearing. Honest statements about what is *not* built are a feature of these docs,
not an embarrassment — see [design/dialogue.md](design/dialogue.md) for a worked example.
