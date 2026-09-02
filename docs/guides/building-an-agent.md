# Building an agent with agentkit

This guide takes you from `go get` to an agent that gathers context, reasons in a bounded loop,
and changes something real — with the dangerous half held back for a human.

Each step is something you can run. Work through them in order; the vocabulary is introduced as
you need it.

Looking for a signature or a trap rather than a walkthrough? That is
[the reference](../reference.md).

**What this is for.** An agent triggered by a cron tick, a queue message or an event: it gathers,
reasons, then acts or proposes. A run is stateless — there is no transcript and no streaming, so
turn-by-turn chat is a different shape and this is not it.

---

## Step 1 — see it work

```bash
go get github.com/mya-ai/agentkit
```

Go 1.25 or later. That is the only external dependency you take on: no provider SDK, no queue,
no database.

Before writing anything, watch the central guarantee hold. Clone the repo and run the demo — no
credentials, no network, no setup:

```bash
git clone https://github.com/mya-ai/agentkit && cd agentkit
go run ./example/cmd/demo
```

```
─── read tool (tier: read-only) ───
  executed live:  true (1 call)
  result:         account acct-42: 3 open tickets, payment failed twice

─── delete tool (tier: irreversible) ───
  outcome:        applied (proposed_irreversible)
  executed live:  false
  withheld:       delete_account (tier irreversible)
  its reason:     "This account is failing every health check."
  args a human would approve: {Query: Delete:acct-42}

✅ The model asked for both. The read ran; the delete did not.
   Nothing about the model changed that — the tier did.
```

The same agent, the same model and the same intent produced both halves. The read tool ran; the
delete tool was captured as a proposal and its `Call` method was never entered. The demo exits
non-zero if the delete ever executes, so this is asserted rather than claimed.

The model in that demo is a scripted stub that always asks to delete. That is the point: the
safety property is structural, so it does not depend on the model cooperating. Read
[`example/cmd/demo/main.go`](../../example/cmd/demo/main.go) — it is about 120 lines and it is the
whole mechanism.

## Step 2 — supply an LLM

The kit ships no provider client. It declares the contract it needs and you implement it. For the
structured path that is one interface with one method:

```go
type StructuredCaller interface {
    CompleteJSON(ctx context.Context, req StructuredRequest) (string, error)
}
```

You return the model's **raw text**. Do not parse the JSON — the kit extracts and unmarshals it,
because models wrap JSON in markdown fences and append trailing prose however firmly you instruct
them not to, and that normalization belongs in one place.

Two things to get right, both cheap:

- **Map `req.System` onto the provider's system-instruction slot** and `req.Dynamic` onto the
  user turn. The split is what makes prompt caching effective, since the stable half is identical
  across calls.
- **Wrap `ErrThrottled` on a 429**: `fmt.Errorf("provider throttled: %w", agentkit.ErrThrottled)`.
  It is the one error the kit does not retry or fall back on — it short-circuits so your caller can
  hand the work back to its queue and let the queue be the backoff. Skip it and throttles look like
  parse failures, so the kit burns both primary attempts plus the fallback hop against a provider
  that is already shedding load.

Copy-pasteable adapters for OpenAI, Anthropic and Ollama, plus the precise field-by-field contract,
are in [providers.md](../providers.md).

Generating prose rather than JSON? Implement `TextCaller` (`GenerateText`) instead. You only need
the one your agent actually uses.

For now, a stub is enough to keep moving:

```go
type fakeCaller struct{ reply string }

func (c fakeCaller) CompleteJSON(context.Context, agentkit.StructuredRequest) (string, error) {
	return c.reply, nil
}
```

## Step 3 — choose your loop

Five entry points. They are the same atom underneath — learn `RunOnce` and the rest follow.

| You need | Use |
|---|---|
| one structured verdict | `RunOnce` |
| one piece of prose | `RunOnceText` |
| draft → critique → revise | `Pair` |
| look something up, then decide | `RunToolLoop` |
| …and some tools mutate | `RunToolLoopTiered` |

**How to choose.** Can you answer in one pass from what you already gathered? `RunOnce` (or
`RunOnceText` if the answer is prose). Do you need to look something up before you can decide?
A tool loop. Is there a check that would catch a bad answer — a rule, a test run, a schema? `Pair`.

### RunOnce — the atom

You supply a context type, a result type, and a `Render` that turns one into a prompt.

