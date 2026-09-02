# Notes for coding agents

For AI coding agents working in this repository or writing code that imports it. Humans should read
[README.md](README.md) first; nothing here is secret, it is just addressed to a different reader.

## Read this before answering an API question

**[docs/reference.md](docs/reference.md) is the entry point, not the source.** It is written to be
grepped: lookup tables first, rules second, runnable samples last. It maps every exported symbol to
`file:line` and a signature, plus the traps a signature cannot show.

**The `file:line` anchors are trustworthy.** CI asserts not only that each anchor resolves but that
the line holds the symbol the row names (`TestREADMEAnchorsResolve` in
[readme_anchors_test.go](readme_anchors_test.go), `TestREADMEAnchorsPointAtTheNamedSymbol` in
[readme_identity_test.go](readme_identity_test.go)). So you can answer a question by citing an anchor
without opening the file. If an anchor and the code ever disagree, **believe the code** and file an
issue — that is a CI failure, not a doc nit.

## Check "Not built" before you import anything

**[docs/reference.md#not-built](docs/reference.md#not-built)** names symbols that do **not** exist,
because older documentation mentioned them: `ToolRegistry`, `ReadTool`, `AgentMessage`, `EventLog`.
They appear in doc-comment prose as deferred ideas; they are not declarations. Importing one is the
single most likely way to waste your user's time here.

Also listed there, and easy to assume otherwise: `RunToolLoop` and `RunToolLoopTiered` take exactly
**one** `Tool`. No provider adapter ships — you implement `StructuredCaller`, which is one method.

## The decision that is easy to get wrong

Four constructors declare "what my agent can do", and **all four compile**, so choosing wrong fails at
review rather than at build. Pick by asking where the act's contract comes from:

| Your backend serves an op contract | `Bind(ctx, src, uses)` — [bind.go:112](bind.go#L112) |
|---|---|
| …and some acts are yours alone | `BindMixed(ctx, src, uses, local)` — [bind.go:125](bind.go#L125) |
| …and there is no backend contract | `NewActionSet[S]()` — [action.go:281](action.go#L281) |
| The model emits `calls[]` you must gate | `NewToolSet[S]()` — [toolset.go:83](toolset.go#L83) |

`doc.go` has the long form. `BindMixed(ctx, nil, nil, local)` is correct even though it reads like a
mistake.

## Rules that are load-bearing, not stylistic

1. **Never execute `verdict.Calls` directly — always through `GateProgram`.** The tier gate is the
   only thing preventing an irreversible auto-run. Nothing else in the kit enforces it.
2. **Never hand-write a tool list into a prompt.** Use `RenderCapabilities(caps)`, or the prompt and
   the code drift silently.
3. **Always declare a `Tier` explicitly.** The zero value is `TierReadOnly`, which auto-runs, so a
   forgotten field would mean "run it live" — `GateProgram` rejects a bare zero tier for that reason.
   Read-only acts say so with `TierReadOnlyExplicit`.
4. **Declare each capability exactly once.** A duplicate name is rejected by the gate: specs are a
   slice (first match wins) while handlers are a map (last write wins), so a duplicate would gate on
   one declaration's tier and execute the other's handler.
5. **A `GateProgram` error rejects the whole program.** Do not drop the bad call and continue —
   that half-applies a multi-op write.

## If you change code here

- `go test -race -count=1 ./...`, `gofmt -l .`, `go vet ./...`, and `./scripts/check-deps.sh` all must
  pass. `-count=1` matters: the doc guards read files the compiler does not track, so a cached PASS
  can hide a doc that drifted.
- **A bug fix needs a failing test in the same commit**, and it must fail before the fix.
- **Do not add a dependency.** [scripts/check-deps.sh](scripts/check-deps.sh) caps external deps and
  hard-fails on a banned import. Raising the cap is a design decision, not a lint fix.
- If you move code, the anchor guards will name which `docs/reference.md` references went stale. Fix
  the references; do not delete the guard.
- Read [CONTRIBUTING.md](CONTRIBUTING.md) and [docs/contributing.md](docs/contributing.md) before
  changing anything in the core. They are blunt on purpose.

## A caveat worth carrying

This kit's safety properties are structural and tested, but "the doc says it is structural" and "I
proved it is structural" are different claims. A duplicate-name defect that executed an irreversible
handler live survived four production consumers and a full read-through review; only an adversarial
test found it. The repo's own standard, from [docs/contributing.md](docs/contributing.md), is the
right one to apply to anything you assert here:

> **Verify every claim, including your own.** Grep or run before writing it down.
