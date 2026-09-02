# Architecture

Why agentkit is shaped the way it is: the design principles behind the loops, the tier gate and
the actor model, the alternatives that were rejected, and an honest account of what is built and
what is not.

For signatures and traps, see [reference.md](../reference.md). For the order of work when building
an agent, see [building-an-agent.md](../guides/building-an-agent.md). Two mechanisms have their own
pages: [the agent runtime](agent-runtime.md) and [proposals](proposals.md).

## What the library is

agentkit is a thin Go runtime for **autonomous** agents — the cron- or event-triggered shape that
gathers context, reasons in a budgeted loop, and either acts within a mandate or proposes a change
for a human to bless. It is not a chat framework: there is no transcript, no streaming, and a run is
deliberately stateless.

The kit owns the agent-generic machinery — the loop, the round budget, the degeneration guard, the
throttle contract, capability rendering and verification, the tier gate, the actor contracts. An
agent supplies only its identity: its output schema, its context gather, its prompts, its tools, its
write rules.

**Go owns the write.** Every loop returns a decision. Nothing in this package persists domain state.

## The design principles

These are named rather than numbered, because each stands on its own argument.

### One loop primitive, two transports, compositions on top

Single-call, ReAct, reflection pairs, plan-execute, tree-of-thought and debate all reduce to "an LLM
call inside a tool-driven loop". Planning, reflection and sub-agents are tools or nested loops
*inside* the one loop, not separate runtimes.

The atom is *render → one LLM call → observe*. It exists in two transport flavours because the
result types genuinely differ: `RunOnce[Ctx, V]` returns a typed `V` through the structured caller,
`RunOnceText[Ctx]` returns a string through the text caller. Collapsing them into one generic would
erase the structured path's compile-time typing — and forcing prose through JSON mode wraps a
markdown digest in a `{"description":"..."}` envelope, which is a bug that actually shipped in local
testing. So the text flavour is necessary, not a convenience.

What keeps this one primitive rather than two copy-pasted loops is structural: each public wrapper is
a thin shell over a shared core (`runOnce` / `runOnceText`) honouring the same `RoleSpec`, `Observer`
and throttle discipline, and the loop compositions build on the atom. `Pair` is `RunOnce` plus a
`Verifier`. `RunToolLoop` is `RunOnce` plus tool dispatch plus result-carry. `RunToolLoopTiered`
is `RunToolLoop` with a tier hook — and it shares one loop body (`runToolLoopCore`) rather than
copying it, because a duplicated loop inside the kit is the same anti-pattern the kit forbids in its
consumers. A single-shot query step is `RunToolLoop` with `MaxRounds: 1`, the degenerate case.

### Agent-controlled termination inside a code-enforced ceiling

The agent reasons about and declares its terminal state; Go validates and enforces it. The model
never writes — `end_turn` is never the write decision.

Three terminal states the model can declare: a material verdict (the judge gates it, then the
consumer applies), nothing-material (`IsMaterial() == false` → `OutcomeSilent`, write nothing), and
"I need the user", which is a **terminal state, not a pause** — the run ends and the user's answer
re-triggers a fresh run. An autonomous worker cannot block on a human.

Code backstops all three: a hard round ceiling in `LoopConfig.MaxRounds`, and a degeneration guard
(`SameFunc`) that catches an executor resubmitting materially the same result despite specific
feedback. The ceiling is a **safety ceiling, not a target** — the loop exits on the first judge
approval, not after N rounds.

The budget is fed *into* the prompt rather than merely enforced around it. `RoundInfo{Round,
MaxRounds, Feedback}` reaches `Render`, and `RoundInfo.IsFinalRound()` lets an executor commit its
best complete result on the last round instead of being truncated mid-revision.

`DefaultMaxRounds` is 2 — a draft plus one critique-driven revision. An unset `LoopConfig{}` gets 2,
not 1, because a one-round `Pair` is degenerate: the judge rejects the first draft and the loop
immediately records a stalemate, so no revision ever happens. That was a real footgun for an author
who left the field unset.

### The executor–judge pair is an option, gated on whether the judge adds signal

