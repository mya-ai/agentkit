# Proposals: an ask is a function of state

Why agentkit mints a proposal only when applying it would actually change something, why a dedupe key
cannot decide that, and how you declare the shape of an effect instead of hand-writing a comparator.

Symbols: `ActionSet`, `Action`, `ChangeCategory`, `Bind` / `BindMixed`, `WillChange`, and the older
`ChangeEvaluator` / `FilterNoOpProposals` seam — see [reference.md](../reference.md) for signatures.

## The failure this exists to stop

An agent proposes *"I'll add your ticket to this queue — approve."* The user approves. The ticket is
now in the queue. Next run, the agent proposes it **again**, because nothing checked whether applying
the op would change anything.

On a development environment this produced **five identical `add_to_queue` asks for one ticket** —
four accepted, one still pending — each re-minted after the last was applied.

The dedupe key did not stop it, and could not have. The backend's idempotency index is scoped to
pending rows, so once an ask is accepted it leaves the index and the next identical ask inserts fresh.

⭐ **A string key catches a repeat; it cannot catch an ask that was never warranted.** "Set the
deadline to Aug 31" on a project whose deadline already *is* Aug 31 is perfectly well-formed, has a
stable key, and is pure noise. The same class covers every setter: proposing a status the object
already holds, a due date it already has.

## The rule

**Mint a proposal if and only if its op would change state.** Before persisting, ask one question of
the leading op against current state: would applying this change the target? If not, the op is the
identity — there is nothing to decide — so the ask is skipped. That is a benign no-op, not an error.

This replaces "is this a duplicate string?" with "is this a no-op transition?" — a fact about state
rather than a guess about wording.

The formal frame is a STRIPS-style effect specification. Model each op as `(Pre, Add, Del)`. Applying
it does nothing exactly where the current state is already a fixpoint of its post-state:

```
WillChange(op, args, state)  =  ¬( Add(op,args) ⊆ state  ∧  Del(op,args) ∩ state = ∅ )
```

Equivalently in weakest-precondition terms, an op degenerates to `skip` exactly where its own
postcondition already holds. For a scalar `set X = v` this collapses to `state.X != v`. This is the
standard planning result that an action whose add-effects already hold and whose delete-effects are
absent produces no change — lifted from apply-time to propose-time.

Every category below is a *derivation* of that line, not a rule of thumb. That is why the subtle cases
cannot be forgotten: they are consequences.

## Declare the effect; the kit derives the rule

You state what the action does to state — its `Category`, plus which operand carries the value — and
the kit generates the comparator. You supply only the one thing the kit cannot know: the current
value.

```go
actions := agentkit.NewActionSet[myState]().
    Add(agentkit.Action{
        Opcode:   "set_due_date",
        Tier:     agentkit.TierReversible,
        Category: agentkit.ChangeScalarSet,
        Input:    "due",
        Operands: []agentkit.Operand{{Name: "due", Type: "date", Required: true}},
    }, func(_ context.Context, s *myState, a agentkit.Action) (string, error) {
        return s.Task(a.Arg("task_id")).DueDate, nil
    })
```

⭐ **Choosing a category is a classification, which is a far easier judgment than writing a
comparator — and a wrong choice is caught by a test, where a wrong comparator is silent.**

The state reader receives the **act**, not just the state, so it knows which object to read. Without
that, a reader could only consult ambient state, and an agent whose run touches several tasks had to
thread a mutable "current target" field — forget to set it and you compare the wrong object, which
fails *closed* and silently drops a legitimate ask.

## The categories

