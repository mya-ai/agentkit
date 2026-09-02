# Contributing

Thanks for looking. A few things worth knowing before you open a PR.

## The standing question

Every change is judged against one question:

> **Does this make adding or changing a capability easier or harder for a developer who does not work
> on this framework?**

Harder is a blocked merge, however elegant the internal justification.

The design rules that follow from that — including the review standard and the two ways a design
tends to go wrong — are in **[docs/contributing.md](docs/contributing.md)**. Read it
before changing anything in the core. It is blunt, and deliberately so.

## Before you start

For anything beyond a typo or an obvious bug fix, **open an issue first**. This framework has a
strong opinion about its own scope, and the fastest way to have work rejected is to build something
that belongs in a consumer rather than in the kit. The line:

> Is this solving an AGENT-SPECIFIC problem, or re-solving a FRAMEWORK problem locally?

Agent-specific belongs in your agent: gather, prompts, output schema, domain rules. Framework
problems — loops, retries, dedup, proposal machinery, single-writer — belong here, and a local
workaround for one is what this kit exists to delete.

## What will get a PR rejected

- **A new dependency in the core.** Importing agentkit costs one external dependency today. `scripts/check-deps.sh` caps it at five
  in CI and hard-fails on a banned import, so adding one is visible but not blocked — make it a
  deliberate decision, not a side effect. If you need a provider, a queue, or a store, it goes behind an
  interface the consumer implements. Raising the budget is a design decision, not a lint fix.
- **A comment where a compiler error belongs.** "Keep these in sync" is a failed design. If a wrong
  edit is not caught by the compiler or a test, the design is not finished.
- **A bug fix without a failing test.** The test goes in the same commit, and it must fail before
  the fix.
- **Documentation that outruns the code.** Do not describe a PLANNED symbol as if it exists. The
  BUILT/PARTIAL/PLANNED labels are load-bearing and CI checks the reference's anchors against source.
- **Defaults that fail open.** A forgotten field must not mean "run it live".

## Running the checks

```bash
gofmt -l .              # must print nothing
go vet ./...
go test -race ./...
./scripts/check-deps.sh # the dependency budget
golangci-lint run       # optional locally; runs in CI (pin v2.1.0 to match it)
```

The doc guards are part of `go test`: if you move code, `TestREADMEAnchorsResolve` and
`TestREADMEAnchorsPointAtTheNamedSymbol` will tell you which `file:line` references in
[docs/reference.md](docs/reference.md) went stale. Fix the references — do not delete the guard.

## Commit messages

State the failure, then the fix. `fix(loop): a judge error discarded the verdict` beats
`fix: improve error handling`. If a defect shipped, say what it cost — that sentence is why the next
person does not reintroduce it.

## Versioning

If your change breaks an existing symbol, moves a tool to a **less** restrictive tier, or adds a
method to an interface consumers implement, say so in the PR — that is a minor bump while we are on
v0.x, and it needs a release note. [VERSIONING.md](VERSIONING.md) has the rules and the
what-counts-as-breaking table.

## Code of conduct

The design rules here are blunt about *code*. That is aimed at designs, never at people — see
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

By contributing you agree that your contributions are licensed under
[Apache 2.0](LICENSE).