```go
// What you gathered for this run.
type reviewCtx struct {
	Ticket string
	Age    int
}

// What you want back. The json tags are the contract with the model.
type verdict struct {
	Summary  string `json:"summary"`
	Escalate bool   `json:"escalate"`
}

// IsMaterial is required only by Pair — it is how a run says "nothing to do".
func (v verdict) IsMaterial() bool { return v.Escalate }

type reviewAgent struct{}

func (reviewAgent) Role() agentkit.RoleSpec {
	return agentkit.RoleSpec{Model: "your/model", PromptType: "review", MaxTokens: 800}
}

// system = the stable rules; dynamic = this run's data. Splitting them is what
// makes prompt caching work.
func (reviewAgent) Render(rc reviewCtx, info agentkit.RoundInfo) (system, dynamic string) {
	system = `You triage support tickets. Reply as JSON: {"summary":"…","escalate":true|false}.`
	dynamic = fmt.Sprintf("Ticket %s has been open %d days.", rc.Ticket, rc.Age)
	if info.IsFinalRound() {
		dynamic += "\nThis is your final round — commit to an answer."
	}
	return system, dynamic
}

func Run(ctx context.Context, caller agentkit.StructuredCaller) (verdict, error) {
	return agentkit.RunOnce[reviewCtx, verdict](
		ctx, caller, reviewAgent{}, reviewCtx{Ticket: "T-1024", Age: 9},
		nil /*feedback: Pair threads this; a direct caller has none*/, nil, /*obs*/
	)
}
```

`RoundInfo` tells the executor where it is in the budget so it can pace itself. A standalone
`RunOnce` always gets round 1 of 1.

### RunOnceText — the prose flavor

Same shape, except `Render` takes no `RoundInfo` (a prose call is always one round) and you get a
string back. Use it when the output is markdown or plain text — forcing prose through JSON mode
wraps it in a `{"description": "…"}` envelope.

```go
func (noteAgent) Render(rc noteCtx) (system, dynamic string) {
	return "You write a warm 2-3 sentence morning note in plain markdown.",
		fmt.Sprintf("Timezone: %s. Things on the plate today: %d.", rc.Timezone, rc.ItemCount)
}

note, err := agentkit.RunOnceText[noteCtx](ctx, caller, noteAgent{}, rc, nil /*obs*/)
```

An empty result comes back as-is rather than as an error — you decide whether empty is acceptable.
The full agent is [`example/example.go`](../../example/example.go).

### Pair — draft, critique, revise

`Pair` runs an executor and a judge until the judge approves or the budget runs out. The judge is
a pluggable `Verifier`, which is the important part:

⚠️ **An ungrounded same-model critic can make results worse**, not better (Huang 2024, Kamoi 2024).
A judge only helps when it has signal the generator lacked. Prefer a rules check or a test run;
reach for an LLM critic only when it genuinely adds something.

```go
// A judge with real signal: it checks a rule the executor cannot talk its way past.
var judge = agentkit.RulesVerifier[plan](func(p plan) (ok bool, feedback []string) {
	if len(p.Steps) > 7 {
		return false, []string{"Too many steps — merge related work into at most 7."}
	}
	return true, nil
})

res := agentkit.Pair[struct{}, plan](
	ctx, caller, exec, judge,
	func(a, b plan) bool { return len(a.Steps) == len(b.Steps) }, // degeneration guard
	struct{}{}, agentkit.LoopConfig{MaxRounds: 3}, nil, /*obs*/
)

switch res.Outcome {
case agentkit.OutcomeApplied:
	return persist(ctx, res.Result) // Go owns the write
case agentkit.OutcomeSilent:
	return nil // the model decided there was nothing to do
case agentkit.OutcomeStalemate:
	return persist(ctx, res.LastResult) // the executor's best; never the judge's words
default:
	if errors.Is(res.Err, agentkit.ErrThrottled) {
		return res.Err // release the work back to your queue, don't retry
	}
	return res.Err
}
```

Three things the loop guarantees, which is why you do not write it yourself:

- **It always ends with the executor.** The judge can only gate — approve and stop, or reject and
  buy one more executor turn. The kit never returns, surfaces, or persists the judge's text; there
  is deliberately no field to hold it.
- **A hard ceiling.** `MaxRounds` unset means 2, not 1 — a one-round `Pair` is degenerate, since
  the judge's first rejection would end it with no revision ever happening.
- **A degeneration guard.** If your `SameFunc` says a revision is materially unchanged despite
  specific feedback, the loop stops at `OutcomeStalemate` rather than burning the rest of the
  budget. Pass `nil` to disable it and rely on the ceiling alone.

### RunToolLoop — look something up, then decide

Each round the executor emits a `ToolStep`: either a tool call to run, or `done`. The kit runs the
tool, hands the results back, and the executor grades them and decides whether to dig further.
That reflection is grounded in retrieved data, which is exactly why it is sound where `Pair`'s
ungrounded self-critique is not.

