// Package agentkit builds AUTONOMOUS agents that CHANGE THINGS — safely.
//
// You declare what your agent may do as typed Capability specs. agentkit renders them into the
// prompt (so the tool list cannot drift from your code), verifies the model's output against them
// (models invent opcodes — a fact to verify, not to prompt away), and gates every call by
// REVERSIBILITY: reversible calls may run live; irreversible ones are routed to a Propose bucket
// that path physically cannot execute. Around that gate sit budgeted loop primitives with
// agent-controlled termination, a hard round ceiling, and a degeneration guard.
//
// Go owns the write. The loop returns a DECISION; it never persists.
//
// Shape: cron/event-triggered, gather → reason → act-or-propose (the "worker" shape).
// NOT for turn-by-turn chat (no transcript, no streaming — deliberately stateless per run).
//
// # ⭐ FIRST DECISION: how do I declare what my agent can do?
//
// There are four constructors and they are NOT interchangeable. Pick by answering ONE question —
// where does the act's CONTRACT (its opcode, operands, tier) come from?
//
//	Your BACKEND serves an op contract      → Bind(ctx, src, uses)
//	  ...and some acts are yours alone      → BindMixed(ctx, src, uses, local)
//	  ...and there is NO backend contract    → NewActionSet[S]()      (declare it all yourself)
//	The MODEL emits calls[] you must GATE    → NewToolSet[S]()        (spec + handler, then GateProgram)
//
// The first three build an *ActionSet — what your agent may PROPOSE (the user blesses it, then a
// backend op applies it). The fourth builds a *ToolSet — what your agent DOES itself, gated by tier.
// An agent may need both; most need one.
//
// ⚠️ ALL FOUR COMPILE, so choosing wrong fails at REVIEW, not at build. (The arg types no longer
// differ — CapabilityArg is an alias for Operand — but the four constructors still do.)
//
// ⚠️ BindMixed(ctx, nil, nil, local) is CORRECT even though it reads like a mistake: a Local[S] act
// carries Run and Ask, which ActionSet.Add cannot take, so BindMixed is the only door to a local act
// even when there is no backend contract. "nil, nil" means "I have no backend ops", not "I forgot
// the arguments".
//
// # Start here
//
//	docs/reference.md         — symbol → file:line → signature, traps, compiled samples, not-built
//	example_doc_test.go       — runnable Examples (go test ./...)
//	example/                  — three reference agents, wired end to end
//	docs/providers.md         — bring your own LLM (OpenAI, Anthropic, Ollama, a fake)
//	docs/guides/building-an-agent.md    — the order of work
//	docs/design/architecture.md — why it is shaped this way
//
// # Bring your own everything
//
// The kit declares interfaces and you supply the implementations. It has no provider client, no
// queue, no database and no config service, which is why importing it costs ONE external dependency:
//
//	StructuredCaller / TextCaller  — your LLM (one method; see docs/providers.md)
//	ActorStore (Lease + Inbox)     — durable single-writer state; an in-memory one ships here
//
// One helper the kit does NOT call — your Role() consults it, so the kit never learns where your
// prompts live:
//
//	TemplateProvider               — prompt/model/region per (agent, role); nil = embedded defaults
//	NarrationSink                  — where live progress narration is written
//	Observer                       — per-round latency/metrics
//
// Nothing here dictates what triggers your agent. A queue consumer, a cron tick, an HTTP handler and
// a test are all equally valid front doors — see example/ for the hosting contract.
//
// # The two most load-bearing design principles
//
//   - One loop primitive in two transport flavors, compositions on top. RunOnce (a structured call →
//     a typed verdict) and RunOnceText (→ prose) are the atoms; Pair and RunToolLoop compose them.
//     Learn the atom and the rest follows.
//
//   - Agent-controlled termination inside a code-enforced ceiling. The loop stops when the agent says
//     it is done (a non-material result, or the judge approves); code enforces a hard round CEILING
//     and a degeneration guard, recording a stalemate rather than ever shipping a contested result.
//     Go owns the write — the loop returns a DECISION, it never persists. (A lesson learned the hard
//     way: never let the model's end_turn be the write decision.)
//
// # What is BUILT today vs PLANNED
//
// The kit is built INCREMENTALLY, each slice validated by a real production consumer before it lands
// here. This is the current ground truth of the code; the architecture doc describes the full target.
//
//	BUILT (this package, with tests + a production consumer):
//	  - Capability / CapabilityArg / ToolCall / Plan — declare what an agent may do, ONCE, as data (capability.go)
//	  - RunOnce / RunOnceText / Pair / RunToolLoop / RunToolLoopTiered — the loops (loop.go, text.go, tools.go)
//	  - StructuredCaller / TextCaller / GenerateStructured — the LLM seam, provider-agnostic (llm.go)
//	  - Skip / IsSkip — "acknowledged, but not done": the outcome `return nil` cannot express (skip.go)
//	  - RunActor / RunActorWithMessage — the actor-per-unit loop: single-writer + starvation bound (actor.go)
//	  - Lease / Inbox / ActorStore / UnitWorker / ActorMessage / UnitKey / ErrLeaseLost — the actor contracts (actor.go)
//	  - Two guards: Guard 1 = control-loop fail-through; Guard 2 = Heartbeat→ErrLeaseLost cancels a superseded round (efficiency)
//	  - InMemoryActorStore — a real ActorStore for tests and single-process local dev (actor_inmem.go)
//	  - Verifier / OnceAgent / TextAgent / RoleSpec / LoopConfig / Result / Outcome / Material (agent.go)
//	  - Observer — the per-round latency seam
//	  - TemplateProvider / MapTemplateProvider — keyed (agent, role) prompt/model/region resolver (prompts.go)
//	  - ReflectivePrompt / RenderReflective — prior-results framing for a tool loop (reflective.go; it took
//	    one consumer's Render from ~30 lines to ~6). Use it; do not re-implement.
//	  - Narrator / NarrationSink / NarratingObserver — live "what I'm doing now" doc for long runs (narrate.go)
//	  - RunLocalPair / RunLocalOnce — debug runners: gather + loop, print each step, no queue, no apply (runlocal.go)
//	  - example/ — reference agents: a RunOnceText "morning note", a RunToolLoop "research note", a tiered loop
//	  - example_doc_test.go — the README's samples, compiled + run in CI
//
//	  A DURABLE ActorStore is yours to provide (this package must not import infrastructure). The
//	  production binding it was extracted from is Postgres-backed: two tables (unit_lease, unit_inbox)
//	  satisfying ActorStore, injected via NewRuntime. GUARANTEE: single-writer = MUTUAL EXCLUSION
//	  (lease) + LAST-WRITER-WINS idempotent writes. The fence is the lease-STEAL DETECTION token
//	  (Guard 2), NOT a write-conditioner — fencing an idempotent write buys efficiency, not
//	  correctness (Kleppmann), so it is deliberately not built. If YOUR write is NOT idempotent,
//	  either make it so or add a fence-aware conditional write (WHERE fence < :n).
//
//	BUILT (in this package; adopted unevenly — know before you build on it):
//	  - TokenizeQuestion / Token / Labeler / MapLabeler / Ref / ObjectRef / StripTokens — production-proven.
//	    The tokenizer guarantees an ask NAMES what it acts on (it was written after a live bug in which
//	    four different asks all rendered as "…to THIS PROJECT").
//	  - Proposal / ProposalOp / UserSlot / Build / DeriveArchetype / DedupeKey / MaxProposals /
//	    ChangeEvaluator / FilterNoOpProposals — the neutral proposal TYPE + helpers. Real, tested code.
//	    ⚠️ Both production agents DECLINED this seam and build their backend's proposal type directly.
//	    It is a seam consumers turned down, not one awaiting adoption: the kit will not import a
//	    backend proto, so the neutral type costs a per-consumer mapper. Open question — should agents
//	    decouple from the backend proto at all?
//
//	DEFERRED — designed, waiting on a consumer to prove the shape (NOT in this package):
//	  - ToolRegistry for MIXED read+mutating tool sets in one loop — lands with a write-tool consumer
//	  - AgentMessage / Dispatch (the full generic inbox) — ActorMessage (actor.go) is the v1 envelope
//	  - Fence-GUARDED / advisory-lock single-writer — ONLY for a future NON-idempotent write
//
//	CUT — decided against, not merely unbuilt:
//	  - EventLog snapshot/tail — the inbox + the agent's persisted output suffice
//	  - agenttest (round-trip + spy-env) and an eval harness
//
// Do not document or import a PLANNED symbol as if it exists; the developer guide marks each step
// BUILT/PLANNED accordingly.
package agentkit
