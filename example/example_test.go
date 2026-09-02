package example

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mya-ai/agentkit"
)

// fakeCaller is a TextCaller returning a fixed note (or error).
type fakeCaller struct {
	text string
	err  error
}

func (f fakeCaller) GenerateText(_ context.Context, _ agentkit.GenerateTextRequest) (agentkit.LLMCallResult, error) {
	if f.err != nil {
		return agentkit.LLMCallResult{}, f.err
	}
	return agentkit.LLMCallResult{Text: f.text}, nil
}

// fakeStore records what was written + returns a fixed gather count.
type fakeStore struct {
	count     int
	gatherErr error
	recorded  []string
	recordErr error
}

func (s *fakeStore) gatherCount(_ context.Context, _ string) (int, error) {
	return s.count, s.gatherErr
}
func (s *fakeStore) recordNote(_ context.Context, _, markdown string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.recorded = append(s.recorded, markdown)
	return nil
}

func cmd() Job {
	return Job{UserID: "u1", UserTimezone: "America/New_York"}
}

func TestExample_HappyPath(t *testing.T) {
	store := &fakeStore{count: 3}
	r := NewRunner(fakeCaller{text: "Good morning! You have 3 things today."}, store, "test/model")
	if err := r.Run(context.Background(), cmd()); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if len(store.recorded) != 1 || store.recorded[0] != "Good morning! You have 3 things today." {
		t.Fatalf("note not recorded as expected: %v", store.recorded)
	}
}

func TestExample_QuietDayStillRecords(t *testing.T) {
	// The "Go owns the write, unconditionally" lesson: zero items is a quiet day, NOT a no-op.
	store := &fakeStore{count: 0}
	r := NewRunner(fakeCaller{text: "A calm, open day — nothing pressing."}, store, "test/model")
	if err := r.Run(context.Background(), cmd()); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if len(store.recorded) != 1 {
		t.Fatalf("a quiet day must still record a note, got %d", len(store.recorded))
	}
}

func TestExample_EmptyOutputRedelivers(t *testing.T) {
	store := &fakeStore{count: 1}
	r := NewRunner(fakeCaller{text: "   "}, store, "test/model")
	err := r.Run(context.Background(), cmd())
	if err == nil {
		t.Fatalf("empty model output must error (redeliver), not persist")
	}
	if len(store.recorded) != 0 {
		t.Fatalf("must not record an empty note")
	}
}

func TestExample_ThrottleSurfaces(t *testing.T) {
	store := &fakeStore{count: 1}
	r := NewRunner(fakeCaller{err: fmt.Errorf("429: %w", agentkit.ErrThrottled)}, store, "test/model")
	err := r.Run(context.Background(), cmd())
	if !errors.Is(err, agentkit.ErrThrottled) {
		t.Fatalf("throttle must surface so the caller can release the work, got %v", err)
	}
}

func TestExample_RunBounded(t *testing.T) {
	// The whole agent runs through one bounded entrypoint with no per-agent plumbing.
	store := &fakeStore{count: 2}
	r := NewRunner(fakeCaller{text: "Two things on deck today."}, store, "test/model")
	if err := r.RunBounded(context.Background(), cmd()); err != nil {
		t.Fatalf("RunBounded err: %v", err)
	}
	if len(store.recorded) != 1 {
		t.Fatalf("expected one recorded note, got %d", len(store.recorded))
	}
}