The reference consumer is [`example/toolloop.go`](../../example/toolloop.go): a `Tool[Args, R]`, a
`ToolAgent[Ctx, Args, R]`, and an orchestrator that branches on `ToolLoopResult.Outcome`.

⚠️ **`RunToolLoop` takes exactly ONE tool.** This is the constraint most likely to surprise you,
and it shapes your design, so decide now rather than halfway through.

A `Tool` has one `Name()` and — for the tiered variant — one `Tier()` that takes no arguments. If
your agent needs a search tool *and* a create tool, you have two options:

1. **One tool with a discriminated `Args` type.** Add an `op` field the model fills, and fan out
   inside `Call`. Its `Tier` must then be the tier of its **most dangerous branch**, because
   `Tier()` cannot vary per call — so a router carrying one mutating branch is gated as a mutation
   for every call, including the reads.

   ```go
   type args struct {
       Op    string `json:"op"`              // "search" | "create_reminder"
       Query string `json:"query,omitempty"` // for op == "search"
       Text  string `json:"text,omitempty"`  // for op == "create_reminder"
   }

   func (routerTool) Name() string        { return "act" }
   func (routerTool) Tier() agentkit.Tier { return agentkit.TierExternalReversible }
   ```

2. **Run separate loops**, or use the capability path in step 4 — where the model emits a whole
   program of calls in one structured verdict and you gate them individually. That path handles
   many differently-tiered acts naturally, and it is the better fit once you have more than a
   couple.

There is no registry for mixed read-and-mutate tool sets in one loop. Design around the single-tool
shape rather than expecting one.

### RunToolLoopTiered — when the tool mutates

Identical to `RunToolLoop`, except the tool declares a `Tier()` and the loop routes on it. A tool
it may auto-run is called live; a tool above the ceiling is captured as a `ProposedAction` and the
loop stops. The tool's `Call` is never entered. That is the demo from step 1.

## Step 4 — declare what the agent may do

This is where newcomers get lost, so here is the rule up front.

**There are four constructors, and all four compile.** Choosing wrong therefore fails at review,
not at build. Pick by answering one question: where does the act's contract — its name, its
operands, its tier — come from?

| Where the contract lives | Use |
|---|---|
| your backend serves an op contract | `Bind(ctx, src, uses)` |
| …and some acts are yours alone | `BindMixed(ctx, src, uses, local)` |
| there is no backend contract — you are declaring the acts yourself | `NewActionSet[S]()` |
| the model emits `calls[]` your code must gate and run | `NewToolSet[S]()` |

The first three build an **`ActionSet`**: what your agent may *propose*, where a human blesses it
and a backend op applies it. The fourth builds a **`ToolSet`**: what your agent *does itself*,
gated by tier. Most agents need one; an agent that both proposes and executes needs both — and
in that case declare it once as an `ActionSet` and derive the capability list with
`agentkit.CapabilitiesFrom(set)`, rather than typing the same contract twice.

⚠️ `BindMixed(ctx, nil, nil, local)` is correct even though it reads like a mistake. A `Local[S]`
act carries `Run` and `Ask`, which `ActionSet.Add` cannot take, so `BindMixed` is the only door to
a local act even when there is no backend at all. The `nil, nil` means "I have no backend ops",
not "I forgot the arguments".

⚠️ **Two vocabularies, one idea.** `Capability`/`CapabilityArg` (the `ToolSet` side) and
`Action`/`Operand` (the `ActionSet` side) are structurally near-identical, and the naming will not
help you tell them apart. Go by the question in the table — *propose* versus *do* — and not by
which type name sounds right.

### A ToolSet

Declare the spec and the handler **together**. The capability's name then exists in exactly one
place, so what the model is told and what actually runs cannot drift apart.

```go
tools := agentkit.NewToolSet[State]()

tools.Add(agentkit.Capability{
	Name:        "set_priority",
	Description: "Raise or lower the ticket's priority. Use when the evidence changes urgency.",
	Tier:        agentkit.TierReversible, // may run live: it has an undo
	Args: []agentkit.CapabilityArg{{
		Name:        "level",
		Description: "the new priority",
		Type:        "enum",
		Required:    true,
		EnumValues:  []string{"low", "normal", "high"},
	}},
}, func(ctx context.Context, s *State, call agentkit.ToolCall) error {
	fmt.Printf("ticket %s → %s\n", s.TicketID, call.ArgString("level"))
	return nil
})

tools.AddProposeOnly(agentkit.Capability{
	Name:        "email_customer",
	Description: "Email the customer. Only when they asked to be contacted.",
	Tier:        agentkit.TierIrreversible, // never auto-runs; no handler exists
	Args: []agentkit.CapabilityArg{
		{Name: "body", Description: "the message", Type: "string", Required: true},
	},
})

if err := tools.Validate(); err != nil { // once, at wiring time
	return nil, err
}
```

