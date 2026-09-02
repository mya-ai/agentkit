package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This file is the kit's LLM SEAM: the contract agentkit needs from a language
// model, and nothing more. It is deliberately provider-agnostic — there is no
// SDK here, no HTTP client, no credentials, no retries against a specific API.
//
// ⭐ THE DIRECTION MATTERS. A loop primitive that cannot call a model is not a
// loop primitive, so the kit MUST reach an LLM. The question is which side owns
// the type. If the kit imported a provider package, every consumer would inherit
// that provider (and its cloud SDK, metrics stack and service clients) whether
// they used it or not. So the kit DECLARES the contract and the provider
// implements it — one small interface, injected by the consumer:
//
//	type StructuredCaller interface {
//	    CompleteJSON(ctx context.Context, req StructuredRequest) (string, error)
//	}
//
// Implement that against OpenAI, Anthropic, Bedrock, Vertex, Ollama or a fake in
// a test, and the whole kit runs on it. See docs/providers.md for worked
// adapters, and scripts/check-deps.sh for the tripwire that keeps this honest.

// ErrThrottled marks an LLM error as a provider rate-limit / capacity signal, as
// opposed to a bad request or an unusable response. Your CompleteJSON (or
// GenerateText) implementation should wrap it — `fmt.Errorf("...: %w",
// agentkit.ErrThrottled)` — when the provider returns a 429 or equivalent.
//
// It is load-bearing, not cosmetic: a throttle is the one error the kit does NOT
// retry or fall back on. It short-circuits so the caller can hand the work back
// to its queue and let the queue be the backoff, rather than burning a retry hop
// while holding a worker slot and worsening the contention that caused it.
// Callers branch on errors.Is(err, agentkit.ErrThrottled).
var ErrThrottled = errors.New("llm provider throttled")

// ErrUnparseableStructuredOutput marks a structured call that COMPLETED — no
// throttle, no transport error — but never produced JSON that unmarshals into the
// requested type, across the primary attempts AND the fallback.
//
// It is a distinct failure class from ErrThrottled and callers should treat it as
// one: a parse failure means the model's output was unusable (retry, escalate, or
// drop the work), whereas a throttle means the provider was busy (release the
// work and let it come back).
var ErrUnparseableStructuredOutput = errors.New("llm structured output did not parse")

// StructuredRequest is one schema-bound generation.
//
// System vs Dynamic is the kit's prompt split, and it is worth honoring: System
// carries the stable rules/format contract, Dynamic carries the per-call data and
// the imperative. Providers that support a system-instruction slot should map
// System onto it — that is what makes prompt caching effective, since the stable
// half is identical across calls.
//
// Implementations are expected to run the model in JSON mode where the provider
// offers one (Gemini's application/json response type, an Anthropic tool schema,
// OpenAI's response_format) so the returned text is JSON rather than prose.
type StructuredRequest struct {
	Model       string  // primary model ID, in whatever vocabulary your caller understands
	Fallback    string  // model tried after the primary fails to PARSE; "" disables the hop
	PromptType  string  // a metrics/log label, e.g. "review" — never sent to the model
	System      string  // stable rules + output contract
	Dynamic     string  // per-call data and the imperative
	MaxTokens   int     // output cap
	Temperature float64 // sampling temperature
	Region      string  // optional provider region/location; "" = the implementation's default
}

// StructuredCaller is the narrow seam the kit depends on for JSON generation: a
// single JSON-mode completion for a given model. This one method is the entire
// obligation — implement it and every structured path in the kit works.
//
// CompleteJSON returns the model's RAW TEXT; the kit extracts and unmarshals the
// JSON itself (models wrap JSON in markdown fences and append prose no matter how
// firmly they are told not to, so that normalization belongs in one place rather
// than in every adapter). Return an ErrThrottled-wrapped error on a provider 429
// so the kit can surface the throttle instead of retrying into it.
type StructuredCaller interface {
	CompleteJSON(ctx context.Context, req StructuredRequest) (string, error)
}

// GenerateTextRequest is one PROSE generation — markdown or plain text, no schema.
// Same System/Dynamic split as StructuredRequest; there is no Fallback because
// there is no parse step to fail.
type GenerateTextRequest struct {
	Model       string
	PromptType  string
	System      string
	Dynamic     string
	MaxTokens   int
	Temperature float64
	Region      string
}

// LLMCallResult is what a prose call returns.
type LLMCallResult struct {
	Text         string
	InputTokens  int
	OutputTokens int
	TookMs       int64

	// FinishReason is the provider's stop reason (STOP, MAX_TOKENS, SAFETY, …),
	// empty when the provider does not report one.
	//
	// ⚠️ Populate it if you can. It answers "was this body truncated, or did the
	// model refuse?" — a question that gets asked at the JSON parse site, which
	// otherwise sees only an unparseable string and cannot tell the difference.
	FinishReason string
}

// TextCaller is the prose counterpart to StructuredCaller. Only implement it if
// your agent generates prose; the structured path does not need it.
type TextCaller interface {
	GenerateText(ctx context.Context, req GenerateTextRequest) (LLMCallResult, error)
}

// primaryAttempts is how many times GenerateStructured calls the PRIMARY model
// before falling back. A forced-schema drop is intermittent across providers, so
// a single retry on the same model almost always lands it; two total attempts
// balances reliability against latency and cost.
const primaryAttempts = 2

