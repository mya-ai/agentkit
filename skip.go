package agentkit

// Skip and its sentinel express an outcome that "return nil" cannot: the work was
// legitimately NOT DONE, and that is not the same as done.
//
// ⚠️ THE FAILURE THIS EXISTS TO PREVENT. Without it, a runner that decides it has
// nothing to do can only signal so by returning nil — which every caller classifies
// as SUCCESS. The job is counted as processed, and a real outage (every unit
// silently skipped because a dependency was unwired) reads as healthy throughput on
// the dashboard. Nothing fires. The graph looks fine.
//
// Return Skip(reason) instead, and have your caller unwrap it: acknowledge the work
// (it is not an error, so do not redeliver it) but record it as SKIPPED with the
// reason, not as processed.
//
//	if !r.ready() {
//	    return agentkit.Skip("deps_unwired")
//	}
//
//	// in your queue consumer / scheduler:
//	if reason, ok := agentkit.IsSkip(err); ok {
//	    metrics.Skipped(reason)
//	    return nil // ack: skipping is a legitimate outcome, not a failure
//	}

// skipError carries the reason a unit of work was skipped.
type skipError struct{ reason string }

func (e skipError) Error() string { return "agentkit: skip (" + e.reason + ")" }

// Skip returns the sentinel a runner returns to acknowledge a unit of work WITHOUT
// having done it. reason should be a low-cardinality label suitable for a metric —
// "deps_unwired", "not_eligible", "already_current" — never a formatted message
// with an id in it, which would explode a metrics label set.
func Skip(reason string) error { return skipError{reason: reason} }

// IsSkip reports whether err is a Skip sentinel, and returns its reason. It is the
// half a caller needs: a skip must be ACKNOWLEDGED (it is not a failure) but must
// NOT be counted as processed.
//
// ⚠️ RETURN A Skip BARE. It does NOT survive wrapping — `fmt.Errorf("unit %s: %w",
// id, agentkit.Skip("x"))` does not read as a skip, and the work is redelivered
// instead. That is a deliberate narrowing, not an oversight: a skip is a TERMINAL
// decision by the runner about work it examined and chose not to do, and a decision
// that survives being wrapped in three layers of context is no longer terminal. If
// you want to say WHY, the reason string is the place: Skip("deps_unwired").
//
// The failure direction is the safe one — a missed skip becomes a redelivery, which
// is loud and self-correcting, rather than a silent ack that drops the work. But
// the obligation is real, so honour it at the call site: return Skip bare, or
// forfeit skip handling.
func IsSkip(err error) (reason string, ok bool) {
	if e, is := err.(skipError); is {
		return e.reason, true
	}
	return "", false
}
