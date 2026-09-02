package agentkit

import (
	"context"
	"errors"
	"testing"
)

// recordingVerifier tracks whether it ran (to prove short-circuiting).
type recordingVerifier struct {
	ok     bool
	fb     []string
	err    error
	called *int
}

func (r recordingVerifier) Verify(_ context.Context, _ testVerdict) (bool, []string, error) {
	if r.called != nil {
		*r.called++
	}
	return r.ok, r.fb, r.err
}

func TestRulesVerifier_OkAndReject(t *testing.T) {
	okV := RulesVerifier(func(v testVerdict) (bool, []string) { return v.Plan != "", []string{"need a plan"} })
	ok, _, err := okV.Verify(context.Background(), testVerdict{Plan: "x"})
	if !ok || err != nil {
		t.Fatalf("expected ok, got ok=%v err=%v", ok, err)
	}
	ok, fb, err := okV.Verify(context.Background(), testVerdict{})
	if ok || err != nil || len(fb) == 0 {
		t.Fatalf("expected reject with feedback, got ok=%v fb=%v err=%v", ok, fb, err)
	}
}

func TestVerifierChain_ShortCircuitsOnFirstReject(t *testing.T) {
	// A rules reject must SHORT-CIRCUIT — the (expensive) second verifier never runs.
	secondCalls := 0
	chain := VerifierChain[testVerdict](
		RulesVerifier(func(testVerdict) (bool, []string) { return false, []string{"rules say no"} }),
		recordingVerifier{ok: true, called: &secondCalls},
	)
	ok, fb, err := chain.Verify(context.Background(), testVerdict{Material: true, Plan: "x"})
	if ok || err != nil {
		t.Fatalf("chain must reject when the first verifier rejects, got ok=%v err=%v", ok, err)
	}
	if len(fb) == 0 || fb[0] != "rules say no" {
		t.Fatalf("chain must return the first rejecter's feedback, got %v", fb)
	}
	if secondCalls != 0 {
		t.Fatalf("the LLM-tier verifier must NOT run after a rules reject (short-circuit), got %d calls", secondCalls)
	}
}

func TestVerifierChain_AllPass(t *testing.T) {
	secondCalls := 0
	chain := VerifierChain[testVerdict](
		RulesVerifier(func(testVerdict) (bool, []string) { return true, nil }),
		recordingVerifier{ok: true, called: &secondCalls},
	)
	ok, _, err := chain.Verify(context.Background(), testVerdict{Material: true, Plan: "x"})
	if !ok || err != nil {
		t.Fatalf("chain must approve when all pass, got ok=%v err=%v", ok, err)
	}
	if secondCalls != 1 {
		t.Fatalf("the second verifier must run when the first passes, got %d calls", secondCalls)
	}
}

func TestVerifierChain_ErrorIsTerminal(t *testing.T) {
	secondCalls := 0
	chain := VerifierChain[testVerdict](
		recordingVerifier{err: errors.New("judge throttle")},
		recordingVerifier{ok: true, called: &secondCalls},
	)
	_, _, err := chain.Verify(context.Background(), testVerdict{})
	if err == nil {
		t.Fatalf("an errored verifier must be terminal")
	}
	if secondCalls != 0 {
		t.Fatalf("no verifier runs after an error, got %d", secondCalls)
	}
}

func TestVerifierChain_EmptyApproves(t *testing.T) {
	ok, _, err := VerifierChain[testVerdict]().Verify(context.Background(), testVerdict{})
	if !ok || err != nil {
		t.Fatalf("an empty chain approves, got ok=%v err=%v", ok, err)
	}
}
