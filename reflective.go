package agentkit

import (
	"fmt"
	"strings"
)

// reflective.go is the ReflectiveToolLoop SCAFFOLD: a domain-free helper that
// assembles the standard prime → reflect-on-results → self-pace dynamic prompt every RunToolLoop
// executor's Render builds. It was deliberately NOT built until a real consumer showed the shape
// (the no-speculative-build discipline); a retrieval agent was that
// consumer — its Render and the two example Renders all repeat the same
// structure, and this lifts that repetition into the kit.
//
// The scaffold names NO domain: it takes the consumer's doctrine (system) + request line + an
// optional per-result line renderer, and returns the (system, dynamic) a ToolAgent.Render returns.
// The COGNITION (what the doctrine says, how to render a result, what counts as "enough") stays in
// the consumer — the scaffold only assembles the round's framing (request, budget position, the
// reflect-on-prior-results section, the final-round stop note).

// ReflectivePrompt is the consumer-supplied content the scaffold frames into a round prompt. It is
// pure data + one optional callback — no domain types in the kit.
type ReflectivePrompt[R any] struct {
	// System is the consumer's cognition doctrine (the system prompt). Required.
	System string
	// Request is the one-line statement of what this run is after (the topic / the user's text).
	// Optional — omitted from the dynamic prompt when empty.
	Request string
	// RenderItem turns one prior-result item into a reflection line (without a leading "- "). When
	// nil, the scaffold reflects only the COUNT of prior results (the example research-agent shape);
	// when set, it lists items so the executor can reflect on specifics (the retrieval shape). Optional.
	RenderItem func(R) string
	// MaxItemsShown caps the reflection list so the prompt stays light. <= 0 → DefaultReflectItemsShown.
	MaxItemsShown int
}

// DefaultReflectItemsShown bounds the reflection list when RenderItem is set (keeps the prompt light).
const DefaultReflectItemsShown = 12

// RenderReflective assembles the standard (system, dynamic) for one tool-loop round from the
// consumer's ReflectivePrompt + the round's ToolRoundInfo. A ToolAgent.Render is then a one-liner:
//
//	type Doc struct{ Title string }
//
//	func (a myAgent) Render(rc Ctx, info agentkit.ToolRoundInfo[Doc]) (string, string) {
//	    return agentkit.RenderReflective(agentkit.ReflectivePrompt[Doc]{
//	        System: a.doctrine, Request: rc.text, RenderItem: func(d Doc) string { return d.Title },
//	    }, info)
//	}
//
// The dynamic prompt always carries: the request (if any), the round position (self-pacing), a
// reflection of prior results (count, or a capped list when RenderItem is set), and — on the final
// round — an explicit stop instruction. This is exactly the framing the reflect-on-results contract
// needs; the consumer supplies only the doctrine + how to render an item.
func RenderReflective[R any](p ReflectivePrompt[R], info ToolRoundInfo[R]) (system, dynamic string) {
	system = p.System

	var b strings.Builder
	if p.Request != "" {
		fmt.Fprintf(&b, "Request: %q\n", p.Request)
	}
	fmt.Fprintf(&b, "Round %d of %d.", info.Round, info.MaxRounds)

	total := 0
	for _, batch := range info.PriorResults {
		total += len(batch)
	}
	switch {
	case total == 0:
		b.WriteString("\nNothing retrieved yet — build your first tool call.")
	case p.RenderItem == nil:
		// Count-only reflection (the consumer didn't supply a line renderer).
		fmt.Fprintf(&b, "\nRetrieved so far: %d item(s) across %d call(s). Reflect: is that enough, or refine?",
			total, len(info.PriorResults))
	default:
		limit := p.MaxItemsShown
		if limit <= 0 {
			limit = DefaultReflectItemsShown
		}
		fmt.Fprintf(&b, "\nRetrieved so far: %d item(s) across %d call(s):", total, len(info.PriorResults))
		shown := 0
		for _, batch := range info.PriorResults {
			for _, it := range batch {
				if shown >= limit {
					break
				}
				fmt.Fprintf(&b, "\n  - %s", p.RenderItem(it))
				shown++
			}
		}
		b.WriteString("\nReflect: do these cover the request? If yes, stop (done:true). If a gap remains, refine the call.")
	}

	if info.IsFinalRound() {
		b.WriteString("\n\nThis is the FINAL round — stop now (done:true) and use what you have; do not start a new call.")
	}
	return system, b.String()
}
