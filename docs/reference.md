# agentkit API reference

Symbol → `file:line` → signature, plus the traps that are not visible in a signature.

Written to be **grepped** — by a person looking up one call, and by a coding agent reading the whole
file. Lookup tables first, rules second, runnable samples last.

- Every `file:line` anchor points into this repository and is asserted against source in CI. If an
  anchor and the code disagree, **believe the code** and please file an issue.
- Code blocks marked **✅ compiled** are extracted from [`example_doc_test.go`](../example_doc_test.go)
  and run in CI. Unmarked blocks are illustrative — they show shape, not a copy-paste-ready program.
- Anything not listed here is probably in [Not built](#not-built). Check before you import it.

## Contents

- [Choosing a constructor](#choosing-a-constructor) — ⭐ read this first
- [Symbol index](#symbol-index)
  - [Capabilities, verification and the tier gate](#capabilities-verification-and-the-tier-gate)
  - [ToolSet — spec and handler in one declaration](#toolset--spec-and-handler-in-one-declaration)
  - [Asking a human](#asking-a-human)
  - [Dialogue types](#dialogue-types)
  - [Bind — fetch the contract, don't copy it](#bind--fetch-the-contract-dont-copy-it)
  - [ActionSet — proposable actions](#actionset--proposable-actions)
  - [The ActionSet test harness](#the-actionset-test-harness)
  - [Tiers and autonomy](#tiers-and-autonomy)
  - [Loops](#loops)
  - [Interfaces you implement](#interfaces-you-implement)
  - [Results](#results)
  - [Verifiers, runtime, narration](#verifiers-runtime-narration)
  - [Constants and accepted values](#constants-and-accepted-values)
- [Rules](#rules)
- [Task: add a capability](#task-add-a-capability)
- [Task: add a proposable action](#task-add-a-proposable-action)
- [Task: pick a loop](#task-pick-a-loop)
- [Samples](#samples)
- [Testing your agent](#testing-your-agent)
- [Maturity — what carries production traffic](#maturity--what-carries-production-traffic)
- [Not built](#not-built)
- [Reference implementations](#reference-implementations)
- [How symbols are named](#how-symbols-are-named)

### Other documents

| Question | Document |
|---|---|
| a signature, a trap, a sample | **this file** |
| how do I build an agent, in order | [guides/building-an-agent.md](guides/building-an-agent.md) |
| how do I plug in my LLM | [providers.md](providers.md) |
| why is it shaped this way | [design/architecture.md](design/architecture.md) |
| why not the obvious alternative | [design/agent-runtime.md](design/agent-runtime.md) |
| the proposal/fixpoint model in full | [design/proposals.md](design/proposals.md) |
| the dialogue contract | [design/dialogue.md](design/dialogue.md) |
| how to contribute | [contributing.md](contributing.md) |
| runnable code | [example/](../example/), [example_doc_test.go](../example_doc_test.go) |

---

## Choosing a constructor

**Start here.** Four exported entry points build "what my agent can do." They are not interchangeable,
and **all four compile** — so choosing wrong produces no compiler error and no failing test. Every
other trap in this package is guarded (`Validate` catches duplicate names, `VerifyAction` fails closed,
`AddMultiField` mispairing is rejected in both directions). This one is unguarded, and it is the first
decision you make.

Pick by one question: **where does the act's contract — opcode, operands, tier — come from?**

| Your situation | Use | Builds |
|---|---|---|
| the **backend** serves an op contract | `Bind(ctx, src, uses)` | `*ActionSet[S]` |
| …and some acts are yours alone | `BindMixed(ctx, src, uses, local)` | `*ActionSet[S]` |
| …and there is **no** backend contract | `NewActionSet[S]()` | `*ActionSet[S]` |
| the **model** emits `calls[]` you must gate | `NewToolSet[S]()` → `GateProgram` | `*ToolSet[S]` |

**`ActionSet` is what the agent may propose** — a human blesses it, and a backend op applies it.
**`ToolSet` is what the agent does itself**, gated by tier. An agent may need both; most need one.

⭐ **`BindMixed(ctx, nil, nil, local)` is correct, not a mistake.** A `Local[S]` act carries `Run` and
`Ask`, which `ActionSet.Add` cannot take, so `BindMixed` is the only door to a local act — even when
you have no backend at all. The kit's own error text says so.

⚠️ **Reading that call as misuse is the trap this table exists to prevent.** `nil, nil` reads as
"I passed nothing," but it means "I have no backend ops," which is a legitimate and common shape.

---

## Symbol index

### Capabilities, verification and the tier gate

| Symbol | Anchor | Signature / shape |
|---|---|---|
| `Capability` | `capability.go:57` | `struct{Name, Description string; Tier Tier; Args []CapabilityArg}` |
| `CapabilityArg` | `capability.go:89` | `struct{Name, Description, Type string; Required bool; EnumValues []string; List bool}` |
| `ToolCall` | `capability.go:106` | `struct{Name string; Args map[string]any}` |
| `ToolCall.ArgString` | `capability.go:113` | `func(name string) string` |
| `ToolCall.ArgStringList` | `capability.go:140` | `func(name string) []string` |
| `ToolCall.ArgInt` | `capability.go:181` | `func(name string) (int, bool)` — ⚠️ see the trap below |
| `Plan` | `capability.go:232` | `struct{Apply, Propose []ToolCall}` |
| `RenderCapabilities` | `capability.go:246` | `func(caps []Capability) string` |
| `VerifyCall` | `capability.go:284` | `func(caps []Capability, call ToolCall) error` |
| `GateProgram` | `capability.go:344` | `func(caps []Capability, program []ToolCall, rung AutonomyRung) (Plan, error)` |

⚠️ **`ArgInt` accepts native Go integers, not only JSON ones.** Args usually arrive from
`json.Unmarshal`, where every number is a `float64` — so an implementation handling only `float64` and
`string` passes every test that goes through the wire, then silently rejects a hand-built
`map[string]any{"n": 812}` in someone's unit test, and a required int vanishes into `ok == false`.
That asymmetry (accepts `"812"`, rejects `812`) is undebuggable for a newcomer, so `ArgInt` handles
every integer kind explicitly: `int`, `int8/16/32/64`, `uint`, `uint8/16/32/64`, `float32`, `float64`,
`json.Number`, and a numeric `string`. Anything else returns `ok == false`.

### ToolSet — spec and handler in one declaration

See [Task: add a capability](#task-add-a-capability) for the worked pattern.

| Symbol | Anchor | Signature / shape |
|---|---|---|
| `ToolSet[S]` | `toolset.go:74` | the set: capability specs plus their handlers |
| `NewToolSet[S]` | `toolset.go:83` | `func() *ToolSet[S]` |
| `RunFunc[S]` | `toolset.go:68` | `func(ctx, state *S, call ToolCall) error` — `nil` means done **or deliberately skipped** |
| `ToolSet.Add` | `toolset.go:96` | `func(spec Capability, run RunFunc[S]) *ToolSet[S]` — chains |
| `ToolSet.AddProposeOnly` | `toolset.go:120` | ⭐ a capability the tier can never auto-run — **needs no handler** |
| `ToolSet.Capabilities` | `toolset.go:128` | `func() []Capability` — feeds `RenderCapabilities` |
| `ToolSet.Lookup` | `toolset.go:155` | `func(name string) (Capability, bool)` — read one spec back |
| `ToolSet.GateProgram` | `toolset.go:165` | `func(program []ToolCall, rung AutonomyRung) (Plan, error)` |
| `ToolSet.ApplyPlan` | `toolset.go:177` | `func(ctx, state *S, plan Plan) []error` — ⛔ **never runs `plan.Propose`** |
| `ToolSet.Validate` | `toolset.go:213` | `func() error` — ⚠️ an **empty set**, an empty or duplicate name, an empty `Description`, a missing handler, and a **zero `Tier`** |
| `CapabilitiesFrom[S]` | `toolset.go:307` | ⭐ `func(set *ActionSet[S]) []Capability` — derive the model's tool list *from* the actions, so an agent that both does and proposes declares each op once |

### Asking a human

These are the dialogue pieces with production consumers. They are pure functions over data you
already have — you never construct a kit type and hand it back.

| Symbol | Anchor | Signature / shape |
|---|---|---|
| `AskKey` | `proposal.go:197` | `func(agent, objectKind, objectID, op string, discriminator ...string) string` |
| `Ref` | `proposal_label.go:61` | ⭐ `func(l Labeler, kind, id string) ObjectRef` — **use this, not the struct literal** |
| `Labeler` | `proposal_label.go:31` | the seam that resolves an id to a human-readable label |
| `MapLabeler` | `proposal_label.go:43` | `func(map[string]map[string]string) Labeler` |
| `TokenizeQuestion` | `proposal_question.go:89` | `func(l Labeler, q string, anchor ObjectRef, also ...ObjectRef) string` |
| `ObjectRef` | `proposal.go:58` | `struct{Kind, ID, Label string}` — ⚠️ see the trap below |

⚠️ **The empty-`Label` trap.** The kit promises that no agent can ship an ask that does not name what
it acts on — and that guarantee **silently no-ops when `Label` is empty**: the early return at
`proposal_question.go:190` hands the question back untouched. Writing `ObjectRef{Kind: "task", ID: id}`,
which the field list openly invites, loses the guarantee and tells you nothing.

⭐ **Always build a ref with `Ref(labeler, kind, id)`.** It resolves the label from your own labeler and
yields the zero ref for an unresolvable id, so an invented or stale id can never become a chip.

⚠️ **Pass a discriminator to `AskKey` whenever your opcode is a channel** — one opcode carrying many
different questions, such as `ask_user`. Without it, every question about one object shares a
single key and the upsert keeps only the last. The variadic makes omission silent, which is why this
warning lives here rather than in a code comment.

### Dialogue types

⚠️ Built, unit tested, and adopted by exactly one production agent. Read
[design/dialogue.md](design/dialogue.md) and the caveat under
[Maturity](#maturity--what-carries-production-traffic) before adopting.

| Symbol | Anchor | Signature / shape |
|---|---|---|
| `Turn` | `dialogue.go:104` | `struct{Kind TurnKind; Body string; About ObjectRef; Act *Op; Slots []TurnSlot; Discriminator string}` |
| `Turn.Discriminator` | `dialogue.go:133` | ⭐ what distinguishes this turn from another about the same object and op. A **field**, so identity travels with the value |
| `TurnQuestion` | `dialogue.go:59` | about information — carries no act |
| `Op` | `dialogue.go:77` | `struct{Opcode string; Args map[string]string}` — the **backend's** vocabulary, not a `ToolCall` |
| `TurnSlot` | `dialogue.go:90` | `struct{Field, Type, Label string; Required bool; Options []string; Default string}` |
| `NewQuestion` | `dialogue.go:137` | `func(body string, about ObjectRef, slots ...TurnSlot) (Turn, error)` |
| `NewQuestionVia` | `dialogue.go:159` | ⭐ `func(body string, about ObjectRef, channel Op, slots ...TurnSlot) (Turn, error)` — a question delivered over a backend channel op |
| `NewProposal` | `dialogue.go:168` | `func(body string, about ObjectRef, act Op, slots ...TurnSlot) (Turn, error)` |
| `Turn.Key` | `dialogue.go:229` | `func(agent string, discriminator ...string) string` — ⚠️ pass a discriminator when your opcode is a channel |
| `Reply` | `dialogue.go:241` | `ReplyAnswer` · `ReplyApprove` · `ReplyReject` · `ReplyRedirect` |
| `Reply.ReleasesAct` | `dialogue.go:273` | only **approve** releases the act |
| `Reply.IsRejection` | `dialogue.go:276` | ⚠️ a **redirect is not a rejection** |
| `Reply.HandsBackToAgent` | `dialogue.go:280` | redirect means the outcome is the agent's, now informed |
| `RenderDialogue` | `dialogue.go:290` | `func() string` — dialogue is not a capability, but the model must still be told |

### Bind — fetch the contract, don't copy it

| Symbol | Anchor | Signature / shape |
|---|---|---|
| `Bind[S]` | `bind.go:112` | `func(ctx, src ContractSource, uses []Use[S]) (*ActionSet[S], error)` — ⚠️ **fails closed** |
| `BindMixed[S]` | `bind.go:125` | same, plus `local []Local[S]` — ⭐ for acts the backend has never heard of |
| `ContractSource` | `bind.go:57` | `BackendOps(ctx) ([]Action, error)` — implement over your registry RPC |
| `Use[S]` | `bind.go:66` | which op, plus the premise (`Category`/`Input`/`Current`), `Run`/`Ask`, and `Extra` |
| `Local[S]` | `bind.go:251` | an `Action` plus readers — the agent owns the whole contract |
| `RunAct[S]` | `bind.go:220` | `func(ctx, state *S, act Action) error` |
| `AskFor` | `bind.go:223` | `func(act Action) string` — ⭐ typed, so the kit can enforce the ceiling and the no-`Ask` rule |
| `ExtraOf[X,S]` | `bind.go:263` | `func(set *ActionSet[S], opcode string) (X, bool)` — agent-private facts, typed |
| `CachedContract` | `bind.go:282` | TTL wrapper: survives an outage warm, ⚠️ **fails closed cold** |
| `NewCachedContract` | `bind.go:292` | `func(src ContractSource, ttl time.Duration) *CachedContract` |

### ActionSet — proposable actions

See [Task: add a proposable action](#task-add-a-proposable-action) for the worked pattern.

| Symbol | Anchor | Signature / shape |
|---|---|---|
| `ActionSet[S]` | `action.go:274` | the set: action declarations plus their state readers |
| `NewActionSet[S]` | `action.go:281` | `func() *ActionSet[S]` |
| `Action` | `action.go:204` | `struct{Opcode, Description string; Tier; Operands; Category; Input; TerminalStates; Args; UserFills}` |
| `Operand` | `action.go:187` | `struct{Name, Description, Type string; Required bool; EnumValues []string}` |
| `ChangeCategory` | `action.go:46` | `ScalarSet` · `Membership` · `Terminal` · `MultiField` · ⭐ `FillsSlot` · `AlwaysMints` · `Merge` |
| `StateReader[S]` | `action.go:259` | `func(ctx, state *S, act Action) (string, error)` — ⚠️ takes the **act**, so it reads the right object |
| `FieldReader[S]` | `action.go:270` | `func(ctx, state *S, act Action, field string) (string, error)` — note the `act` **and** the `field` |
| `ActionSet.Add` | `action.go:293` | `func(a Action, read StateReader[S]) *ActionSet[S]` — a `nil` reader is fine for `AlwaysMints`/`Merge` |
| `ActionSet.AddMultiField` | `action.go:307` | ⚠️ **use for `ChangeMultiField`** — its reader takes the operand name |
| `ActionSet.Lookup` | `action.go:333` | `func(opcode string) (Action, bool)` — the index; don't rebuild one |
| `ActionSet.VerifyAction` | `action.go:353` | `func(act Action) error` — ⚠️ **fails closed** |
| `ActionSet.WillChange` | `action.go:406` | `func(ctx, state *S, act Action) (bool, error)` — ⚠️ **fails open**; generated from `Category` |
| `ActionSet.Validate` | `action.go:503` | `func() error` — missing reader, `Input` typo, no `TerminalStates` |
| `RenderActions` | `action.go:614` | `func(actions []Action) string` |

### The ActionSet test harness

Production-safe to import: these live in the main package but are meant to be called from `_test.go`.

| Symbol | Anchor | Signature / shape |
|---|---|---|
| `VerifyActionSet` | `action_harness.go:71` | ⭐ `func[S](t TestingT, set *ActionSet[S])` — the whole formal contract, **one call** |
| `VerifyActionDeclarations` | `action_harness.go:97` | ⭐ `func[S](t TestingT, set *ActionSet[S])` — the reader-free half, for agents whose backend owns the premise gate |
| `VerifyActionSetVocabulary` | `action_harness.go:150` | ⭐ `func[S](t TestingT, set *ActionSet[S], truth VocabularyOracle)` — **enum drift**; the one error that degrades behaviour while every other check stays green |
| `VocabularyOracle` | `action_harness.go:133` | `func(opcode, operand string) []string` — `nil` means no opinion |
| `VerifyActionSetAgainst` | `action_harness.go:285` | `func[S](t TestingT, set *ActionSet[S], fixpoints map[string]*S)` — adds the assertion that needs real state |
| `SimulateTurns` | `action_harness.go:348` | `func[S](set *ActionSet[S], state *S, actions []Action) (surviving []Action, dropped map[string]string)` |
| `DropReason` | `action_harness.go:404` | ⭐ `func(dropped map[string]string, opcode string) string` — use this, not `dropped[opcode]` |
| `Explain` | `action_harness.go:424` | `func(surviving []Action, dropped map[string]string) string` |
| `FakeState` | `action_harness.go:48` | `map[string]string` with a `.Reader(key)` method — state with no database |
| `TestingT` | `action_harness.go:40` | the `*testing.T` subset the harness needs |

### Tiers and autonomy

| Symbol | Anchor | Value / meaning | Auto-runs |
|---|---|---|---|
| `TierReadOnlyExplicit` | `tier.go:68` | ⭐ **declare read-only on purpose** — the value `-1`. Use this in any set you `Validate` | ✅ |
| `TierReadOnly` | `tier.go:45` | lookups, no mutation. ⚠️ the **zero value**, and therefore ⛔ **rejected by `Validate`** — see below | ✅ |
| `TierReversible` | `tier.go:48` | mutation with a real undo | ✅ |
| `TierExternalReversible` | `tier.go:51` | external effect, recoverable but not silently | ❌ → `Propose` |
| `TierIrreversible` | `tier.go:53` | delete, mass communications, payments, permissions | ❌ **hard ceiling** |
| `MaxRungFor` | `tier.go:120` | `func(tier Tier) AutonomyRung` | — |
| `RungAsk` | `tier.go:101` | the floor — the call must be proposed for human approval | — |
| `RungAuto` | `tier.go:103` | the loop may execute the call live; pass this to `GateProgram` normally | — |

⛔ **The zero `Tier` is a rejection, not a default — and this catches people out.** `TierReadOnly` is
`iota == 0`, so a spec that simply *omits* `Tier` is indistinguishable at runtime from one that
deliberately declares read-only. Read-only auto-runs, so a forgotten field would silently make a
destructive act live.

Both `ToolSet.Validate` (`toolset.go:213`) and `ActionSet.Validate` (`action.go:503`) therefore reject
**`TierReadOnly` itself**, not merely some separate "unset" marker. There is no way to tell the two
apart, so the ambiguous value is refused outright.

⭐ **The consequence: `TierReadOnly` is unusable in any set you validate.** When an act really is
read-only, spell it `TierReadOnlyExplicit` (`tier.go:68`, the value `-1`). That makes "this may
auto-run" a statement rather than an omission — the four tiers you can actually declare are
`TierReadOnlyExplicit`, `TierReversible`, `TierExternalReversible` and `TierIrreversible`.

### Loops

| Need | Symbol | Anchor | Signature |
|---|---|---|---|
| one typed struct | `RunOnce` | `loop.go:19` | `[Ctx, V any](ctx, caller StructuredCaller, agent OnceAgent[Ctx,V], rc Ctx, feedback []string, obs Observer) (V, error)` |
| one piece of prose | `RunOnceText` | `text.go:40` | `[Ctx any](ctx, caller TextCaller, agent TextAgent[Ctx], rc Ctx, obs Observer) (string, error)` |
| draft → critique → revise | `Pair` | `loop.go:105` | `[Ctx any, V Material](ctx, caller, exec OnceAgent[Ctx,V], judge Verifier[V], same SameFunc[V], rc Ctx, cfg LoopConfig, obs Observer) Result[V]` |
| look up, then decide | `RunToolLoop` | `tools.go:91` | `[Ctx, Args, R any](ctx, caller, exec ToolAgent[Ctx,Args,R], tool Tool[Args,R], rc Ctx, cfg LoopConfig, same SameFunc[Args], obs Observer) ToolLoopResult[R]` |
| …and some tools mutate | `RunToolLoopTiered` | `tier.go:202` | ⭐ `[Ctx, Args, R any](ctx, caller, exec ToolAgent[Ctx,Args,R], tool TieredTool[Args,R], rc Ctx, cfg LoopConfig, same SameFunc[Args], obs Observer) TieredLoopResult[Args,R]` |

#### The tiered loop's own types

`RunToolLoopTiered` is `RunToolLoop` with tier-gated dispatch. Each intended call is routed by
`MaxRungFor(tool.Tier())`:

- **`RungAuto`** (read-only or reversible) — the loop calls the tool live, and results flow into the
  reflect carry.
- **`RungAsk`** (external-reversible or irreversible) — the loop **captures** the call as a
  `ProposedAction` and **stops**. The tool is never invoked.

⭐ The captured proposal *is* a usable result (`OutcomeApplied`): the run did its job — it worked out
what to do — and only the doing is gated. An agent whose tools are all read-only never hits propose and
behaves exactly like `RunToolLoop`.

| Symbol | Anchor | Signature / shape |
|---|---|---|
| `TieredTool[Args,R]` | `tier.go:163` | `Tool[Args,R]` plus `Tier() Tier` |
| `ProposedAction[Args]` | `tier.go:171` | `struct{ToolName string; Tier Tier; Args Args; Rationale string}` — the exact call the executor intended, for the consumer to replay on approval |
| `TieredLoopResult[Args,R]` | `tier.go:183` | `struct{Items []R; Rounds int; Outcome Outcome; StoppedReason string; Err error; Proposed []ProposedAction[Args]}` |
| `ToolStep[Args]` | `tools.go:32` | what the model emits each round: a call, or `Done` |
| `ToolRoundInfo[R]` | `tools.go:42` | what the executor sees on the next round |
| `ToolLoopResult[R]` | `tools.go:62` | what `RunToolLoop` returns — no `Args` parameter, no propose concept |

⭐ `TieredLoopResult` is its own type rather than a `Proposed` field on `ToolLoopResult`, so a consumer
that never uses tiers never sees a `ProposedAction`.

### Interfaces you implement

| Interface | Anchor | Methods |
|---|---|---|
| `OnceAgent[Ctx,V]` | `agent.go:40` | `Render(rc Ctx, info RoundInfo) (system, dynamic string)` · `Role() RoleSpec` |
| `TextAgent[Ctx]` | `text.go:17` | `Render(rc Ctx) (system, dynamic string)` · `Role() RoleSpec` |
| `ToolAgent[Ctx,Args,R]` | `tools.go:50` | `Render(rc Ctx, info ToolRoundInfo[R]) (system, dynamic string)` · `Role() RoleSpec` |
| `Tool[Args,R]` | `tools.go:24` | `Name() string` · `Call(ctx, args Args) ([]R, error)` |
| `Material` | `agent.go:16` | `IsMaterial() bool` — **required by `Pair`** |
| `Verifier[V]` | `agent.go:31` | `Verify(ctx, V) (ok bool, feedback []string, err error)` |
| `StructuredCaller` | `llm.go:83` | `CompleteJSON(ctx, StructuredRequest) (string, error)` |
| `TextCaller` | `llm.go:118` | `GenerateText(ctx, GenerateTextRequest) (LLMCallResult, error)` |
| `Observer` | `agent.go:150` | `ObserveLLM(role string, round int, dur time.Duration)` |
| `Labeler` | `proposal_label.go:31` | resolves an object id to a display label |
| `ContractSource` | `bind.go:57` | `BackendOps(ctx) ([]Action, error)` |
| `ActorStore` | `actor.go:88` | `Lease` (`actor.go:61`) plus `Inbox` (`actor.go:73`) |
| `UnitWorker` | `actor.go:108` | `Fold(ctx, msgs []ActorMessage) error` · `RunRound(ctx, round int, fence Fence) (bool, error)` |

See [providers.md](providers.md) for copy-pasteable `StructuredCaller` and `TextCaller` adapters.

### Results

| Symbol | Anchor | Shape / meaning |
|---|---|---|
| `Result[V]` | `agent.go:138` | `struct{Outcome; Result, LastResult V; Rounds int; Err error}` |
| `OutcomeApplied` | `agent.go:101` | agreed within budget → use `res.Result` |
| `OutcomeSilent` | `agent.go:104` | the model decided there was nothing to do → **success**; write nothing |
| `OutcomeStalemate` | `agent.go:107` | degenerated → `res.LastResult` holds the **executor's** output |
| `OutcomeError` | `agent.go:110` | → `res.Err` |
| `RoundInfo` | `agent.go:57` | `struct{Round, MaxRounds int; Feedback []string}` |
| `RoundInfo.IsFinalRound` | `agent.go:65` | `func() bool` — the executor should commit its best answer |
| `RoleSpec` | `agent.go:71` | `struct{Model, Region, Fallback, PromptType string; Temperature float64; MaxTokens int}` |
| `LoopConfig` | `agent.go:91` | `struct{MaxRounds int}` |
| `DefaultMaxRounds` | `agent.go:84` | `= 2` — an unset `LoopConfig{}` uses this, **not 1** |
| `SameFunc[V]` | `loop.go:74` | `func(a, b V) bool` — the degeneration guard; `nil` disables it |
| `ErrThrottled` | `llm.go:40` | release the work back to your queue; do not retry |
| `Skip` | `skip.go:35` | `func(reason string) error` — the work was legitimately NOT DONE. Returning `nil` instead counts it as processed, so an outage reads as healthy throughput |
| `IsSkip` | `skip.go:52` | `func(err error) (reason string, ok bool)` — ⚠️ a `Skip` does **not** survive `fmt.Errorf("%w")`; return it bare or forfeit skip handling |

⚠️ **`DefaultMaxRounds` is 2, and that is deliberate.** A one-round `Pair` is degenerate: the judge
rejects the first draft and you get an immediate stalemate, because no revision ever happens. An
explicit `MaxRounds: 1` is still honoured — that is a deliberately single-shot reflective check.

### Verifiers, runtime, narration

| Symbol | Anchor | Signature |
|---|---|---|
| `RulesVerifier` | `verify.go:30` | `[V any](fn RulesFunc[V]) Verifier[V]`; `RulesFunc[V] = func(V) (bool, []string)` |
| `VerifierChain` | `verify.go:40` | `[V any](verifiers ...Verifier[V]) Verifier[V]` — short-circuits on the first reject |
| `RenderReflective` | `reflective.go:55` | `[R any](p ReflectivePrompt[R], info ToolRoundInfo[R]) (system, dynamic string)` |
| `AgentRuntime` | `runtime.go:25` | `.Submit(ctx, k UnitKey, ask ActorMessage, w UnitWorker, obs) (ActorResult, error)` · `.RunUnit(…)` |
| `NewInMemoryRuntime` | `runtime.go:59` | `func(cfg ActorConfig) AgentRuntime` |
| `NewNarrator` | `narrate.go:206` | `func(sink NarrationSink, sum NarrationSummarizer, pacer func(phase string, idx int) string, opts ...Opt) *Narrator` |

### Constants and accepted values

| Fact | Value |
|---|---|
| `CapabilityArg.Type` accepted | `"string"`, `"int"`, `"date"`, `"enum"`, `"bool"` |
| `ArgInt` accepted arg types | every Go integer kind, `float32`, `float64`, `json.Number`, and a numeric `string` |
| `RoleSpec.Model` format | whatever string **your** `StructuredCaller` understands. The kit never parses it |
| Default round cap | `2` (`DefaultMaxRounds`, `agent.go:84`) |
| Where model ids live | wherever you put them — resolved per (agent, role) through `TemplateProvider` in `prompts.go` |

---

## Rules

Violating these causes known bugs.

1. **Never hand-write a tool list in a prompt.** Use `RenderCapabilities(caps)`. A hand-written list
   drifts silently, and if the prompt is runtime-overridable no test can catch it.
2. **Never execute `verdict.Calls` directly — always via `GateProgram`.** The tier gate is the only
   thing stopping an irreversible auto-run.
3. **A duplicate capability name fails the gate.** `caps` is a slice (the tier is read from the
   **first** match) while `ToolSet` handlers are a map (`Add` **overwrites**). A read-only `act`
   followed by an irreversible `act` therefore gated as auto-runnable and then executed the
   irreversible handler — silently. `GateProgram` now rejects duplicates outright, in either order.
   Declare each capability exactly once.
4. **A `GateProgram` error rejects the whole program.** Never drop the bad call and continue: dropping
   one op of a multi-op program half-applies a write.
5. **Never surface the judge's text to a user.** `Pair` ships the executor's output by design.
6. **Do not use `Pair` with a same-model, same-context judge for quality.** Ungrounded self-critique
   degrades results. Use `RulesVerifier`, or a differently-grounded judge.
7. **`RoleSpec.Model` is an opaque string.** A provider-prefixed convention like `provider/model-name`
   works well when one adapter fronts several providers, but the kit never parses it and a bare name is
   perfectly fine.
8. **`OutcomeSilent` is success**, not failure.
9. **Order `VerifierChain` cheap → expensive**: deterministic rules first, an LLM critic last.
10. **Declare a tier explicitly.** The zero value is a rejection, not "read-only".

---

## Task: add a capability

Use `ToolSet` (`toolset.go:74`). The spec and its handler are declared together, so the name exists in
**exactly one place** and cannot drift.

```go
tools := agentkit.NewToolSet[myState]().
    Add(agentkit.Capability{
        Name: "set_due_date", Tier: agentkit.TierReversible,
        Description: "Set the item's due date — only when the evidence states a real deadline.",
        Args: []agentkit.CapabilityArg{{Name: "due", Type: "date", Required: true}},
    }, func(ctx context.Context, s *myState, c agentkit.ToolCall) error {
        return store.SetDue(ctx, s.ItemID, c.ArgString("due"))
    })

if err := tools.Validate(); err != nil { return err }              // once, at wiring time
system := prompt + agentkit.RenderCapabilities(tools.Capabilities())
plan, err := tools.GateProgram(verdict.Calls, agentkit.RungAuto)
errs := tools.ApplyPlan(ctx, &state, plan)                         // never runs plan.Propose
```

**To add one:** copy the whole `Add` call. **To remove one:** delete it. Both are single edits that
cannot be half-done. A fuller version of this, with three tiers and the resulting plan printed, is
`ExampleToolSet` in [`example_doc_test.go`](../example_doc_test.go) — **✅ compiled**.

| Rule | Why |
|---|---|
| call `Validate()` at wiring time | catches an **empty set** (which would otherwise render no tools and report a legitimate-looking silent run), an empty name, a **duplicate** name (lookup takes the first spec, so the other is unreachable and its tier silently ignored), an empty `Description` (**that field is the prompt**), a spec with **no handler**, and a **zero `Tier`** |
| `Capabilities()` feeds the prompt | never hand-write a tool list — a hand-written one drifts silently |
| handlers take `*S` | real handlers share state: one call writes a draft that a later step composes |
| return `nil` to skip | a business brake ("the user already set this themselves") is a normal outcome, not an error |
| errors accumulate | a failed write never aborts the calls after it |

⚠️ **The bug this design removes.** Previously a capability's name lived in three unreconciled places —
the spec, a constant, and a switch arm. Miss one and the model emits the name, `VerifyCall` passes,
`GateProgram` routes it to `Apply`, and the switch matches nothing — so the call **vanishes with no
error while the run reports success**. `ApplyPlan` now returns an unregistered name as a hard error.

---

## Task: add a proposable action

⭐ **If your backend already serves its op contract, use `Bind` — do not retype it.**

```go
set, err := agentkit.Bind(ctx, backendOps, []agentkit.Use[State]{{
    Opcode:   "set_task_due_date",              // which op — your only identity claim
    Category: agentkit.ChangeScalarSet,         // the premise — your judgment
    Input:    "due_date",
    Current: func(_ context.Context, s *State, a agentkit.Action) (string, error) {
        return s.Task(a.Arg("task_id")).DueDate, nil   // the act names the target
    },
}})
```

Operands, types, enums, required flags, tier and description all arrive **from the backend**. A
compiled version of this pattern is `ExampleBind` in
[`example_doc_test.go`](../example_doc_test.go) — **✅ compiled**.

| Bind rejects | Which, without it, fails… |
|---|---|
| an opcode the backend does not have | in production — the whole program is discarded at verify time |
| an `Input` naming no operand | silently — it reads an absent arg and re-mints the ask forever |
| an understated tier | as a **safety** bug — the gate auto-runs an act that should have been withheld |
| ⭐ a withheld act with no `Ask` | silently — the gate withholds it and the user is never asked |

⚠️ **`Run` and `Ask` are typed, not an opaque bag** — that last check is only possible because the kit
can *see* both halves on one declaration. It turns a real production bug (`Plan.Propose` had zero
readers) into a startup error.

⚠️ **Not every act is a backend op.** Use `BindMixed(ctx, src, uses, local)`; a `Local` act carries its
own contract. And `BindMixed(ctx, nil, nil, local)` is legitimate — see
[Choosing a constructor](#choosing-a-constructor).

⚠️ **Failure modes.** Cold, with no contract, `Bind` **fails closed** — an agent that guesses its
operands loses whole programs. Wrap with `NewCachedContract(src, ttl)` so a backend redeploy does not
take the agent down, and note that the **TTL is load-bearing**: an indefinite cache rejects enum values
a deploy added, failing closed on valid model output.

### Hand-built sets

With no contract source, use `NewActionSet` (`action.go:281`) directly. Same `Add(spec, behaviour)`
shape as `ToolSet`, so there is one mental model.

```go
actions := agentkit.NewActionSet[myState]().
    Add(agentkit.Action{
        Opcode: "set_task_due_date", Category: agentkit.ChangeScalarSet, Input: "due_date",
        Description: "Set the task's due date.", Tier: agentkit.TierReversible,
        Operands: []agentkit.Operand{{Name: "due_date", Type: "date", Required: true}},
    }, func(_ context.Context, s *myState, _ agentkit.Action) (string, error) { return s.Task.DueDate, nil })
```

⭐ **You never write the comparison.** You declare the action's **category** — the shape of its effect —
and the kit *generates* `WillChange` from it. Your reader supplies only what the kit cannot know: the
current value. The underlying rule is a fixpoint test — an act changes nothing exactly when everything
it would add is already present and nothing it would remove still is. [design/proposals.md](design/proposals.md)
develops this in full.

**Pick the category — this is the whole job:**

| Category | Use when | You declare | Derived rule |
|---|---|---|---|
| `ChangeScalarSet` | it sets one field to one value | `Input` plus a reader | `WillChange = (s.X != v)` |
| `ChangeMembership` | it links two objects | `Input` plus a reader | `= ¬linked` |
| `ChangeTerminal` | archive or complete | `TerminalStates` plus a reader | `= ¬isTerminal(s.status)` |
| `ChangeMultiField` | several optional fields, e.g. `update_goal` | operands plus ⚠️ **`AddMultiField`** | `=` any **provided** field differs |
| ⭐ `ChangeFillsSlot` | a question asking the user for a value | `Input` plus a reader | `=` the slot is still empty |
| `ChangeAlwaysMints` | a create, or feedback with no slot | ⭐ nothing | the add-set is undefined, so no fixpoint exists |
| `ChangeMerge` | a subset or metadata merge | ⭐ nothing | not modelled in v1, so it mints |

**Two checks, with opposite defaults — this is deliberate:**

| | `VerifyAction` | `WillChange` |
|---|---|---|
| asks | is the opcode declared? are the operands real and present? | would this actually change anything? |
| on failure | ⚠️ **fails closed** — a malformed ask must not reach a human | ⚠️ **fails open** — an ask wrongly dropped is silent |

⭐ **`WillChange` is what stops re-proposing.** A dedupe key catches a *repeat*; only the premise gate
catches an ask that was never *warranted*. "Set the deadline to Aug 31" on a project whose deadline
already *is* Aug 31 is well-formed, has a stable key, and is pure noise.

⭐ **A question is `ChangeFillsSlot`, never `ChangeScalarSet`.** A question asking for a value carries
an empty `Input` by construction — so under `ScalarSet` the empty-value rule fires first and always
mints, and **the agent re-asks a question the user already answered.** A proposal's premise is its own
effect ("does the field already equal *my* value?"); a question's premise is whether the slot is still
empty.

⭐ **Two consequences you get for free.** These are derivations, not conventions — you cannot forget
them:

- **An empty value always mints.** The user fills it in on accept, so the add-set is undetermined and
  the state cannot be a fixpoint of it. This is why *"set the due date — you pick"* is always shown
  while *"set it to Aug 21"* is skipped when it already is.
- ⚠️ **An empty multi-field operand means the opposite.** There it means "not being set", so it is
  *ignored*. Same symbol, opposite meaning, because the add-set differs. Never force a multi-field op
  into `ChangeScalarSet`.

⚠️ **`ChangeMultiField` must use `AddMultiField`**, whose reader takes the operand name, so each field
is compared against *its own* current value. One reader shared across N operands compares every field
to a single string and silently reports "no change" when a field genuinely changed — a fail-closed drop
on a check whose whole contract is fail-open. `Validate` rejects the mispairing in both directions.

⭐ **Comparison is type-aware.** A `date` operand compares as a date, so `"2026-08-21"` and
`"2026-08-21T00:00:00Z"` are the same value; a `number` compares numerically. A raw string comparison
would re-mint the same proposal on every run, forever.

**Checklist**

- ☐ category picked
- ☐ `Input` names a **real** operand (a typo here mints forever)
- ☐ reader supplied, unless the category is `AlwaysMints` or `Merge`
- ☐ ⚠️ `AddMultiField` used for multi-field
- ☐ `Validate()` called at wiring time
- ☐ `VerifyActionSet(t, set)` in your package test
- ☐ ⭐ `VerifyActionSetVocabulary(t, set, oracle)` — **enum drift silently makes your agent reject
  valid model output**, and every other check stays green while it does
- ☐ `VerifyActionSetAgainst(t, set, fixpoints)` — the assertion that needs real state

⚠️ **If your backend already owns the premise gate** — it enforces "would this change state?"
server-side and unbypassably — you do **not** need agent-side state readers; they would be dead
production code. Use `VerifyActionDeclarations` instead of `VerifyActionSet`. It runs every declaration
check without requiring them.

---

## Task: pick a loop

```
Needs to look things up before deciding? ──yes──> do any of those tools MUTATE?
        │no                                             │yes──> RunToolLoopTiered
        │                                               │no ──> RunToolLoop
        ▼
Second opinion has real signal (a rules check, a test run)? ──yes──> Pair + Verifier
        │no
        ▼
Typed result? ──yes──> RunOnce      ──no──> RunOnceText
```

All five are the same atom underneath. Learn `RunOnce` and the rest follow.

---

## Samples

Full runnable source: [`example_doc_test.go`](../example_doc_test.go). Run `go test ./...`.

The blocks below are **adapted** from the named `Example` functions in that file, which CI builds and
runs — adapted, not extracted, so they may be lightly trimmed for reading. The `Example` function named
beside each one is the version under test; when a block and its Example disagree, the Example is right.

**Declare and gate** (`ExampleGateProgram`) — the core pattern:

```go
caps := []agentkit.Capability{{
    Name: "set_due_date", Tier: agentkit.TierReversible,
    Description: "Set a task's due date. Use when the user states a deadline.",
    Args: []agentkit.CapabilityArg{{Name: "due", Type: "date", Required: true}},
}, {
    Name: "send_email", Tier: agentkit.TierIrreversible, // never auto-runs
    Description: "Email the owner. Only when explicitly asked.",
    Args: []agentkit.CapabilityArg{{Name: "to", Type: "string", Required: true}},
}}

program := []agentkit.ToolCall{
    {Name: "set_due_date", Args: map[string]any{"due": "2026-08-21"}},
    {Name: "send_email", Args: map[string]any{"to": "dana@example.com"}},
}

plan, err := agentkit.GateProgram(caps, program, agentkit.RungAuto)
// plan.Apply   = [set_due_date]  → your code runs these
// plan.Propose = [send_email]    → route to a human; unreachable from here
```

**Reject an invented call** (`ExampleVerifyCall`):

```go
err := agentkit.VerifyCall(caps, agentkit.ToolCall{Name: "delete_everything"})
// err != nil — the model invented an opcode
```

Other compiled examples worth reading in full:

| Example | Shows |
|---|---|
| `ExampleVerifyCall_missingRequiredArg` | a well-named call rejected for a missing required arg |
| `ExampleRenderCapabilities` | generating the prompt's tool list — never hand-write it |
| `ExampleToolCall_ArgInt` | the accepted-type behaviour described above |
| `ExampleRulesVerifier` | returned strings are fed to the model on its next turn, so it can fix the problem rather than merely fail |
| `ExampleMaxRungFor` | the tier → rung mapping |
| `ExampleActionSet` | a hand-built `ActionSet` with a premise gate |
| `ExampleToolSet` | three tiers declared, gated, and applied |
| `ExampleBind` | binding against a contract source |

---

## Testing your agent

### For an `ActionSet`, a test kit ships

One call checks the whole formal contract:

```go
func TestMyAgentActions(t *testing.T) {
    agentkit.VerifyActionSet(t, myagent.Actions())                     // the whole contract
    agentkit.VerifyActionDeclarations(t, set)                          // the reader-free half
    agentkit.VerifyActionSetVocabulary(t, set, oracle)                 // ⭐ enum drift
    surviving, dropped := agentkit.SimulateTurns(set, state, actions)  // "what would I ask?"
}
```

`FakeState` (`action_harness.go:48`) gives you state with no database, and `DropReason`
(`action_harness.go:404`) explains why an action was filtered out — use it rather than indexing
`dropped[opcode]` directly.

### For the LLM caller, hand-roll a fake

No fake caller ships. It is one method, so the fake is a few lines — and the patterns below are all
taken from tests in this repository, which you can read and copy.

**A `StructuredCaller` returning one fixed response** — the simplest case, the shape used throughout
[`llm_test.go`](../llm_test.go):

```go
type fakeCaller struct{ resp string }

func (f fakeCaller) CompleteJSON(context.Context, agentkit.StructuredRequest) (string, error) {
    return f.resp, nil
}
```

**A `TextCaller` fake** for a `RunOnceText` agent. Note the different method and types — see
`fakeCaller` in [`example/example_test.go`](../example/example_test.go):

```go
type fakeTextCaller struct {
    text string
    err  error
}

func (f fakeTextCaller) GenerateText(context.Context, agentkit.GenerateTextRequest) (agentkit.LLMCallResult, error) {
    if f.err != nil {
        return agentkit.LLMCallResult{}, f.err
    }
    return agentkit.LLMCallResult{Text: f.text}, nil
}
```

**A scripted multi-response caller** — the one you need for `Pair`, `RunToolLoop` and
`RunToolLoopTiered`, because those call the model more than once and you want a *different* answer per
round. Keep a queue and an index; default the overflow to a terminal answer so a test that runs long
ends rather than panicking. `scriptedStructuredCaller` in
[`example/toolloop_test.go`](../example/toolloop_test.go) is exactly this, and
`scriptedReminderCaller` in [`example/tiered_test.go`](../example/tiered_test.go) is the tiered
variant:

```go
type scriptedCaller struct {
    responses []string
    round     int
}

func (c *scriptedCaller) CompleteJSON(context.Context, agentkit.StructuredRequest) (string, error) {
    if c.round >= len(c.responses) {
        return `{"done":true}`, nil    // overflow ends the loop rather than hanging
    }
    r := c.responses[c.round]
    c.round++
    return r, nil
}
```

⚠️ It must be a **pointer** receiver, and you must pass `&scriptedCaller{…}` — a value receiver
silently replays round zero forever, and your round-cap test then passes for the wrong reason.

### Asserting loop behaviour

| What you want to assert | How |
|---|---|
| the loop converged | `res.Outcome == agentkit.OutcomeApplied`, and check `res.Result` |
| the model chose to do nothing | `res.Outcome == agentkit.OutcomeSilent` — this is **success** |
| a stalemate was recorded | `res.Outcome == agentkit.OutcomeStalemate`; the executor's output is in `res.LastResult`, never the judge's |
| the round cap held | script more rounds than `cfg.MaxRounds` allows, then assert `res.Rounds == cfg.MaxRounds` |
| the degeneration guard fired | pass a `SameFunc` and script the same answer twice; expect a stalemate |
| a throttle propagates | have the fake return `agentkit.ErrThrottled` and assert with `errors.Is` |
| **a mutation never executed** | with `RunToolLoopTiered`, assert the tool's call counter stayed at zero **and** that `res.Proposed` holds the call |

[`loop_test.go`](../loop_test.go) covers each `Pair` outcome as a named test —
`TestPair_DegenerationShortCircuits`, `TestPair_BudgetExhaustedEndsAtExecutor`,
`TestPair_AlwaysRejectingJudgeCannotLoopForever` and `TestPair_UnsetMaxRoundsDefaultsToTwo` are the
four worth reading before writing your own. For the tier guarantee,
`TestRunToolLoop_NeverExecutesIrreversibleLive` in [`tier_test.go`](../tier_test.go) shows the
assertion that matters: the tool's counter, not just the result.

⚠️ **Assert the counter, not only the outcome.** "The result contains a proposal" is satisfied by a
loop that proposed the call *and also ran it*. Only a call counter on your fake tool proves the
mutation did not execute.

Beyond the loops, assert for yourself that every declared capability has a handler and vice versa
(or just call `Validate`), and that no irreversible capability ever reaches `Plan.Apply`.

---

## Maturity — what carries production traffic

Every symbol in this reference compiles and is unit tested. That is not the same as being proven, so
this table says which ones carry production traffic in the agents the kit was extracted from — because
"does anyone actually run this?" is the question a feature list cannot answer.

Use it to decide what to build on: a ✅ row is load-bearing somewhere real; a ⚠️ row is a validated
primitive whose first production consumer might be you.

| Symbols | Status |
|---|---|
| `RunOnce` · `Pair` · `RunToolLoop` · `RunToolLoopTiered` | ✅ production — the load-bearing loops |
| `ToolSet` · `GateProgram` · `VerifyCall` | ✅ production, in a capability-declaring agent |
| `Bind` / `ActionSet` · `ChangeFillsSlot` · `AskKey` | ✅ production, in two independent agents |
| `Turn` / `NewProposal` / `NewQuestionVia` | ✅ production in one agent — ⚠️ read the caveat below first |
| `Narrator` | ✅ production, in one agent |
| `AgentRuntime` / `ActorStore` / `Lease` / `Inbox` | ✅ production behind a Postgres implementation; the in-memory one ships here |
| `CapabilitiesFrom` · `SimulateTurns` · `VerifyActionSetVocabulary` | ⚠️ validated primitives, no production consumer yet |

⚠️ **The `Turn` caveat: it is a shape declaration, not a carrier.** Measured on the one migration that
adopted it, a `Turn` is filled in on one line and destructured on the next, and it supplies **5 of the
13** wire fields (`ObjectType`, `ObjectId`, `Question`, the opcode, and the dedupe key) plus slots. The
rest are hand-filled by the agent's own mapper. Adopt `Turn` for the question-versus-proposal
distinction and for identity — not expecting it to carry your request.

⭐ **What `Turn` did earn there:** its internal validation caught a live bug the agent's own guard
missed. A whitespace-only question became a blank ask that *also* suppressed the fallback, so a bare
capture got no question at all — the agent's `q == ""` check let `"   "` straight through.

### A premise the same migration disproved

The design claimed that once dialogue was a kit primitive, an agent would stop **declaring** an "ask"
capability, and that five declaration sites in one ops agent would be deleted. **Measured: one of five
died.** The split the design missed:

| The ask originates in | Needs a declared capability? |
|---|---|
| **code** — the tier gate withheld an act | ❌ no, and it never did. The premise holds here |
| ⭐ **the model** — it judged that it needs the user | ✅ **yes** — it is emitted as a name in the one verified `calls[]` list |

Delete the spec and `GateProgram` rejects the whole program as an unknown capability, losing the
verdict, the draft and the tags with it. So: **while the model's only output channel is a verified
`calls[]` list, asking must be declared.** Removing those sites needs a separate emission channel —
something like `{"calls": [...], "turn": {...}}` — which is [not built](#not-built).

---

## Not built

Honest gaps. Read this before assuming a feature exists or rebuilding one that ships.

| Missing | Consequence |
|---|---|
| **Turn delivery** | The kit *builds* a `Turn` but never delivers it. A database row, an MCP call, a webhook — that is always yours. It also does not map a `Turn` to your wire format. |
| **The reply path and wake-up** | A turn ends the run, so something must wake the agent when the reply lands. `AgentRuntime` (`runtime.go:25`) is the interface; the durable inbox belongs to the runtime binding. An **unhosted** agent can build a turn but has nowhere for a reply to go. |
| **A user-initiated entry point** | *"Tell the agent…"* with no open turn. Today the only way in is attached to an open proposal. |
| **Multi-tool loops** | `RunToolLoop` and `RunToolLoopTiered` each take **one** `Tool`. Two tools means a discriminated-union `Args`, which you write yourself. |
| **A rules-only reflect loop** | `Pair` requires a `Verifier`, and ⚠️ **a verifier error discards the verdict** (`loop.go:164`) — the executor's result is surfaced as `LastResult` for diagnostics but never applied, because the kit never ships an unverified result. For local rules plus a re-prompt, hand-roll `RunOnce` in a bounded loop. |
| **An `agenttest` package or eval harness** | No fake caller and no eval harness ships. ⚠️ *Partially wrong:* `VerifyActionSet`, `SimulateTurns` and `FakeState` **do** ship — but they cover `ActionSet`s only. See [Testing your agent](#testing-your-agent). |
| **A bundled provider adapter** | The kit declares the LLM seam (`llm.go`) but ships no OpenAI, Anthropic or Bedrock client — you implement `StructuredCaller`, which is one method. That is deliberate: it is why importing agentkit pulls in **one** external dependency. See [providers.md](providers.md) for copy-pasteable adapters. |
| `ToolRegistry` · `ReadTool` · `AgentMessage` · `EventLog` | Named in older documentation. **These do not exist.** Do not import them. |

---

## Reference implementations

Runnable, in this repository. Read these before writing your own.

| Agent | Shape | Read it for |
|---|---|---|
| [`example/example.go`](../example/example.go) | `RunOnceText` | the simplest useful agent: gather → one prose call → record |
| [`example/toolloop.go`](../example/toolloop.go) | `RunToolLoop` | a research loop: call a tool, see results, dig further |
| [`example/tiered.go`](../example/tiered.go) | `RunToolLoopTiered` | the reversibility gate: reads run live, mutations become proposals |
| [`example/cmd/demo`](../example/) | end to end | `go run ./example/cmd/demo` — no credentials needed. It exits non-zero if a withheld delete ever executes |
| [`example_doc_test.go`](../example_doc_test.go) | all of them | every sample in this file, compiled and run in CI |

The kit was extracted from four production agents, each driving a different loop, that between them drove every design decision
recorded here: a capability-declaring ops agent (`RunOnce` plus `GateProgram`), a planning agent
(`Pair` with an LLM judge over a 17-field typed verdict), a retrieval agent (`RunToolLoop`, reflecting
on results), and a digest agent (`RunOnceText`). Where a comment in this repository says "a real bug"
or "measured", that is where the measurement came from.

---

## How symbols are named

A symbol's name should tell you what you are obliged to do with it, before you read its documentation.

| Kind | Your obligation | Signal in the name |
|---|---|---|
| **Data model** | build it, or receive it and read its fields | suffix `Config` · `Result` · `Info` · `Spec` · `Ref` |
| **Seam** — an interface you *implement* | write a type with these methods and inject it | ⚠️ an agent noun (`-er`/`-or`) — not yet consistent |
| **Constraint** — an interface your *output* satisfies | add a method to your own verdict type | ⚠️ `Material` is the only one, and it reads like a seam |
| **Registry** — the kit holds it and dispatches through it | construct it, `Add` in one block, `Validate` at wiring | suffix `Set` |
| **Harness** — meant for `_test.go` | call it from a test; do not put it on a production path | prefix `VerifyAction…`, or `TestingT` |

⚠️ **`Verify` alone does not mean test-only.** `VerifyCall` (`capability.go:284`) is production code on
the safety path — `GateProgram` calls it on every model-emitted call, and `ActionSet.VerifyAction`
(`action.go:353`) is likewise a runtime gate. Only the `VerifyActionSet`, `VerifyActionDeclarations`,
`VerifyActionSetVocabulary` and `VerifyActionSetAgainst` family is the test harness, and it identifies
itself by taking a `TestingT`. **If it takes a `TestingT`, it is a harness; otherwise it is a gate.**

⚠️ **Other exemptions, stated rather than hidden.** `Tier` (an int), `Handler` (a struct, following the
`http.Server` idiom), and `StateReader`/`FieldReader` (func types) all carry `-er` without being seams.
They are pinned by tests or by Go convention and are not worth renaming. New symbols should follow the
table.

⚠️ **One vocabulary overlap, half resolved.** `CapabilityArg` is now an **alias** for `Operand` —
they were structurally identical, the compiler said so, and keeping two copies had already cost a
real bug (`List` was added to one and not the other, so a multi-valued operand was offered to the
model as a single choice and then rejected when the model correctly emitted an array). One name, one
shape; `CapabilityArg` stays so existing code compiles.

⛔ **What is NOT merged: `Capability`/`Action` and `ToolSet`/`ActionSet`.** These remain two
vocabularies for one idea, because they differ in a way the names do not carry — an `ActionSet`
declares what an agent may **propose**, a `ToolSet` declares what it may **do**. A reader searching
for "how do I declare what my agent can do?" still finds two answers. [Choosing a
constructor](#choosing-a-constructor) is the rule until they merge.