// GenerateStructured runs a schema-bound LLM call and returns a validated T.
//
// Flow: try the primary model up to primaryAttempts times (a parse miss is retried
// on the SAME model — the drop is intermittent), then the Fallback model once if
// one is set. A throttle short-circuits immediately, NOT retried and NOT fallen
// back, wrapping ErrThrottled. A genuine transport error also short-circuits. If
// nothing parses across every attempt, returns the zero T and
// ErrUnparseableStructuredOutput.
//
// It is a free generic function rather than a method so ANY StructuredCaller — a
// real client or a fake — can drive it: the caller owns the loop and the budget,
// the implementation owns "here is one JSON completion".
func GenerateStructured[T any](ctx context.Context, caller StructuredCaller, req StructuredRequest) (T, error) {
	var zero T

	// Ordered attempt list: primary ×N, then fallback ×1.
	attempts := make([]string, 0, primaryAttempts+1)
	for i := 0; i < primaryAttempts; i++ {
		attempts = append(attempts, req.Model)
	}
	if req.Fallback != "" {
		attempts = append(attempts, req.Fallback)
	}

	var lastText string
	var lastParseErr error
	for _, model := range attempts {
		callReq := req
		callReq.Model = model
		text, err := caller.CompleteJSON(ctx, callReq)
		if err != nil {
			// A throttle is a capacity signal — surface it untouched so the caller
			// releases the work rather than burning a retry/fallback hop (which
			// holds a worker slot and worsens the contention). Any other transport
			// error is terminal for this call too.
			return zero, err
		}
		lastText = text

		if jsonText := ExtractJSON(text); jsonText != "" {
			var out T
			uerr := json.Unmarshal([]byte(jsonText), &out)
			if uerr == nil {
				return out, nil
			}
			lastParseErr = uerr
		}
		// Parse miss — fall through to the next attempt. Do NOT return here; only
		// an exhausted attempt list is an error.
	}

	// Include the parse error and a bounded preview of the offending text so
	// operators and evals can see WHY it failed (truncation, trailing comma, an
	// unescaped character) rather than just a byte count.
	return zero, fmt.Errorf("%w (%d attempts, last parse err: %v; preview: %s)",
		ErrUnparseableStructuredOutput, len(attempts), lastParseErr, previewText(lastText))
}

// previewText returns a bounded, single-line preview of the END of the model
// output — where truncation and trailing-comma errors surface — for diagnostics.
// Never the whole payload: this lands in logs.
func previewText(s string) string {
	const maxLen = 400
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return "…" + s[len(s)-maxLen:]
}

// ExtractJSON pulls the first complete JSON value out of model output, stripping
// markdown code fences and any prose the model appended after it. If no JSON
// delimiters are found it returns the stripped text as-is.
//
// ⚠️ IT SCANS WITH A DEPTH COUNTER, NOT A LAST-BRACE SEARCH, and it must. An earlier
// version took everything from the first "{" to the LAST "}", which breaks on output
// no more exotic than this:
//
//	{"summary":"the build failed at 80% } complete"} Hope that helps!
//
// The brace inside the string value is not structure, and the "}" in the trailing
// prose is not the close — so the naive span swallowed the prose and the whole call
// failed to parse. Models put braces in prose and in string values routinely; a
// scanner that respects string literals and escapes is the only thing that survives
// contact with real output.
//
// ⛔ NEVER RUN IT ON PROSE. It looks for the first JSON delimiter, and markdown is
// full of them: a briefing containing "- see [link](https://…)" returns "[link]" and
// the rest of the document is DISCARDED. That is not hypothetical — it cost a real
// incident, found only in local testing against real data.
//
// The rule is the CALLER's, because only the caller knows what it asked for: run
// this when you requested JSON, and return the model's text verbatim when you
// requested prose. GenerateStructured (JSON) calls it; RunOnceText (prose) does not.
// If your adapter serves both, branch on which you asked for — do not branch on what
// the text looks like.
//
// It is exported because provider adapters and tests need exactly this
// normalization, and re-deriving it is how subtle bugs get in (the fence-stripping
// is asymmetric on purpose — see stripMarkdownFences).
func ExtractJSON(text string) string {
	text = strings.TrimSpace(stripMarkdownFences(text))
	if text == "" {
		return text
	}
	start := strings.IndexAny(text, "{[")
	if start == -1 {
		return text
	}

	opener := text[start]
	closer := byte('}')
	if opener == '[' {
		closer = ']'
	}

	var depth int
	var inString, escaped bool
	for i := start; i < len(text); i++ {
		c := text[i]

		// Inside a string literal, only the escape state and the closing quote
		// matter — braces and brackets there are DATA, not structure.
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		switch c {
		case '"':
			inString = true
		case opener:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return text[start : i+1] // the first COMPLETE value; prose after it is dropped
			}
		}
	}

	// Unbalanced — the model was cut off mid-value (a MaxTokens truncation, most
	// often). Return from the opening delimiter so the caller's parse error names a
	// truncation rather than a stray prefix.
	return text[start:]
}

// stripMarkdownFences removes markdown code-fence wrappers from model output.
// Models wrap JSON in ```json … ``` despite explicit instructions not to.
//
// ⚠️ The closing fence is stripped ONLY when an opening fence was present. That
// asymmetry is deliberate: a JSON string value can legitimately contain triple
// backticks (a draft containing a code block), and unconditionally cutting at the
// last ``` would corrupt it.
func stripMarkdownFences(text string) string {
	text = strings.TrimSpace(text)

	hadFence := false
	if strings.HasPrefix(text, "```json") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "```json"))
		hadFence = true
	} else if strings.HasPrefix(text, "```") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "```"))
		hadFence = true
	}

	if hadFence {
		if idx := strings.LastIndex(text, "```"); idx != -1 {
			text = strings.TrimSpace(text[:idx])
		}
	}

	return text
}
