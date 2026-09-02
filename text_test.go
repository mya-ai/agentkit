package agentkit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeTextCaller returns a queued markdown string or a queued error.
type fakeTextCaller struct {
	text string
	err  error
	reqs []GenerateTextRequest
}

func (f *fakeTextCaller) GenerateText(_ context.Context, req GenerateTextRequest) (LLMCallResult, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return LLMCallResult{}, f.err
	}
	return LLMCallResult{Text: f.text}, nil
}

// fakeTextAgent renders a fixed prompt + role.
type fakeTextAgent struct{}

func (fakeTextAgent) Render(_ struct{}) (string, string) { return "sys", "dyn" }
func (fakeTextAgent) Role() RoleSpec {
	return RoleSpec{Model: "test/model", PromptType: "digest", MaxTokens: 2048, Temperature: 0.4}
}

func TestRunOnceText_ReturnsMarkdown(t *testing.T) {
	caller := &fakeTextCaller{text: "# Daily Digest\n\nQuiet morning."}
	out, err := RunOnceText[struct{}](context.Background(), caller, fakeTextAgent{}, struct{}{}, nil)
	if err != nil {
		t.Fatalf("RunOnceText err: %v", err)
	}
	if out != "# Daily Digest\n\nQuiet morning." {
		t.Fatalf("unexpected text: %q", out)
	}
	// Confirm the role config flows into the request (and PlainTextResponse is the caller's job).
	if len(caller.reqs) != 1 || caller.reqs[0].Model != "test/model" || caller.reqs[0].MaxTokens != 2048 {
		t.Fatalf("role config did not flow into the request: %+v", caller.reqs)
	}
}

func TestRunOnceText_ThrottleSurfaces(t *testing.T) {
	caller := &fakeTextCaller{err: fmt.Errorf("429: %w", ErrThrottled)}
	_, err := RunOnceText[struct{}](context.Background(), caller, fakeTextAgent{}, struct{}{}, nil)
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("expected ErrThrottled to surface, got %v", err)
	}
}

func TestRunOnceText_EmptyIsNotAnError(t *testing.T) {
	// The kit returns empty as-is; the AGENT decides empty is a redeliver (a digest agent does).
	caller := &fakeTextCaller{text: ""}
	out, err := RunOnceText[struct{}](context.Background(), caller, fakeTextAgent{}, struct{}{}, nil)
	if err != nil {
		t.Fatalf("empty text must not be an error at the kit level: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty, got %q", out)
	}
}

// recordingObserver captures each ObserveLLM call (the shared observation discipline both transport
// flavors must honor).
type recordingObserver struct {
	roles  []string
	rounds []int
}

func (o *recordingObserver) ObserveLLM(role string, round int, _ time.Duration) {
	o.roles = append(o.roles, role)
	o.rounds = append(o.rounds, round)
}

// TestRunOnceText_ObservesLikeTheAtom pins the "one primitive, two transports" alignment: the text
// flavor reports to the Observer exactly once, with a stable role label and the single-shot round (1) —
// the same observation discipline runOnce honors. Guards against the text path drifting back into a
// copy-pasted body that forgets to observe.
func TestRunOnceText_ObservesLikeTheAtom(t *testing.T) {
	obs := &recordingObserver{}
	caller := &fakeTextCaller{text: "ok"}
	if _, err := RunOnceText[struct{}](context.Background(), caller, fakeTextAgent{}, struct{}{}, obs); err != nil {
		t.Fatalf("RunOnceText err: %v", err)
	}
	if len(obs.roles) != 1 {
		t.Fatalf("expected exactly one ObserveLLM call, got %d", len(obs.roles))
	}
	if obs.roles[0] != "once_text" {
		t.Fatalf("expected role %q, got %q", "once_text", obs.roles[0])
	}
	if obs.rounds[0] != 1 {
		t.Fatalf("a standalone text call is round 1, got %d", obs.rounds[0])
	}
}
