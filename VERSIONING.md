# Versioning and releases

agentkit follows [Semantic Versioning 2.0.0](https://semver.org) and Go's module version rules. This
document states what a version number here actually promises, because "semver" alone does not answer
the question that matters: *what may break under me, and when?*

## The tag format

```
v<major>.<minor>.<patch>          v0.1.0, v0.4.2, v1.0.0
v<major>.<minor>.<patch>-rc.<n>   v1.0.0-rc.1        (pre-release, opt-in only)
```

Rules, all of them load-bearing for Go tooling:

- **The `v` prefix is required.** `go get` does not recognize a tag without it.
- **Three numeric components, always.** `v0.2` is not a module version; `v0.2.0` is.
- **Tags are immutable.** The Go module proxy caches a tag the first time anyone fetches it, so a
  moved tag serves different bytes to different people, permanently. A bad release is superseded by
  the next patch, never retagged. If a release is actively harmful, retract it in `go.mod` and
  supersede it — do not delete it.
- **Annotated, signed, on `main`.** `git tag -s -a` with the release note as the message, on a commit
  that is already on `main` and green in CI.
- **`v2` and beyond change the import path.** Go requires `/v2` in the module path for major version
  2+. That is a rename of every consumer's import line, so it is a decision about them, not about us.

## What each component means

**Major** — an existing, documented, non-experimental symbol changed shape or vanished, and correct
code that compiled before no longer does (or, worse, still compiles and now behaves differently).

**Minor** — new capability, backward compatible. New exported symbols, new optional fields, a new
loop primitive, a `PLANNED` entry in [docs/reference.md](docs/reference.md) becoming `BUILT`.

**Patch** — a fix with no API change. Bug fixes, doc corrections, performance, test coverage.

### What counts as breaking here

Beyond the obvious (removing or renaming an exported symbol, changing a signature), this kit treats
these as **breaking**, because consumers depend on them the same way they depend on a signature:

| Change | Why it is major |
|---|---|
| Adding a method to an interface a consumer implements | `StructuredCaller`, `TextCaller`, `ActorStore`, `NarrationSink`, `Observer`, `AgentRuntime` are implemented *by you*. A new method breaks every implementation at compile time. |
| Moving a tool or act to a **less** restrictive tier | Something that was withheld as a proposal now executes live. Nothing fails to compile — the blast radius is production data. This is the most dangerous change the kit can make, and it is major even though it looks like a behavior tweak. |
| Making a default fail **open** | Same reason. A forgotten field must never start meaning "run it live." |
| Tightening capability verification so a previously accepted call is rejected | A working agent starts erroring. |
| Changing the shape of a rendered prompt in a way that alters model output | Not a compile break, but it silently changes what your agent decides. |

And these are explicitly **not** breaking:

- Moving a tool or act to a **more** restrictive tier. Something that ran live is now proposed for a
  human. This is a safety fix and ships in a patch — the kit will always resolve a tier question
  toward withholding.
- Adding a field to a struct the consumer only ever *receives*.
- Any `file:line` anchor in [docs/reference.md](docs/reference.md) moving. Anchors are documentation,
  asserted in CI; they are not API.
- Changing anything listed under [Not built](docs/reference.md#not-built). It does not exist.

## v0.x — where we are now

**While the major version is 0, the minor version is the breaking-change signal.** `v0.2.0` may break
what `v0.1.0` compiled. Patch releases within a minor (`v0.1.1`) stay compatible.

This is deliberate, and honest about the kit's age: it was extracted from four production agents, and
[docs/reference.md](docs/reference.md) marks which symbols carry production traffic and which are
validated but unadopted. **An unadopted symbol has not yet met a consumer who disagreed with it.**
Pinning v0.x is the right call — `go.mod` does it for you.

Even in v0.x, a breaking change ships with:

1. A release note naming the symbol and the migration, in the format below.
2. Where mechanically possible, a deprecated shim for one minor version — the old symbol kept,
   marked `// Deprecated: use X instead.`, delegating to the new one.

The **tier gate is exempt from that grace period in one direction only**: a tier moving toward
*more* restriction ships immediately, in a patch, with no shim. A safety defect does not get a
deprecation window.

## Reaching v1.0.0

v1.0.0 is not a quality claim — the test suite, the race detector, and the dependency budget already
run on every commit. It is a **stability claim**, and it costs the freedom to fix a bad seam cheaply.
So it waits on evidence, not on a date:

- Every interface a consumer implements has been implemented by someone outside this repository.
- The four constructors (`Bind`, `BindMixed`, `NewActionSet`, `NewToolSet`) have survived a consumer
  who picked the wrong one — the failure mode `doc.go` warns about, met in the wild.
- No symbol in the reference is still marked `PARTIAL`.
- The gaps under [Not built](docs/reference.md#not-built) are gaps we intend to keep, not a backlog.

Until then, v0.x is the accurate label.

## Release checklist

```bash
git checkout main && git pull
gofmt -l .                  # must print nothing
go vet ./...
go test -race -count=1 ./...
./scripts/check-deps.sh
go run ./example/cmd/demo   # must exit 0 and must NOT execute the delete

git tag -s -a v0.1.0 -m "v0.1.0 — <one line>"
git push origin v0.1.0
```

Then confirm the proxy has it, which is also the first real test that the module path resolves:

```bash
GOPROXY=proxy.golang.org go list -m github.com/mya-ai/agentkit@v0.1.0
```

## Release note format

Same standard as a commit message in [CONTRIBUTING.md](CONTRIBUTING.md): **state the failure, then
the fix.** A release note is read by someone deciding whether to upgrade, so lead with what would
break for them.

```markdown
## v0.2.0

### Breaking
- `Observer` gained `OnRound`. Implementations must add the method; embed
  `agentkit.NoopObserver` to take the default.

### Safety
- `delete_account` moved from external-reversible to irreversible. It was auto-executing
  inside `RunToolLoopTiered`; it is now withheld as a proposal.

### Added
- `RunToolLoopTiered` accepts multiple tools via `ToolSet.GateProgram`.

### Fixed
- A judge error discarded the verdict instead of surfacing it as `LastResult`.
```
