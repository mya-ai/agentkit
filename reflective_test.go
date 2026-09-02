package agentkit

import (
	"strings"
	"testing"
)

type rdoc struct{ title string }

func info(round, maxRounds int, prior ...[]rdoc) ToolRoundInfo[rdoc] {
	return ToolRoundInfo[rdoc]{RoundInfo: RoundInfo{Round: round, MaxRounds: maxRounds}, PriorResults: prior}
}

// Round 1, no priors: the scaffold primes the request and tells the executor to build its first call.
func TestRenderReflective_FirstRound(t *testing.T) {
	system, dynamic := RenderReflective(ReflectivePrompt[rdoc]{System: "DOCTRINE", Request: "find Acme"}, info(1, 2))
	if system != "DOCTRINE" {
		t.Errorf("system must be the consumer's doctrine, got %q", system)
	}
	if !strings.Contains(dynamic, `Request: "find Acme"`) {
		t.Errorf("must prime the request, got: %s", dynamic)
	}
	if !strings.Contains(dynamic, "Round 1 of 2") {
		t.Errorf("must carry the round position, got: %s", dynamic)
	}
	if !strings.Contains(dynamic, "Nothing retrieved yet") {
		t.Errorf("round 1 must tell the executor to build a first call, got: %s", dynamic)
	}
}

// With a RenderItem renderer, the scaffold LISTS prior results so the executor can reflect on specifics.
func TestRenderReflective_ListsPriorResults(t *testing.T) {
	p := ReflectivePrompt[rdoc]{
		System:     "D",
		RenderItem: func(d rdoc) string { return d.title },
	}
	_, dynamic := RenderReflective(p, info(2, 3, []rdoc{{"Acme onboarding"}, {"Send packet"}}))
	if !strings.Contains(dynamic, "Retrieved so far: 2 item") {
		t.Errorf("must reflect the count, got: %s", dynamic)
	}
	if !strings.Contains(dynamic, "Acme onboarding") || !strings.Contains(dynamic, "Send packet") {
		t.Errorf("must list prior-result lines via RenderItem, got: %s", dynamic)
	}
	if !strings.Contains(dynamic, "Reflect:") {
		t.Errorf("must prompt to reflect, got: %s", dynamic)
	}
}

// Without a RenderItem renderer, the scaffold reflects only the COUNT (the lighter shape).
func TestRenderReflective_CountOnlyWhenNoRenderer(t *testing.T) {
	_, dynamic := RenderReflective(ReflectivePrompt[rdoc]{System: "D"}, info(2, 3, []rdoc{{"x"}, {"y"}}))
	if !strings.Contains(dynamic, "Retrieved so far: 2 item") {
		t.Errorf("must reflect the count, got: %s", dynamic)
	}
	if strings.Contains(dynamic, "- x") {
		t.Error("must NOT list items when no RenderItem renderer is supplied")
	}
}

// The reflection list is capped (prompt stays light).
func TestRenderReflective_CapsList(t *testing.T) {
	var batch []rdoc
	for i := 0; i < 50; i++ {
		batch = append(batch, rdoc{title: "item"})
	}
	p := ReflectivePrompt[rdoc]{System: "D", RenderItem: func(d rdoc) string { return d.title }, MaxItemsShown: 5}
	_, dynamic := RenderReflective(p, info(2, 3, batch))
	if got := strings.Count(dynamic, "- item"); got != 5 {
		t.Errorf("MaxItemsShown=5 must cap the list, got %d lines", got)
	}
}

// The final round instructs the executor to stop (self-pacing).
func TestRenderReflective_FinalRoundStops(t *testing.T) {
	_, dynamic := RenderReflective(ReflectivePrompt[rdoc]{System: "D"}, info(2, 2))
	if !strings.Contains(dynamic, "FINAL round") {
		t.Errorf("the final round must instruct the executor to stop, got: %s", dynamic)
	}
}