| `Category` | Shape | Derivation |
|---|---|---|
| `ChangeAlwaysMints` | a create, or feedback such as a question with no state effect | `Add` is undefined, so there is no fixpoint. No reader needed. |
| `ChangeScalarSet` | sets one field to one value | `Add = {X = v}`, so fixpoint iff `s.X == v`. An **empty** proposed value always mints (see below). |
| `ChangeMembership` | a link or join | `Add = {linked(a,b)}`, so fixpoint iff already linked. |
| `ChangeTerminal` | moves an object to a final state (archive, complete) | fixpoint iff already in a declared `TerminalStates` value. |
| `ChangeMultiField` | writes several independent fields | changes iff **any provided** field differs. Register with `AddMultiField`. |
| `ChangeFillsSlot` | a question asking the user to supply a value | premise is whether the slot is still empty: mint iff `s.X == ""`. |
| `ChangeMerge` | a metadata merge whose post-state is not modelled | conservatively always-changing in v1. No reader needed. |
| `ChangeUnset` | ⛔ the invalid zero value | a missing declaration is a wiring bug and errors. |

### Why the zero value is invalid

`ChangeUnset` exists so a forgotten `Category` cannot be a silent default. The zero value used to be
`ChangeAlwaysMints`, so omitting the field classified the act as "no computable post-state, therefore
always mint" — and the proposal **re-minted forever**, which is the exact defect this machinery exists
to close.

Worse, the verification harness then discarded the developer's own contrary intent: it skips any
category needing no state reader, so supplying a fixpoint state — the act of saying *this must not
mint here* — was silently ignored for precisely those acts.

This is the same argument the kit makes for `TierReadOnlyExplicit`: "this always asks" must be a
statement, never an omission. Unlike `Tier`, `ChangeCategory` is pinned to no wire contract, so the
zero value itself could simply be made invalid. `WillChange` returns an error on it rather than
falling through, `Validate` rejects it at wiring time, and `Bind` calls `Validate`.

### The empty-value rule, and why two categories disagree about it

For `ChangeScalarSet`, an empty proposed value means the user fills it on accept — so `Add` is
undetermined and the state cannot be a fixpoint of it. **Always mint.** This is not a special case; it
is the category's definition completing. Comparing an empty value would either error (an empty string
cast to a timestamp) or wrongly match, silently dropping a titleless ticket's "set the title — you
pick" ask.

⚠️ For `ChangeMultiField` the same symbol means the opposite. An empty field means *not being set*, so
it contributes nothing and is ignored. Same symbol, opposite meaning, because the `Add` set differs —
which is exactly why forcing a multi-field op into `ChangeScalarSet` would be provably wrong.

`ChangeMultiField` must be registered with `AddMultiField`, which takes a `FieldReader` receiving the
operand name, because each provided field is compared against **its own** current value. Sharing one
reader across N operands compares every field to the same string and silently reports "no change"
whenever the other provided fields happen to equal it — a fail-closed drop on a check whose whole
contract is fail-open. `Validate` rejects a multi-field action registered via `Add`.

### Why questions need their own category

⭐ `ChangeFillsSlot` exists because two correct rules collide.

A question asking for a value carries an empty `Input` by construction — that is what makes it a
question. Under `ChangeScalarSet` the empty-value rule fires first and always mints, leaving the
fixpoint check unreachable for exactly the shape most in need of de-duplication. The user answers, and
the agent asks again next run.

```
ChangeScalarSet  (a PROPOSAL): the value is empty  → the user picks   → ALWAYS MINT
ChangeFillsSlot  (a QUESTION): the field has a value → already answered → NEVER ASK
```

⚠️ The value a question carries is irrelevant here. A proposal's premise is "does the field already
equal *my* value?"; a question's is "does the field have *any* value yet?" — because the user, not the
agent, supplies it. See [dialogue.md](dialogue.md) for the rest of the never-ask-twice contract.

### Terminal states, and the trap in them

`ChangeTerminal` compares case-insensitively and trimmed, deliberately. A status arrives as either an
upper-case lifecycle value (`ARCHIVED`) or a lower-case workflow-state name (`archived`), and a
case-sensitive comparison would silently never fire — re-minting an ask for an already-terminal object
forever.