*Ungrounded same-model self-critique can degrade results* — this is the load-bearing research
finding, from Huang et al., *Large Language Models Cannot Self-Correct Reasoning Yet*
([arXiv:2310.01798](https://arxiv.org/abs/2310.01798), ICLR 2024), corroborated by Kamoi et al.,
*When Can LLMs Actually Correct Their Own Mistakes?*
([arXiv:2406.01297](https://arxiv.org/abs/2406.01297), TACL 2024). A critic helps only when it has
signal the generator lacked.

So the judge is a pluggable `Verifier[V]`, not a hardcoded reviewer. A planning agent passes an LLM
critic; a coding agent passes a test runner; a deterministic agent passes a rules check — the same
`Pair`, the right signal each time. Verification strength runs execution-based > rules/deterministic
> LLM judge, and an LLM judge should never be the sole gate on an irreversible act. A single-call or
tool-loop agent is not forced to pay for a judge at all.

A tool loop's reflection is a *different* and acceptable case: the executor's next round sees the
prior tool **results** and decides done-or-reformulate. That reflection is grounded in retrieved
data, so the self-critique caveat does not bite. `ToolRoundInfo.PriorResults` is what carries it.

### The loop ends at the executor, never at the judge

A round is `executor → judge → executor`. The loop is executor-first, then `(judge → executor)`
pairs up to the budget, so it always both starts and ends with an executor turn. The judge's only
power is to gate: approve (stop) or reject (buy one more executor turn, within budget). It can never
be the last word, and the kit never ships, surfaces or persists the judge's text.

This corrected a real bug. The loop used to end on a judge rejection and ship the judge's critique as
the result — leaking the reviewer's internal voice ("the takeaway incorrectly claims…") into a
user-facing card, and stranding the proposal the judge had suggested. `Result` has deliberately no
field to hold judge text: a `StalemateFeedback` slot existed, was never assigned by anything, and was
removed rather than left as an always-nil promise. `OutcomeStalemate` is now the degeneration case
only — the executor stuck resubmitting despite the critique — and even that ships the executor's
output.

On the final permitted round the loop returns without running a judge pass at all. A rejection there
could only be surfaced as judge text (forbidden) or ignored (pointless).

This is a **mechanism** guarantee. What the final executor turn should *say* when the judge keeps
rejecting — reconcile, or "this is the user's call, ask them" — is policy in the executor's prompt,
which reads `RoundInfo.Feedback` and `IsFinalRound()`. The kit encodes none of it.

One asymmetry worth knowing: a judge **error** discards the executor's result rather than applying
it. It was never verified, and the kit never ships an unverified result. On a judge throttle that is
exactly right — release to the queue and the whole run repeats cheaply. On another judge failure it
costs one wasted executor call on redelivery, which is the correct price for not violating "the judge
gates done".

### Safety is structural, in trusted code, never in the prompt

A prompt is a request; a type is a guarantee. Every capability carries a reversibility `Tier`:

| Tier | Meaning | Autonomy |
|---|---|---|
| `TierReadOnlyExplicit` | queries and lookups, no mutation | may run live |
| `TierReversible` | mutation with a real compensating undo | may run live |
| `TierExternalReversible` | an external effect recoverable but not silently (send, book) | propose only |
| `TierIrreversible` | delete, mass-comms, payments, permissions | propose only |

`MaxRungFor` is the one piece of autonomy policy the kit commits to, and it is a floor: live
execution requires both that the caller asked for it (`RungAuto`) and that the capability's own tier
permits it. `GateProgram` splits a model-emitted program into `Plan.Apply` and `Plan.Propose`, and a
call in `Propose` has not run and *cannot* run from there. `RunToolLoopTiered` does the same inside
the loop: an above-ceiling call is captured as a `ProposedAction` and the loop stops without ever
invoking the tool.

**The tier's zero value is a rejection, not a default.** `TierReadOnly` is `iota == 0` — which
auto-runs — so a forgotten `Tier:` field would silently declare a destructive act safe to run live.
Renumbering cannot fix it: consumers pin these int values position-for-position against their own
backend's reversibility enum, and shifting them would mis-tier every op in that registry. So the
guard is at declaration time instead: `GateProgram` **rejects a bare zero tier outright**, and
`TierReadOnlyExplicit` (a negative sentinel, sorting below every real tier) is how a genuinely
read-only capability says so. "This may auto-run" must be a statement, never an omission.

That guard lives on `GateProgram` and not only on `ToolSet.Validate` for a reason worth repeating: it
was added to `Validate` first, and `grep '\.Validate()'` across every consumer returned nothing. The
check sat off the path that actually gates. A guard nobody calls is prose, so it moved onto the only
entry point.

`ParseTier` fails to the *safest* tier, not the zero one: an unrecognised tier name returns
`(TierIrreversible, error)`, so even a caller that ignores the error fails closed. A backend adding a
new tier name makes agents propose rather than act — the correct direction to be wrong.

Mutating writes are discrete, native, verifiable calls. Never CodeAct for durable writes: letting the
model write code that calls tools collapses N mutations into one opaque execution, defeating
per-call gating, approval and audit. It is fine for read orchestration.

### One tool definition, verified rather than trusted

A capability is declared once, as data, and three things fall out of that one declaration:

1. **Render.** `RenderCapabilities` generates the model-facing tool list from the specs, sorted
   stably by name. No agent hand-writes its op list in a prompt, so the prompt cannot drift from the
   code. Enum values are rendered because without them the model invents its own and every call fails
   verification.
2. **Verify.** `VerifyCall` checks each emitted call against the specs. The model *will* invent
   opcodes and operands; that is a fact to verify against, not something to prompt away.
   `GateProgram` verifies the whole program before gating any of it — silently dropping one op of a
   multi-op program is how you get a half-applied write.
3. **Gate.** Routing by tier, as above.

`ToolSet` closes the same loop for dispatch, pairing a spec with its handler in one `Add` call.
Without it a capability's name lives in three unreconciled places — the spec, a name constant, and a
`switch` arm — and missing either of the last two fails *silently and totally*: the model emits the
name, verification passes, the gate routes it to Apply, the switch matches nothing, and the call
vanishes with no error and no log while the run reports success. One production agent carried 11
capability specs, 10 name constants and 8 switch arms, plus a 228-line drift test written to make
that mistake loud, because nothing could make it impossible.

`ToolCall.Args` is `map[string]any`, not `map[string]string`, and that was a hard-won correction. The
first real program a model wrote was thrown away because `{"minutes": 20}` arrived as a JSON number
against a string-only contract. The model was right and the seam was wrong; models are also
inconsistent about `20` versus `"20"`. Read args through `ArgString` / `ArgInt` / `ArgStringList`,
which normalise whatever JSON type arrived.

### Re-gather fresh; thread decisions, not a transcript

Context rot is universal and non-linear — quality degrades well before the window fills. So `Gather`
runs once per run, before the loop, and between rounds only a typed carry changes: the prior verdict
and the judge's feedback. Never a re-gather, never a transcript. Where the source of truth is
external and re-derivable, re-deriving it beats accumulating history.

The tool loop is the deliberate exception: its working memory grows every iteration because each
observation genuinely changes the next step. Even there, prior results are carried as typed batches
(`ToolRoundInfo.PriorResults`), not as an appended conversation.

Asking for more context mid-run splits three ways by *who* has the data. Something the agent can
fetch is a read-tier tool, called synchronously. Fresh durable state is an explicit re-gather tool the
model calls, not silent churn. Information only the user has is a proposal — the run ends and the
answer re-triggers it.

Long-term memory is the durable store plus a typed note-to-future-self, not a parallel memory system.

### One live instance per work-unit

Each agent instance owns one work-unit — one planning agent to one project — the autonomous
equivalent of a chat `session_id`. At most one live run per unit, held by a lease. External changes
route *into* the live instance through its inbox as messages; they do not spawn a competing run. The
loop is a bounded, work-conserving reducer: drain the inbox, fold, run a round, repeat while there is
work, and yield on a fairness cap.

This is [its own page](agent-runtime.md), including the honest account of what single-writer does and
does not guarantee.

### The kit declares its seams; it imports nothing

A loop primitive that cannot call a model is not a loop primitive, so the kit must reach an LLM. The
question is which side owns the type. If the kit imported a provider package, every consumer would
inherit that provider — and its cloud SDK, metrics stack and service clients — whether they used it
or not.

So the kit **declares** the contract and the provider implements it:

```go
type StructuredCaller interface {
    CompleteJSON(ctx context.Context, req StructuredRequest) (string, error)
}
```

One method. Implement it against OpenAI, Anthropic, Bedrock, Vertex, Ollama, or a fake in a test, and
every structured path in the kit runs on it. `TextCaller` is the prose sibling; implement it only if
your agent generates prose.

The measured cost of getting this backwards: before the seam was inverted, `go list -deps` on the
pre-extraction package reported **537 transitive packages, 341 of them non-stdlib** — 95 from an AWS
SDK, 60 from gRPC. Comparing like with like, the extraction took that **341 → 1**
(`github.com/google/uuid`, for id generation). One in-house LLM package accounted for roughly 513 of
the 537 on its own.

Two lessons, not one. The LLM half was *structural* and was fixed by inverting the seam, not by
deleting a feature. The queue half — about 20 packages for a job payload and envelope type — was a
genuine misplacement, and it was evicted rather than moved: the kit ships no queue integration, no
handler and no fan-out. Hosting is the consumer's job. `scripts/check-deps.sh` runs in CI as the
tripwire, failing on a budget overrun or a banned import.

The same discipline applies to every other seam: `ActorStore`, `TemplateProvider`, `NarrationSink`,
`Observer`. The kit declares; you supply.

`ErrThrottled` deserves a note, because it is the one error the kit does **not** retry or fall back
on. `GenerateStructured` tries the primary model twice (a forced-schema drop is intermittent, so the
same-model retry usually lands it) then the fallback model once. But a throttle short-circuits
immediately and surfaces wrapped, so the caller can hand the work back to its queue and let the queue
be the backoff. Retrying into a provider that is already shedding load burns a retry hop while
holding a worker slot and worsens the contention that caused it. Callers branch on
`errors.Is(err, agentkit.ErrThrottled)`.

## What is built, and what is not

Stated plainly so nobody rebuilds something that ships, or relies on something that does not.

### Built, with tests and a production consumer

| Area | Symbols |
|---|---|
| Capability declaration | `Capability`, `CapabilityArg`, `ToolCall`, `Plan`, `RenderCapabilities`, `VerifyCall`, `GateProgram` |
| Dispatch | `ToolSet` — spec and handler in one declaration, then `GateProgram` / `ApplyPlan` |
| Proposable actions | `ActionSet`, `ChangeCategory`, `Bind` / `BindMixed`, and the verification harness |
| Loops | `RunOnce`, `RunOnceText`, `Pair`, `RunToolLoop`, `RunToolLoopTiered` |
| Tier safety | `Tier`, `TierReadOnlyExplicit`, `AutonomyRung`, `MaxRungFor`, `ParseTier`, `ProposedAction` |
| LLM seam | `StructuredCaller`, `TextCaller`, `GenerateStructured`, `ErrThrottled`, `ExtractJSON` |
| Actor model | `RunActor`, `RunActorWithMessage`, `Lease`, `Inbox`, `ActorStore`, `UnitWorker`, `AgentRuntime`, `InMemoryActorStore` |
| Prompt plumbing | `TemplateProvider`, `MapTemplateProvider`, `ReflectivePrompt`, `RenderReflective` |
| Observability | `Observer`, `Narrator`, `NarrationSink`, `NarratingObserver` |
| Local development | `RunLocalPair`, `RunLocalOnce`; three reference agents in [`example/`](../../example/) |

`RenderReflective` is worth calling out as a case of the kit learning from a consumer rather than
guessing ahead of one. It was deliberately not built until a real tool-loop consumer existed, because
prompt-assembly shape is exactly the thing you get wrong speculatively. When that consumer arrived,
its `Render` and the two example renders all repeated the same framing — request line, round position,
reflect-on-prior-results, final-round stop — with only a doctrine string and a per-item line renderer
differing. The scaffold took that consumer's `Render` from about 30 lines to about 6.

### Built, but adopted unevenly

The neutral `Proposal` type and its helpers (`Build`, `DeriveArchetype`, `DedupeKey`, `MaxProposals`,
`ChangeEvaluator`, `FilterNoOpProposals`) are real, tested code — and **both production agents
declined the seam**, building their backend's proposal type directly instead. That is a seam
consumers turned down, not one awaiting adoption: the kit will not import a backend proto, so the
neutral type costs a per-consumer mapper. Whether agents should decouple from the backend proto at
all is genuinely open.

The tokenizer (`TokenizeQuestion`, `Token`, `Labeler`, `Ref`, `ObjectRef`, `StripTokens`) is
production-proven and does carry traffic. It exists because of a live bug in which the same "…to THIS
PROJECT" ask shipped four times with nothing actually named. `Build` is the one choke point that
tokenizes an ask against the agent's `Labeler`, so an ask that does not name what it acts on cannot
ship.

### Not built

- **A `ToolRegistry` for mixed read-and-mutating tool sets in one loop.** Waiting for a write-tool
  consumer. Today `RunToolLoopTiered` takes a single tiered tool.
- **A distinct `ReadTool` type.** `RunToolLoop` accepts any `Tool`, so "a mutation physically cannot
  auto-execute on the read path" is convention rather than structure — pass a mutating tool to
  `RunToolLoop` instead of `RunToolLoopTiered` and it will run live. The tier gate that *is*
  structural is `GateProgram` and the tiered loop.
- **An enforced bounded mandate and audit for reversible-tier auto-runs.** Reversible calls auto-run
  today under the round cap alone.
- **A fence-guarded or advisory-lock single writer.** Deliberately deferred — see
  [the runtime page](agent-runtime.md) for why fencing an idempotent write buys efficiency rather
  than correctness.
- **The full generic `AgentMessage` inbox with typed intents and payloads.** `ActorMessage` is the v1
  envelope.
- **`EventLog` snapshot-and-tail.** Cut, not deferred — agents re-gather fresh from the durable
  store, so the store plus the agent's persisted output already *are* the state. An event log would
  duplicate it. Nothing plans to build it unless a consumer needs deterministic event replay.
- **A generic `agenttest` package and an eval harness.**

## What was deliberately not built

Distinct from the list above: these were considered and rejected or deferred on principle, not merely
unbuilt for want of time. Each waits for a consumer that genuinely needs it.

- **Generic self-editing or vector memory.** The durable store plus a typed note is the memory.
- **A conversation tier and a `RunTurn` verb.** Build it when a conversational surface exists.
- **`[]Agent` dispatch tables.**
- **A universal `Verdict` output struct shared by all agents.** The verdict is the breaking-point seam
  between framework and agent; a universal one designed at a sample size of one would be a guess
  every agent then fights.
- **An LLM `Compactor`.**
- **Multi-agent orchestration.** If it is ever built: decompose by context isolation, never by phase;
  default to a single agent with good tools; use orchestrator-worker with a single writer only for
  independent read-parallel branches; avoid peer meshes. The token cost is roughly an order of
  magnitude, so spend it only when the task value justifies it.

The extraction that produced this module landed at the second-consumer point. Extracting at one
consumer risks over-fitting the shared shape to it; the second consumer is what proves the shape is
actually shared.

## Rejected alternatives

**A chat or transcript model.** Conversational agents are a *service* shape — turn, respond, wait,
accumulate a transcript, synchronous transport. Autonomous agents are a *worker* shape — gather,
reason, propose, human-gated writes, no transport waiting on them. A personal-assistant stress test
showed the loop, the context strategy and the transport are opposite for the two, so forcing one
runtime to do both over-fits it to neither. They belong on separate runtimes joined by an explicit
bridge over shared substrate: both act through the same tiered capabilities and the same
draft-and-hold gate, so gating and voice stay identical; handoff is a message into a unit's inbox in
one direction and reading the proposals and notes the autonomous agent wrote in the other; and the
two meet at the *persisted* layer, never the in-flight layer. Bringing a conversational runtime onto
agentkit later is then a new shape on the same bridge rather than a rewrite.

**A universal `Verdict` type.** See above — the verdict is precisely the framework/agent seam.

**A mandatory worker-to-reviewer spine.** Rejected by the self-correction finding: an ungrounded
same-model critic degrades results, so the judge must be an option with a pluggable, signal-ranked
verifier.

**CodeAct for durable writes.** Rejected: it collapses N mutations into one opaque execution,
defeating per-call gating, approval and audit.

**Adopting a heavy agent framework** (LangGraph, CrewAI, AutoGen) instead of a thin runtime.
Rejected: heavy frameworks leak exactly where you mutate durable, reversible, approval-bound business
state — and the thin substrate already existed and was hardened.

**An ownership brake as a hard wall.** The kit does not ship one, and it should not be lifted from
older designs. The reference planning agent demoted it on its own explicit decision: "who created
this record" is a signal an agent reasons over, not a gate. It survives only on an automatic
reconcile path, where it fails safe by escalating to a proposal.

**A FIFO queue as the single-writer lock.** Evaluated and rejected: per-group in-flight messages are
released when the visibility timeout expires, so a run lasting tens of seconds could double-fire.
A FIFO queue is an ordering primitive, not a lock. The lease is the single writer; the queue is
intake.

## Research grounding

**Self-correction (the load-bearing finding).** Huang et al. 2024, *Large Language Models Cannot
Self-Correct Reasoning Yet* ([arXiv:2310.01798](https://arxiv.org/abs/2310.01798), ICLR 2024);
Kamoi et al. 2024, *When Can LLMs Actually Correct Their Own Mistakes?*
([arXiv:2406.01297](https://arxiv.org/abs/2406.01297), TACL). Also Madaan et al. 2023, *Self-Refine*
([arXiv:2303.17651](https://arxiv.org/abs/2303.17651)) and Shinn et al. 2023, *Reflexion*
([arXiv:2303.11366](https://arxiv.org/abs/2303.11366)).

**Loop topology and termination.** Yao et al. 2023, *ReAct*
([arXiv:2210.03629](https://arxiv.org/abs/2210.03629)) and *Tree of Thoughts*
([arXiv:2305.10601](https://arxiv.org/abs/2305.10601)); Du et al. 2023, *Improving Factuality and
Reasoning through Multiagent Debate* ([arXiv:2305.14325](https://arxiv.org/abs/2305.14325));
Anthropic, *Building Effective Agents*.

**Tools and code execution.** Wang et al. 2024, *Executable Code Actions Elicit Better LLM Agents*
([arXiv:2402.01030](https://arxiv.org/abs/2402.01030), ICML 2024) — the source of the CodeAct
trade-off; Anthropic, *Writing Tools for Agents*, on tool descriptions as the model's only window
into a tool.

**Context.** Packer et al. 2023, *MemGPT* ([arXiv:2310.08560](https://arxiv.org/abs/2310.08560));
Chroma, *Context Rot*; Anthropic, *Effective Context Engineering for AI Agents*.

**Actor-per-unit and single-writer.** Bernstein et al., *Orleans: Distributed Virtual Actors for
Programmability and Scalability* (MSR-TR-2014-41); Cloudflare Durable Objects; Kleppmann, *How to Do
Distributed Locking* — the argument that for idempotent or last-writer-wins writes a lock is an
efficiency optimisation rather than a correctness requirement.

## See also

- [README](../../README.md) — install, the smallest useful agent, the safety model in brief
- [reference.md](../reference.md) — symbol, signature, traps, compiled samples
- [building-an-agent.md](../guides/building-an-agent.md) — the order of work
- [providers.md](../providers.md) — implementing the LLM seam
- [agent-runtime.md](agent-runtime.md) — one live instance per work-unit
- [proposals.md](proposals.md) — when an ask is warranted
- [dialogue.md](dialogue.md) — asking a human, and never asking twice
- [CONTRIBUTING-agentkit.md](../contributing.md) — what gets a PR rejected
- [example/](../../example/) — three reference agents and a runnable demo
