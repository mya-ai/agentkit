# Design: dialogue

How an agent speaks to a human, and why the kit owns so little of it. Symbol signatures and anchors
live in [reference.md](../reference.md); this page is the reasoning, including what was designed,
measured, and then rejected.

## Two kinds of turn

The feature is *dialogue*, not "asks". An ask was only ever one kind of turn. The two kinds are
distinguished by what the turn is **about**:

| | **Question** | **Proposal** |
|---|---|---|
| about | INFORMATION the agent lacks | an **ACT** it may not perform alone |
| carries | the question | the act (an `Op`), plus optional slots |
| the reply | returns as context | approve · reject · **redirect** |

Both kinds are terminal: the run ends. The agent does not proceed without an answer — it stops and
waits to be woken, which is what makes dialogue asynchronous rather than blocking. A memo is **not** a
turn: a dialogue is two-way by definition, while a memo is a metadata write with no identity, no
dedupe, no reply path and no wake.

## Asking is not a capability

⭐ **The model emits acts. It does not classify them.** It emits `send_email`, never
`propose(send_email)`. `GateProgram` sorts by tier. Any prompt text teaching the model what a
"proposal" is, is machinery leaking upward.

Two paths reach a human, and the model emits identically in both:

| Path | Decided by |
|---|---|
| the agent judged that asking is the right act | the **model** |
| the act's tier exceeded the ceiling | **code** (`GateProgram` → `Plan.Propose`) |

A question is about information: no act, so no tier, so nothing for the gate to withhold. That is why
speaking to the user should not be declared as a capability alongside `send_email`. `RenderDialogue`
is how the model learns the option exists, since dialogue is deliberately absent from
`RenderCapabilities` — "not a capability" is about the **gate**, not the **prompt**.

### ⚠️ That is the target, and the code does not enforce it

Nothing in the kit stops you declaring an `ask` capability today, and the one adopting consumer still
declares one. The reason is structural, and it was measured rather than argued.

**Migrating one production ops agent onto `Turn` deleted 1 of its 5 asking sites** — and that one was
killed by ordinary refactoring, not by `Turn`. The prediction had been that roughly eight sites across
two agents would vanish. It missed **who originates the ask**:

| Origin | Must it be a declared capability? |
|---|---|
| **CODE** — the tier gate withheld an act | ❌ no, and it never was. The rule holds exactly here |
| **MODEL** — it judged it needs the user | ⭐ **yes** — it is emitted as a NAME in the one verified `calls[]` list |

Probed, not reasoned: delete the ask capability's spec and `GateProgram` returns `unknown capability`
and discards the **whole program** — the verdict, the draft, the tags and the ask all go with it. A
value type constructed *after* gating cannot delete a site that exists *before* it.

⇒ While the model's only output channel is a `calls[]` list verified against declared specs, a
model-initiated ask **must** be a declared capability, and code must pick it out of that list by name.

⭐ **What would actually collapse the two paths is a separate emission channel** — the model returning
`{"calls": [...], "turn": {...}}`, one more field on the verdict struct, so speech never enters the
act list at all. That is a real design, and it is **not built**: it changes what the model must learn,
so it is eval-first. Do not re-attempt the site deletions without it.

## What the kit does not own

| Not owned | Why |
|---|---|
| an "ask" capability | its `Description` is doctrine. Two production agents' ask specs carry incompatible doctrine — neither could use the other's |
| the question wording | product voice |
| slot **derivation** | slots key on the **op's** operands, and the kit must not know the backend's opcodes |
| the wire format | tenant, surface, source and metadata are fields on *your* backend's type |
| **delivery** | permanently the consumer's. The kit builds a turn; it does not deliver one |

**The test any addition must pass:** does it require knowing what a "project", an "opcode", or a
proposals row is? If yes, it is the agent's.

### ⚠️ Slots key on the OP's operands, not the capability's args

Two vocabularies point in opposite directions:

| | Filled by | Verified against |
|---|---|---|
| **Capability arg** (`ToolCall`) | the MODEL | the agent's declared specs, by `VerifyCall` |
| **Op operand** (`Op`) | the HUMAN, at accept time | the backend's own op registry — the authority |