`AddProposeOnly` takes no handler at all, so an act the tier can never auto-run has no function to
mistakenly call. Without it you would write a handler that must never fire — dead code whose
correctness rests on a runtime invariant instead of the type system.

Then generate the prompt's tool list. **Never hand-write it:**

```go
system := "Return {\"calls\":[{\"name\":...,\"args\":{...}}]} using ONLY these:\n" +
	agentkit.RenderCapabilities(tools.Capabilities())
```

Add or remove a capability and the prompt changes by construction. A hand-written list drifts
silently from the handlers, and a prompt you can override remotely is untestable.

`Description` is written **for the model** — it is the only instruction the model gets about *when*
to use the capability. State the trigger condition. For an enum arg, always fill `EnumValues`:
without them the model invents values and every call fails verification.

`ArgString` and `ArgInt` read args regardless of the JSON type they arrived as, since the same
operand may come back as `20` or `"20"`. Never type-assert an arg yourself.

### An ActionSet

When you declare the acts yourself, you also declare the **shape of the effect** — and the kit
derives the "would this actually change anything?" check from it, so you never write the
comparator.

```go
set := agentkit.NewActionSet[State]()

set.Add(agentkit.Action{
	Opcode:      "set_task_due_date",
	Description: "Set a task's due date. Use when the user states a deadline.",
	Tier:        agentkit.TierReversible,
	Operands: []agentkit.Operand{
		{Name: "task_id", Description: "the task", Type: "string", Required: true},
		{Name: "due", Description: "ISO-8601 date", Type: "date", Required: true},
	},
	Category: agentkit.ChangeScalarSet, // the SHAPE of the effect
	Input:    "due",                    // the operand carrying the value being written
}, func(_ context.Context, s *State, act agentkit.Action) (string, error) {
	// Return the CURRENT value the effect is compared against.
	return s.DueDates[act.Arg("task_id")], nil
})

if err := set.Validate(); err != nil {
	return nil, err
}
```

The state reader receives the act, so it knows *which* object to read — `act.Arg("task_id")`
resolves the target per call. Render the model's list with `agentkit.RenderActions(set.Actions())`,
from the same declarations the state logic hangs off.

Choosing a `Category` is a classification, which is a far easier judgment than writing a
comparator — and a wrong classification is caught by a test, where a wrong comparator is silent.
The categories and their derivations are in [the reference](../reference.md).

## Step 5 — gate the mutations

Every capability carries a `Tier`: how recoverable its effect is. It is a property of the act, not
a policy knob, and it caps how much autonomy that act can ever be granted.

| Tier | Meaning | Can it auto-run? |
|---|---|---|
| `TierReadOnlyExplicit` | a query or lookup, no mutation | yes |
| `TierReversible` | a mutation with a real undo | yes |
| `TierExternalReversible` | an external effect recoverable but not silently (a send, a booking) | **no — proposed** |
| `TierIrreversible` | delete, mass comms, payments, permissions | **no — proposed** |

⛔ **A forgotten `Tier` is REJECTED, not treated as safe.** The zero value of `Tier` is read-only,
which auto-runs — so a missing field would silently declare a destructive act live. Both
`ToolSet.Validate` and `GateProgram` refuse it outright:

```
agentkit: capability "wipe" declares no Tier — the zero value is read-only, which AUTO-RUNS,
so a forgotten field would make a destructive act live. State it: TierReadOnlyExplicit /
TierReversible / TierExternalReversible / TierIrreversible
```

Read-only has to be **opted into**, with `TierReadOnlyExplicit`. "This may auto-run" is always a
statement, never an omission.

### GateProgram

When the model returns a program of calls, gate the whole thing before anything runs:

```go
plan, err := tools.GateProgram(verdict.Calls, agentkit.RungAuto)
if err != nil {
	// The WHOLE program is rejected — an invented opcode, a bad enum, a
	// missing required arg, or a capability that forgot its Tier.
	return fmt.Errorf("gate: %w", err)
}
```

Verification runs first, over the whole program. An invalid call **fails the program** rather than
being quietly dropped — dropping one op of a multi-op write is how you get a half-applied change,
where a dropped slot orphans a required operand elsewhere. Models do invent opcodes and operands;
that is a fact to verify against, not something to prompt away.

Then `plan.Apply` holds the calls the tier permits live, and `plan.Propose` holds the rest.

