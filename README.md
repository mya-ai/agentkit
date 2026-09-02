# agentkit

**Autonomous Go agents that change things — safely.**

You declare what your agent may do as typed specs. agentkit renders them into the prompt (so the tool
list cannot drift from your code), verifies the model's output against them (models invent opcodes — a
fact to verify, not to prompt away), and gates every call by **reversibility**: reversible calls may
run live, irreversible ones are captured as proposals on a path that physically cannot execute them.

Around that gate sit budgeted loop primitives with agent-controlled termination, a hard round ceiling,
and a degeneration guard.

**Go owns the write.** The loop returns a decision; it never persists.

```go
import "github.com/mya-ai/agentkit"
```

One external dependency. No provider SDK, no queue, no database — you bring those.

---

## Is this for you?

**Yes, if** you are building an agent that runs on a schedule or an event, gathers context, reasons in
a bounded loop, and then changes something real — a record, a plan, a ticket, a calendar.

**No, if** you want turn-by-turn chat. There is no transcript and no streaming here; a run is
deliberately stateless. That is a different shape, and this kit is not it.

## Install

```bash
go get github.com/mya-ai/agentkit
```

Requires Go 1.25 or newer.

**See it work first**, with no credentials and no setup:

```bash
git clone https://github.com/mya-ai/agentkit && cd agentkit
go run ./example/cmd/demo
```

The agent asks to read an account and to delete it. The read runs; the delete is withheld as a
proposal for a human. Same loop, same model, same intent — the tier decides. The demo exits non-zero
if the delete ever executes.

## The smallest useful agent

Gather context, make one call, write the result:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/mya-ai/agentkit"
)

type reviewCtx struct {
	Ticket string
	Age    int
}

type reviewVerdict struct {
	Summary  string `json:"summary"`
	Escalate bool   `json:"escalate"`
}

// IsMaterial tells the loop whether anything worth acting on happened. A
// non-material result is a legitimate, model-decided silent run.
func (v reviewVerdict) IsMaterial() bool { return v.Escalate }

type reviewAgent struct{}

func (reviewAgent) Role() agentkit.RoleSpec {
	return agentkit.RoleSpec{Model: "gpt-4o-mini", MaxTokens: 800, PromptType: "review"}
}

// System is the stable contract; Dynamic is this call's data. The split is what
// makes prompt caching work.
func (reviewAgent) Render(rc reviewCtx, _ agentkit.RoundInfo) (system, dynamic string) {
	return "You triage support tickets. Reply as JSON: {summary, escalate}.",
		fmt.Sprintf("Ticket %s has been open %d days.", rc.Ticket, rc.Age)
}

// caller is YOUR LLM — one method. Swap this stub for a real adapter
// (OpenAI, Anthropic, Ollama, …): see docs/providers.md.
type caller struct{}

func (caller) CompleteJSON(context.Context, agentkit.StructuredRequest) (string, error) {
	return `{"summary":"Customer blocked on billing","escalate":true}`, nil
}