`Op` is deliberately not a `ToolCall`. In one production agent the delivered slot is named `answer`
(the op's operand) while the capability's arg is `question`; naming a slot after the capability arg
gets it dropped at verify. The kit cannot derive slots — it would need the backend's op registry,
which it is forbidden to import — so slot construction is the agent's, permanently.

Two slot modes exist and only the agent knows which applies: **input** (a required operand with no
value — *"what's your target date?"*) and **draft** (a filled value offered back as editable via
`Default` — *"I'd set Aug 31, approve or edit"*). Draft mode is not derivable; "worth offering back
rather than just applying" is a product decision. `DeriveArchetype` reads the mode back out of the
shape, so it cannot drift from what the proposal actually asks.

### A question often needs an op anyway

The kit models question-versus-proposal as absence-versus-presence of an act. Real backends model it
as *which opcode*: a backend may have exactly one channel for "ask the user and route the answer
back", and that channel is itself an op. So a pure question needs an opcode to be deliverable — but
`NewQuestion` rejects an act, and `NewProposal` would show an approve/reject affordance for something
that is not an act.

**Both production consumers had to violate the invariant to use the type.** Two of two is the kit
being wrong about the world, not the consumers being unusual. Hence `NewQuestionVia`: the op is a
transport detail, `Kind` stays `TurnQuestion`, and the reply is an answer, never an approval.

## Identity: never ask the same thing twice

An agent that re-asks is the failure a user feels most — the same decision piling up on their board
every run. Two mechanisms answer **different questions**, and only one of them is about identity:

| | the premise gate (`WillChange`) | the dedupe key |
|---|---|---|
| asks | *would this op CHANGE state?* | *did I send this exact ask before?* |
| catches | an ask with no premise (the value is already set) | a re-send of the same ask |
| misses | nothing about warrant | ⚠️ an ask that was never warranted — it passes the key check cleanly |

⭐ **An ask whose premise is already satisfied is not a duplicate to collapse — it should never have
been minted.** The key would happily persist it, once. In one measured case a single item produced
**five identical add-to-collection asks** (four accepted, one still pending), each re-minted after the
last was applied: the backend's idempotency index is scoped to *pending* rows, so an accepted ask
leaves the index and the next identical one inserts fresh. A key cannot see an effect that has already
been applied.

The premise question is a declaration, not a hand-written comparison. You declare the action's
`ChangeCategory` — the shape of its effect — and `ActionSet.WillChange` is generated from the
state-transition law; your reader supplies only the current value. Deriving it makes the subtle rules
*consequences* rather than conventions somebody must remember: an empty proposed value leaves the
effect undetermined, so the state cannot be a fixpoint of it, so the ask always mints. That is why
*"set the due date — you pick"* is always shown while *"set it to Aug 21"* is skipped when it already
is.

⚠️ **That derivation caught a real design error.** For a *multi-field* op an empty field means "not
being set" and is ignored — the exact opposite of the scalar rule. Same symbol, opposite meaning,
because the effect set differs; a multi-field update is `ChangeMultiField`, never `ChangeScalarSet`.
`VerifyActionSet` runs the whole contract in one call and `SimulateTurns` answers *"given this state,
which asks would my agent make?"*, so the duplicate-ask class is a unit test, not a production
observation.

### The key excludes the wording

`AskKey` and `Turn.Key` produce `agent:kind:id:op`, deliberately without the question text. A reworded
or freshly tokenized draft of the same ask must **refresh the open row in place**. Keying on the
question — the obvious mistake — gives a better-worded ask a different key, so it accumulates beside
the stale one. A production agent paid for this.

⭐ Two findings about these helpers are worth stating plainly:

- `DedupeKey` produced a key **byte-identical** to one a consumer had hand-rolled for itself
  (`agentName + ":" + objectType + ":" + itemID + ":" + opcode`). The kit had solved it and the
  consumer re-implemented it anyway — because `DedupeKey` takes a `Proposal`, a type no consumer
  builds. **A helper that takes a type nobody has is a helper nobody can call.** `AskKey` is the same
  key over values a consumer already holds.
- `agentkit.Proposal` therefore stays unadopted on purpose: its `Ops []ProposalOp` models a multi-op
  program, and all six construction sites across the two consuming agents are single-op. It
  generalizes something nobody does.

⚠️ **Pass a discriminator whenever your opcode is a channel rather than a specific act.** Both
consumers were bitten by its absence from opposite directions: one surfaced every tier-withheld act
through one opcode, so two different acts shared a key; the other rode every open question on one
opcode, so two unrelated questions did. A backend upserting `ON CONFLICT` on that key silently
overwrites the first with the second and the user is never asked. `Turn.Discriminator` is a **field**,
not only a `Key` argument, so identity travels with the value and a generic deliverer can key a turn
from the turn alone.

## A redirect is not a rejection

| Reply | The user means | The act |
|---|---|---|
| `ReplyApprove` | do it | released as proposed — determined |
| `ReplyReject` | don't do it | dropped — determined |
| ⭐ `ReplyRedirect` | *"do something else"* / *"do this, with…"* | **handed back to the agent — not determined** |

⛔ A redirect is not a shaded rejection. The user is not declining; they are handing the agent new
information and returning control. The outcome is not fixed: the agent may keep the act adjusted, drop
it and propose a different one, act with the input applied, or do nothing further.

⚠️ **Why it must never be recorded as a rejection:** a correction-suppression mechanism reading
"rejected" will silence the very re-proposal the user just asked for. The turn is not declined — it is
finished, with a better one coming. Test with `Reply.IsRejection()`, which returns **false** for a
redirect, rather than `!= ReplyApprove`. `Reply.HandsBackToAgent()` names the redirect case directly.

## A real bug: `Plan.Propose` had zero readers

An ask-surfacing function looped `Plan.Apply` only, matching one magic capability name. The agent
declared two propose-tier capabilities; when the model emitted one, `GateProgram` correctly withheld
it into `Plan.Propose`… and nothing consumed it. **The ask never reached the user** — no error, no
log. Exactly the silent-drop class the tier gate exists to eliminate.

Read **both** halves of the `Plan`. Pin it in your own delivery code: the kit builds the turn, but a
silent drop and a double delivery are both properties of how you surface it.

## Rules, restated

1. Never teach the model about proposals — it emits acts, `GateProgram` classifies. A prompt with
   dozens of occurrences of *propos\** is the leak, not the standard.
2. Read both halves of the `Plan`. Slots name the op's operands, never a capability arg.
3. Key on `(agent, object, op)`, never on question text; pass a discriminator for a channel opcode.
4. **Build refs with the labeler.** `TokenizeQuestion` and `Build` exist because four proposals once
   shipped with the byte-identical question *"Here's the goal I'd set"*, naming nothing. ⚠️ An
   `ObjectRef` struct literal with an empty `Label` makes that guarantee silently no-op.
5. **Cap the asks** (`MaxProposals`). Twelve asks is a backlog handed to a human, and nothing is lost:
   the key makes re-surfacing next run free.
6. **The backend's own op verification is the authority**; `VerifyCall` is the agent-side fast reject.
   Adding format validation to `VerifyCall` duplicates the authority on the wrong side of the
   boundary. Layer, don't copy.

## Not built — do not import

| Missing | Consequence |
|---|---|
| turn → wire mapping | `Turn` exists, but the kit does not convert it to your backend's request |
| delivery, and a `Deliverer` interface | own a narrow interface yourself. Not worth abstracting until a second transport exists |
| the reply path end-to-end | the kit defines `Reply` and `AgentRuntime` (the wake); the durable inbox is your runtime binding's. An unhosted agent can build a turn but has nowhere for a reply to land |
| a user-initiated turn | *"tell the agent"* with no open turn has no path in |
| slot derivation | supply slots yourself; the kit cannot know the op registry |
| a neutral `Ask` struct | designed, then **rejected** — see below |

⭐ **The rejected `Ask` struct is the most useful row on that list.** Measured in a real consumer: its
ask-building function is 40 lines, and about 35 are field-filling on the backend's proposal type,
which the kit cannot own. A kit `Ask` type would leave those 35 lines in place *and* add a conversion
step. **Bigger, not smaller** — it failed the shrink test and was not built.

⚠️ Do not confuse it with `AskFor`, which ships (`bind.go`) and is a required field on `Use` for any
withheld act. It is a `func(Action) string` rendering the wording shown to a human: a name collision
with the rejected design, not the design itself.
