package agentkit

import (
	"context"
	"fmt"
	"io"
)

// RunLocalPair runs a Pair loop and prints each round's outcome to w — NO queue, NO apply. First-agent
// debugging is mostly "why did the loop stalemate?"; this surfaces the rounds, the final outcome, and the
// result/feedback so a developer can iterate on prompts + the judge against a real caller (or a fake)
// from a small main()/test, before any handler or queue wiring exists.
//
// It is a thin wrapper over Pair that logs; the loop semantics are identical. w nil → os.Stdout-less
// (discard) is the caller's choice; pass a *bytes.Buffer in a test or os.Stdout in a throwaway main.
func RunLocalPair[Ctx any, V Material](
	ctx context.Context,
	w io.Writer,
	caller StructuredCaller,
	exec OnceAgent[Ctx, V],
	judge Verifier[V],
	same SameFunc[V],
	rc Ctx,
	cfg LoopConfig,
) Result[V] {
	res := Pair[Ctx, V](ctx, caller, exec, judge, same, rc, cfg, nil)
	if w != nil {
		logf(w, "[agentkit/RunLocal] Pair outcome=%s rounds=%d\n", res.Outcome, res.Rounds)
		switch res.Outcome {
		case OutcomeApplied:
			logf(w, "  applied result: %+v\n", res.Result)
		case OutcomeSilent:
			logf(w, "  silent (non-material): %+v\n", res.LastResult)
		case OutcomeStalemate:
			// Stalemate is the degeneration case (executor stuck resubmitting); the result is the executor's
			// last output. StalemateFeedback is deliberately nil (the kit never surfaces judge text — see
			// Result doc), so we don't print it: it would always read "[]" and mislead.
			logf(w, "  stalemate (degeneration); executor's last result: %+v\n", res.LastResult)
		case OutcomeError:
			logf(w, "  error: %v\n", res.Err)
		}
	}
	return res
}

// logf is the debug-print helper: best-effort, discards the write error (RunLocal is a developer debug
// aid, not a production path — a failed debug write is not actionable).
func logf(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// RunLocalOnce runs a single RunOnce and prints the result to w — NO queue, NO apply. The debug entrypoint
// for a RunOnce agent (e.g. a daily digest): see what the one structured call returns before any handler.
func RunLocalOnce[Ctx, V any](
	ctx context.Context,
	w io.Writer,
	caller StructuredCaller,
	agent OnceAgent[Ctx, V],
	rc Ctx,
) (V, error) {
	v, err := RunOnce[Ctx, V](ctx, caller, agent, rc, nil, nil)
	if w != nil {
		if err != nil {
			logf(w, "[agentkit/RunLocal] RunOnce error: %v\n", err)
		} else {
			logf(w, "[agentkit/RunLocal] RunOnce result: %+v\n", v)
		}
	}
	return v, err
}