func main() {
	ctx := context.Background()
	rc := reviewCtx{Ticket: "T-1024", Age: 9}

	verdict, err := agentkit.RunOnce[reviewCtx, reviewVerdict](
		ctx, caller{}, reviewAgent{}, rc, nil, nil,
	)
	if err != nil {
		// errors.Is(err, agentkit.ErrThrottled) → release the work, don't retry
		log.Fatal(err)
	}
	if verdict.Escalate {
		// YOUR code does the write. The loop returns a decision; it never persists.
		fmt.Println("escalating:", verdict.Summary)
	}
}
```

That runs as-is. The only piece you replace is `caller` — one method over whichever provider you
use. See **[docs/providers.md](docs/providers.md)** for OpenAI, Anthropic, Ollama and test-fake adapters.

## Pick your loop

| You need | Use | What it does |
|---|---|---|
| one structured verdict | `RunOnce` | render → one JSON call → typed result |
| one piece of prose | `RunOnceText` | same, returning markdown/text |
| draft → critique → revise | `Pair` | an executor and a judge, converging within a budget |
| look something up, then decide | `RunToolLoop` | the kit calls your tool, the agent reflects on results |
| …and some tools mutate | `RunToolLoopTiered` | reads run live; mutations become proposals, never executed |

All five are the same atom underneath. Learn `RunOnce` and the rest follow.

## What you bring

The kit declares interfaces; you supply implementations. This is why it costs one dependency.

Seams the kit **calls into** — implement them and the kit drives them:

| Interface | You provide | Ships here |
|---|---|---|
| `StructuredCaller` / `TextCaller` | your LLM (one method) | fakes in `example/` |
| `ActorStore` (`Lease` + `Inbox`) | durable single-writer state | `InMemoryActorStore` |
| `NarrationSink` | where live progress is written | — |
| `Observer` | per-round latency/metrics | — |

And one helper the kit does **not** call — your own `Role()` consults it, so the kit stays ignorant
of where your prompts live:

| Type | For | Ships here |
|---|---|---|
| `TemplateProvider` | resolving prompt/model/region per (agent, role) | `MapTemplateProvider`, `TemplateProviderFunc`; `nil` = embedded defaults |

Nothing dictates what triggers your agent. A queue consumer, a cron tick, an HTTP handler and a test
are all equally valid front doors.

## Safety model

Three mechanisms, all structural rather than prompt-based — because a prompt is a request and a type
is a guarantee:

1. **Reversibility tiers gate autonomy.** Every tool carries a tier. Read-only and reversible calls
   run live; external-reversible and irreversible ones are captured as a `ProposedAction` for a human
   to bless. In `RunToolLoopTiered` a mutation *physically cannot* auto-execute.
2. **Declared capabilities are verified, not trusted.** The model's emitted calls are checked against
   your specs before anything runs. An invented opcode is rejected, loudly.
3. **A proposal is a function of state.** A proposal is minted only if its op would actually change
   something, so agents do not re-propose what is already true.

Plus: a hard round ceiling, a degeneration guard that records a stalemate rather than shipping a
contested result, and a throttle contract (`ErrThrottled`) that releases work back to your queue
instead of retrying into a provider that is already shedding load.

## Documentation

| | |
|---|---|
| **[docs/reference.md](docs/reference.md)** | symbol → `file:line` → signature, traps, compiled samples. Start here for API questions. |
| [docs/providers.md](docs/providers.md) | bring your own LLM |
| [docs/guides/building-an-agent.md](docs/guides/building-an-agent.md) | the order of work, start to finish |
| [docs/design/architecture.md](docs/design/architecture.md) | why it is shaped this way |
| [docs/design/agent-runtime.md](docs/design/agent-runtime.md) | the load-bearing decisions |
| [docs/contributing.md](docs/contributing.md) | what gets a PR rejected |
| [VERSIONING.md](VERSIONING.md) | what a version number promises, and what counts as breaking |
| [example/](example/) | three reference agents, plus a runnable demo |
| [AGENTS.md](AGENTS.md) | if you are an AI coding agent: start here |

Every `file:line` anchor in the reference is asserted against source in CI, so the docs cannot drift
the way documentation usually does.

## Maturity

Extracted from four production agents — each driving a different loop — that between them drove
every design decision recorded here. Where a claim below counts *consumers* rather than agents, it
means the codebases that adopted a particular seam, which is a smaller number.
The reference lists exactly which symbols carry production traffic and which are validated but
unadopted — including the ones production consumers *declined*, and why. Where a comment says "a real
bug" or "measured", that is where the measurement came from.

It is honest about gaps: see **[Not built](docs/reference.md#not-built)** before assuming a feature exists.

### Versioning

**v0.x — pin it.** While the major version is 0, the *minor* version is the breaking-change signal:
`v0.2.0` may break what `v0.1.0` compiled, while `v0.1.1` will not. `go.mod` pins for you.

Two rules worth knowing before you upgrade, because neither is what plain semver would tell you:

- A tool moving to a **less** restrictive tier — something withheld as a proposal now runs live — is
  a **breaking** change, even though nothing fails to compile. The blast radius is your data.
- A tool moving to a **more** restrictive tier ships in a **patch**, immediately, with no deprecation
  window. The kit will always resolve a tier question toward withholding.

Full rules, the what-counts-as-breaking table, and what v1.0.0 waits on: **[VERSIONING.md](VERSIONING.md)**.

## License

Apache 2.0 — see [LICENSE](LICENSE).