⚠️ Where archival is a soft delete via a timestamp rather than a status value, the reader must read
that timestamp and surface it as the archived status, or the check never fires at all.

## Fail open, always

⚠️ A state-read error returns `WillChange = true` and the ask is minted. **An ask wrongly shown is
recoverable; an ask wrongly dropped is silent.** The gate only ever removes provably-moot asks, which
is also why partial adoption is safe — a reader you have not written yet costs you nothing but noise.

An *undeclared action* is different and returns an error. Silently minting there would hide the
invented-opcode case that verification exists to catch.

There are two checks, and they fail in opposite directions on purpose:

| Check | Question | On uncertainty |
|---|---|---|
| `VerifyAction` | is this opcode declared, are its operands real and present? | fails **closed** |
| `WillChange` | would applying it actually change anything? | fails **open** |

## Where the check lives

The kit never learns what a "queue" is — it imports no backend. So it owns the **rule** and the
**seam**, and you supply the **fact**. This mirrors how capabilities work: the kit decides the rule,
the consumer supplies the state read.

⚠️ **A gate that lives only in the agent is bypassable** — by a replay, by a second agent, by a future
caller. The durable-state owner should enforce the same question at its own write path. The kit gives
you the law and the seam; your backend enforces it where nothing can route around it.

Coverage is enforced rather than hoped for. `VerifyActionSet` and its companions in
`action_harness.go` walk your real action set and fail the build unless every action declares a
`Category` — either a real state test or a deliberate always-mint. A new mutating op cannot silently
fall through to "always true", which is the bug this whole mechanism kills.

## Consequences and limits

- A whole class of stale asks becomes structurally impossible to create. The five-duplicate bug cannot
  recur, and every setter-to-the-current-value ask is dropped silently.
- **The dedupe key is demoted, not removed.** It still refreshes a live pending row in place when an
  ask is reworded. It is simply no longer load-bearing for "do not re-create an applied effect" —
  state is.
- A hand-written comparator is the anti-pattern here. It rewrites the same boilerplate per op and
  loses the empty-value rule to whoever forgets it.
- **Merge ops are conservative in v1.** Metadata merges are treated as always-changing, so they never
  wrongly drop but may occasionally mint a no-op ask until a subset comparison lands. Tightening this
  later changes no consumer.
- **Resolve-then-compare ops stay conservative.** An op whose operand may create or resolve a row
  before comparing — a name that might mint a new record, or match an existing id — is treated as
  always-changing.
- The candidate pool feeding an agent should be state-derived too, excluding items already adopted, so
  one never re-enters the candidates. That is the same principle applied one layer up.

⚠️ **Do not add reads here to pre-empt the server.** This is a pre-check. The backend's own gate is
the authority.

## The other end of a proposal's life

This gate stops a moot ask being *minted*. The companion problem — retiring a pending ask once its
premise dies, because the target became terminal or the state answered the question on its own — is
the withdraw-time twin. Together they bracket a proposal's life: do not create one the state already
answers, and retire one the state later answers. Both rest on the same idea, that a proposal is a
function of state rather than a string to dedupe.

## A note on the neutral `Proposal` type

The kit also ships a neutral `Proposal` type with `Build`, `DeriveArchetype`, `DedupeKey`,
`MaxProposals`, and the older `ChangeEvaluator` / `FilterNoOpProposals` seam. It is real, tested code,
and `ActionSet` is the preferred declarative path in front of it.

⚠️ Worth knowing before you build on it: **both production consumers declined the neutral type** and
build their backend's proposal type directly. The kit will not import a backend proto, so the neutral
type costs a per-consumer mapper. It is a seam consumers turned down, not one awaiting adoption.

## See also

- [architecture.md](architecture.md) — the design principles this sits inside
- [dialogue.md](dialogue.md) — asking a human, and never asking the same thing twice
- [building-an-agent.md](../guides/building-an-agent.md) — declaring a proposable action, step by step
- [reference.md](../reference.md) — signatures and the testing harness
