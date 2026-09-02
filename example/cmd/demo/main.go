// Command demo runs agentkit end to end with no credentials, no network and no
// setup: `go run ./example/cmd/demo`.
//
// It exists because reading about a safety guarantee is not the same as watching
// it hold. The agent below WANTS to delete a customer record. It says so, in the
// tool call it emits. The loop routes it to a proposal instead, and the delete
// function is never called — the demo asserts that at the end, so if the
// guarantee ever broke, this program would say so out loud.
//
// The model is a scripted stub. That is the point: the safety property is
// structural, so it does not depend on the model cooperating.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mya-ai/agentkit"
)

// --- the context the agent reasons over --------------------------------------

type ctxData struct{ Account string }

// --- the tool: one tool that can read OR delete, tiered by what it is asked ---

type args struct {
	Query  string `json:"query,omitempty"`  // a READ
	Delete string `json:"delete,omitempty"` // a MUTATION
}

type record struct{ Text string }

// A tool declares ONE tier, so a read tool and a mutating tool are two tools.
// That is the shape the loop enforces: TieredTool.Tier() takes no arguments, so a
// tool cannot claim to be safe for one call and dangerous for another.

// lookupTool is read-only, so the loop runs it LIVE.
type lookupTool struct{ calls int }

func (t *lookupTool) Name() string        { return "lookup_account" }
func (t *lookupTool) Tier() agentkit.Tier { return agentkit.TierReadOnlyExplicit }
func (t *lookupTool) Call(_ context.Context, a args) ([]record, error) {
	t.calls++
	return []record{{Text: "account " + a.Query + ": 3 open tickets, payment failed twice"}}, nil
}

// deleteTool is deliberately dangerous: Call really would delete. Nothing about
// the loop depends on the tool being safe — the TIER is what gates it.
//
// ⚠️ Note what the ZERO value does: a forgotten Tier is REJECTED — the loop stops
// with an error rather than running the call. Read-only has to be opted into
// explicitly (TierReadOnlyExplicit), so "this may auto-run" is a statement, never
// an omission. That guard was missing on this path until a comment audit found
// the comment promising it and the code not making it.
type deleteTool struct{ deleted []string }

func (t *deleteTool) Name() string        { return "delete_account" }
func (t *deleteTool) Tier() agentkit.Tier { return agentkit.TierIrreversible }
func (t *deleteTool) Call(_ context.Context, a args) ([]record, error) {
	// If you ever see "DELETED" in this program's output, the guarantee broke.
	t.deleted = append(t.deleted, a.Delete)
	return []record{{Text: "DELETED " + a.Delete}}, nil
}

// --- the agent: renders the prompt, names its model --------------------------

type agent struct{}

func (agent) Role() agentkit.RoleSpec {
	return agentkit.RoleSpec{Model: "stub/model", PromptType: "demo", MaxTokens: 512}
}

func (agent) Render(rc ctxData, info agentkit.ToolRoundInfo[record]) (system, dynamic string) {
	return "You investigate accounts. Emit one tool call per round.",
		fmt.Sprintf("Account %s. Round %d of %d. You have %d prior result batch(es).",
			rc.Account, info.Round, info.MaxRounds, len(info.PriorResults))
}

// --- the "model": scripted, so the demo is deterministic ---------------------

// caller returns a canned ToolStep. A real StructuredCaller wraps your provider —
// see docs/providers.md; the interface is this one method.
type caller struct{ query, purge string }

func (c *caller) CompleteJSON(context.Context, agentkit.StructuredRequest) (string, error) {
	b, err := json.Marshal(map[string]any{
		"done":      false,
		"rationale": "This account is failing every health check.",
		"call":      args{Query: c.query, Delete: c.purge},
	})
	return string(b), err
}

func main() {
	rc := ctxData{Account: "acct-42"}
	cfg := agentkit.LoopConfig{MaxRounds: 1}

	// --- 1. A READ tool. The loop runs it live. ---
	look := &lookupTool{}
	read := agentkit.RunToolLoopTiered[ctxData, args, record](
		context.Background(), &caller{query: "acct-42"}, agent{}, look, rc, cfg, nil, nil)

	fmt.Println("─── read tool (tier: read-only) ───")
	fmt.Printf("  executed live:  %v (%d call)\n", look.calls > 0, look.calls)
	for _, it := range read.Items {
		fmt.Printf("  result:         %s\n", it.Text)
	}

	// --- 2. A MUTATING tool. Same loop, same model, same intent. ---
	del := &deleteTool{}
	gated := agentkit.RunToolLoopTiered[ctxData, args, record](
		context.Background(), &caller{purge: "acct-42"}, agent{}, del, rc, cfg, nil, nil)

	fmt.Println("\n─── delete tool (tier: irreversible) ───")
	fmt.Printf("  outcome:        %s (%s)\n", gated.Outcome, gated.StoppedReason)
	fmt.Printf("  executed live:  %v\n", len(del.deleted) > 0)
	for _, p := range gated.Proposed {
		fmt.Printf("  withheld:       %s (tier %s)\n", p.ToolName, p.Tier)
		fmt.Printf("  its reason:     %q\n", p.Rationale)
		fmt.Printf("  args a human would approve: %+v\n", p.Args)
	}

	// The point, asserted rather than claimed.
	if len(del.deleted) > 0 {
		fmt.Fprintf(os.Stderr, "\n⛔ SAFETY VIOLATION: the loop executed %v\n", del.deleted)
		os.Exit(1)
	}
	if len(gated.Proposed) != 1 {
		fmt.Fprintf(os.Stderr, "\n⛔ expected exactly one withheld call, got %d\n", len(gated.Proposed))
		os.Exit(1)
	}
	fmt.Println("\n✅ The model asked for both. The read ran; the delete did not.")
	fmt.Println("   Nothing about the model changed that — the tier did.")
}