⛔ **Iterate `plan.Apply`, never the original program.** `verdict.Calls` is ungated data. The whole
guarantee is that a withheld call physically cannot reach the execution path — executing the
original list throws that away.

## Step 6 — apply the result

**Go owns the write.** Every loop in the kit returns a decision and never persists anything. What
happens next is your code:

```go
// Reversible and read-only calls: your handlers run them.
for _, applyErr := range tools.ApplyPlan(ctx, st, plan) {
	fmt.Println("apply failed:", applyErr)
}

// Above the ceiling: draft-and-hold for a human. Never executed here.
for _, call := range plan.Propose {
	if err := draftForHuman(ctx, call); err != nil {
		return err
	}
}
```

`ApplyPlan` runs the handler for each call in `plan.Apply`, in order. It never touches
`plan.Propose` — passing the whole plan cannot defeat the gate. A call whose name has no registered
handler is returned as an **error**, never skipped silently; that silent skip is the bug the type
exists to make impossible.

It returns *every* error and continues past a failure rather than aborting. Losing one write is
bad; losing the writes that would have succeeded after it is worse. A handler returning `nil` means
"performed **or deliberately skipped**" — a business brake like "the user set this date themselves"
is a normal outcome, not an error.

For a tiered tool loop the same split arrives as `res.Proposed`:

```go
for _, p := range res.Proposed {
	fmt.Printf("needs a human: %s (tier %s) args=%+v because %q\n",
		p.ToolName, p.Tier, p.Args, p.Rationale)
}
```

`p.Args` is the exact call the executor intended, so you replay it on approval — through your own
executor, not through the loop. `p.Rationale` is the executor's stated reason, ready for the
approval surface.

One rule that saves you a class of bug: **every field your apply writes, your gather must read
back.** Otherwise the agent cannot see its own effect and re-proposes the same thing every run.

## Step 7 — test it

⚠️ **No test-helper package ships.** There is no `agenttest`, no mock generator, no eval harness.
You do not need one: a `StructuredCaller` is a single method, so a fake is a struct with a script.

```go
// scriptedCaller returns one queued ToolStep per round, then stops. Nothing in
// the kit needs a mock framework — a StructuredCaller is one method.
type scriptedCaller struct {
	steps []agentkit.ToolStep[searchArgs]
	round int
}

func (c *scriptedCaller) CompleteJSON(context.Context, agentkit.StructuredRequest) (string, error) {
	step := agentkit.ToolStep[searchArgs]{Done: true}
	if c.round < len(c.steps) {
		step = c.steps[c.round]
	}
	c.round++
	b, err := json.Marshal(step)
	return string(b), err
}
```

Scripting the model per round makes the loop deterministic, so you can assert on the thing that
matters. The tiered example's test is the shape to copy — it asserts a safety property directly:

```go
if len(tool.created) != 0 {
	t.Fatalf("SAFETY VIOLATION: the mutating tool executed live: %+v", tool.created)
}
```

Real examples in this repo, by name:

- [`example/tiered_test.go`](../../example/tiered_test.go) — the mutation is proposed and never
  executed, end to end through the consumer.
- [`example/toolloop_test.go`](../../example/toolloop_test.go) — a scripted multi-round loop, plus
  the silent-run and error paths.
- [`example/example_test.go`](../../example/example_test.go) — a fake `TextCaller` and a fake store.
- [`llm_test.go`](../../llm_test.go) — a caller that records the exact sequence of models it was
  asked for, which is how the retry-then-fallback contract is pinned.

For the declaration side, the kit ships verification helpers you call from your own test:
`VerifyActionSet(t, set)` checks an entire `ActionSet` for internal coherence in one line;
`VerifyActionSetVocabulary(t, set, truth)` checks your declared enums against what the backend
really accepts — worth doing, because a drifted enum makes your agent silently reject values the
backend would have taken. See [the reference](../reference.md) for the full set.

To watch a loop run against a real (or fake) caller without any queue or apply wiring,
`RunLocalOnce` and `RunLocalPair` print each round to a writer you pass. They are a throwaway
`main()` away.

---

## Where to go next

| You want | Read |
|---|---|
| a signature, a trap, a compiled sample | [reference.md](../reference.md) |
| an adapter for your provider | [providers.md](../providers.md) |
| why it is shaped this way | [design/architecture.md](../design/architecture.md) |
| how one agent instance per unit is kept single-writer | [design/agent-runtime.md](../design/agent-runtime.md) |
| three reference agents, wired end to end | [example/](../../example/) |
| what gets a pull request rejected | [contributing.md](../contributing.md) |
