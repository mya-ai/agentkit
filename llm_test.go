package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// This file exists because it did not, and the absence was measurable. A mutation
// audit of the whole suite found five surviving mutations; four were in llm.go —
// the seam every loop depends on — because tests live beside their source and
// nobody had created this file. The fallback model could be DELETED OUTRIGHT and
// the suite stayed green.
//
// ⭐ The lesson generalizes: a missing test FILE is invisible in a way a missing
// test case is not. Nothing lists the files you did not write.

// recordingCaller records the exact sequence of models it was asked for, so a test
// can assert ORDER — which is what pins the retry-then-fallback contract. Each
// response is returned in turn; running past the end is an explicit failure rather
// than a silent zero value.
type recordingCaller struct {
	responses []string // returned in order, one per call
	errs      []error  // parallel to responses; nil means "no error"
	models    []string // what CompleteJSON was actually asked for, in order
}

func (c *recordingCaller) CompleteJSON(_ context.Context, req StructuredRequest) (string, error) {
	i := len(c.models)
	c.models = append(c.models, req.Model)
	if i >= len(c.responses) {
		return "", fmt.Errorf("caller invoked %d time(s); only %d response(s) scripted", i+1, len(c.responses))
	}
	if i < len(c.errs) && c.errs[i] != nil {
		return "", c.errs[i]
	}
	return c.responses[i], nil
}

type verdict struct {
	Answer string `json:"answer"`
}

// ⭐ THE MUTATION THIS KILLS. Deleting the fallback append, or reordering the
// attempt list to try the fallback first, both survived the entire suite. A
// contributor "simplifying" GenerateStructured could remove a documented public
// field's only behaviour and ship green — the failure would surface in production
// as a degraded parse rate nobody attributes to the change.
//
// Asserting on the ORDERED slice of models is what makes this test load-bearing:
// a set-membership check would pass the reordering mutant.
func TestGenerateStructured_AttemptOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		responses  []string
		fallback   string
		wantModels []string
		wantAnswer string
		wantErr    error
	}{
		{
			name:       "primary parses first time — the fallback is never called",
			responses:  []string{`{"answer":"ok"}`},
			fallback:   "fallback-model",
			wantModels: []string{"primary-model"},
			wantAnswer: "ok",
		},
		{
			name:       "a parse miss retries the SAME model before falling back",
			responses:  []string{`not json`, `{"answer":"second try"}`},
			fallback:   "fallback-model",
			wantModels: []string{"primary-model", "primary-model"},
			wantAnswer: "second try",
		},
		{
			name:       "primary exhausted, THEN the fallback — in that order",
			responses:  []string{`not json`, `still not json`, `{"answer":"from fallback"}`},
			fallback:   "fallback-model",
			wantModels: []string{"primary-model", "primary-model", "fallback-model"},
			wantAnswer: "from fallback",
		},
		{
			name:       "no fallback configured — the primary's attempts are all there is",
			responses:  []string{`not json`, `still not json`},
			fallback:   "",
			wantModels: []string{"primary-model", "primary-model"},
			wantErr:    ErrUnparseableStructuredOutput,
		},
		{
			name:       "nothing parses anywhere — unparseable, not a transport error",
			responses:  []string{`a`, `b`, `c`},
			fallback:   "fallback-model",
			wantModels: []string{"primary-model", "primary-model", "fallback-model"},
			wantErr:    ErrUnparseableStructuredOutput,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingCaller{responses: tc.responses}
			got, err := GenerateStructured[verdict](context.Background(), c, StructuredRequest{
				Model: "primary-model", Fallback: tc.fallback,
			})

			if len(c.models) != len(tc.wantModels) {
				t.Fatalf("called %d time(s) %v, want %d %v", len(c.models), c.models, len(tc.wantModels), tc.wantModels)
			}
			for i, want := range tc.wantModels {
				if c.models[i] != want {
					t.Errorf("attempt %d asked for %q, want %q (full order: %v)", i+1, c.models[i], want, c.models)
				}
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Answer != tc.wantAnswer {
				t.Errorf("Answer = %q, want %q", got.Answer, tc.wantAnswer)
			}
		})
	}
}

// primaryAttempts is a constant with a stated rationale ("a single retry on the
// same model almost always lands it"). Dropping it to 1 survived the suite.
func TestGenerateStructured_PrimaryIsTriedExactlyTwice(t *testing.T) {
	c := &recordingCaller{responses: []string{`x`, `y`, `{"answer":"z"}`}}
	if _, err := GenerateStructured[verdict](context.Background(), c, StructuredRequest{
		Model: "primary-model", Fallback: "fallback-model",
	}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var primaries int
	for _, m := range c.models {
		if m == "primary-model" {
			primaries++
		}
	}
	if primaries != primaryAttempts {
		t.Errorf("primary tried %d time(s), want primaryAttempts=%d (order: %v)", primaries, primaryAttempts, c.models)
	}
}

// ⚠️ A THROTTLE IS NOT A PARSE MISS. It must short-circuit — no retry, no fallback
// — so the caller can hand the work back to its queue instead of burning attempts
// against a provider that is already shedding load. Retrying here holds a worker
// slot and worsens the contention that caused the throttle.
func TestGenerateStructured_ThrottleShortCircuits(t *testing.T) {
	c := &recordingCaller{
		responses: []string{"", `{"answer":"never reached"}`, `{"answer":"nor this"}`},
		errs:      []error{fmt.Errorf("429: %w", ErrThrottled)},
	}
	_, err := GenerateStructured[verdict](context.Background(), c, StructuredRequest{
		Model: "primary-model", Fallback: "fallback-model",
	})
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("err = %v, want it to wrap ErrThrottled", err)
	}
	if len(c.models) != 1 {
		t.Errorf("a throttle made %d attempt(s) %v — it must short-circuit after ONE", len(c.models), c.models)
	}
}

// A non-throttle transport error is also terminal: it is not a parse miss, so
// retrying the same model buys nothing.
func TestGenerateStructured_TransportErrorIsTerminal(t *testing.T) {
	boom := errors.New("connection reset")
	c := &recordingCaller{responses: []string{"", `{"answer":"never reached"}`}, errs: []error{boom}}
	_, err := GenerateStructured[verdict](context.Background(), c, StructuredRequest{
		Model: "primary-model", Fallback: "fallback-model",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the transport error surfaced", err)
	}
	if len(c.models) != 1 {
		t.Errorf("made %d attempt(s) %v — a transport error must not retry", len(c.models), c.models)
	}
}

// The System/Dynamic split and the tuning knobs must reach the caller unaltered —
// an adapter cannot honour what it never receives.
func TestGenerateStructured_PassesTheRequestThrough(t *testing.T) {
	var got StructuredRequest
	c := callerFunc(func(_ context.Context, req StructuredRequest) (string, error) {
		got = req
		return `{"answer":"ok"}`, nil
	})
	want := StructuredRequest{
		Model: "m", Fallback: "f", PromptType: "label", System: "rules", Dynamic: "data",
		MaxTokens: 512, Temperature: 0.3, Region: "somewhere",
	}
	if _, err := GenerateStructured[verdict](context.Background(), c, want); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.System != want.System || got.Dynamic != want.Dynamic {
		t.Errorf("System/Dynamic = %q/%q, want %q/%q", got.System, got.Dynamic, want.System, want.Dynamic)
	}
	if got.MaxTokens != want.MaxTokens || got.Temperature != want.Temperature ||
		got.Region != want.Region || got.PromptType != want.PromptType {
		t.Errorf("knobs did not pass through: %+v", got)
	}
}

type callerFunc func(context.Context, StructuredRequest) (string, error)

func (f callerFunc) CompleteJSON(ctx context.Context, r StructuredRequest) (string, error) {
	return f(ctx, r)
}

// ⭐ THE ASYMMETRY THIS PINS, WHICH ONLY A COMMENT DEFENDED. The closing fence is
// stripped ONLY when an opening fence was present. Making it unconditional
// survived the whole suite — and it is a tempting "fix", because the asymmetry
// looks like an oversight until you see this case.
//
// A model returning UNFENCED JSON whose string value contains a markdown code
// block is common (any "draft" or "summary" field). Cutting at the last ``` then
// truncates the JSON and corrupts the payload.
func TestExtractJSON_ClosingFenceOnlyStrippedWhenOpened(t *testing.T) {
	t.Run("unfenced JSON containing backticks survives intact", func(t *testing.T) {
		raw := "{\"answer\":\"run ```go\\nx := 1\\n``` to start\"}"
		got := ExtractJSON(raw)

		var v verdict
		if err := json.Unmarshal([]byte(got), &v); err != nil {
			t.Fatalf("⛔ the extracted text no longer parses — the payload was truncated at a backtick fence.\n"+
				"    in:  %s\n    out: %s\n    err: %v", raw, got, err)
		}
		if !strings.Contains(v.Answer, "```go") {
			t.Errorf("the code block inside the value was mangled: %q", v.Answer)
		}
	})

	for _, tc := range []struct{ name, in, want string }{
		{"```json fence is stripped", "```json\n{\"answer\":\"a\"}\n```", `{"answer":"a"}`},
		{"bare ``` fence is stripped", "```\n{\"answer\":\"a\"}\n```", `{"answer":"a"}`},
		{"trailing prose after the object is dropped", `{"answer":"a"} Hope that helps!`, `{"answer":"a"}`},
		{"a JSON array is extracted too", `[{"answer":"a"}]`, `[{"answer":"a"}]`},
		{"no JSON delimiters — returned as-is", `I could not answer that.`, `I could not answer that.`},
		{"empty input stays empty", ``, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractJSON(tc.in); got != tc.want {
				t.Errorf("ExtractJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The error text is a diagnostic surface: it must carry WHY it failed and a
// bounded look at the offending output, without dumping the whole payload.
func TestGenerateStructured_UnparseableErrorCarriesAPreview(t *testing.T) {
	junk := strings.Repeat("x", 900) + "TAIL_MARKER"
	c := &recordingCaller{responses: []string{junk, junk}}
	_, err := GenerateStructured[verdict](context.Background(), c, StructuredRequest{Model: "m"})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TAIL_MARKER") {
		t.Errorf("the preview must show the END of the output, where truncation shows up; got %q", msg)
	}
	if len(msg) > 900 {
		t.Errorf("the preview must stay bounded — this lands in logs; got %d chars", len(msg))
	}
}

// ⛔ THE BUG THIS PINS, AND WHY A LAST-BRACE SEARCH CANNOT WORK. ExtractJSON used to
// take everything from the first "{" to the LAST "}". A pre-publication review found
// that a brace inside a STRING VALUE, or in the prose a model appends afterwards,
// breaks that span — and this is the function every structured call runs through.
//
// The failure was safe but expensive: an unparseable span burns both primary
// attempts AND the fallback hop before surfacing, so one stray brace costs three
// model calls and a failed run.
//
// ⭐ Braces in prose and in string values are not exotic. "the build failed at 80% }
// complete" is the kind of sentence a model writes unprompted.
func TestExtractJSON_ScansStructureNotTheLastBrace(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			name: "a brace inside a string value is DATA, not structure",
			in:   `{"summary":"the build failed at 80% } complete"} Hope that helps!`,
			want: `{"summary":"the build failed at 80% } complete"}`,
		},
		{
			name: "a brace in the trailing prose is not the close",
			in:   `{"ok":true} Let me know if that helps :}`,
			want: `{"ok":true}`,
		},
		{
			name: "the FIRST complete value wins when the model emits two",
			in:   `{"a":1} and also {"b":2}`,
			want: `{"a":1}`,
		},
		{
			name: "a bracket in prose after an array is not the close",
			in:   `[{"id":"x"}] see the list above ]`,
			want: `[{"id":"x"}]`,
		},
		{
			name: "an ESCAPED quote does not end the string",
			in:   `{"quote":"she said \"done }\" and left"} trailing`,
			want: `{"quote":"she said \"done }\" and left"}`,
		},
		{
			name: "nested structure closes at the OUTER delimiter",
			in:   `{"outer":{"inner":{"deep":1}}} tail`,
			want: `{"outer":{"inner":{"deep":1}}}`,
		},
		{
			name: "an escaped backslash before a quote still ends the string",
			in:   `{"path":"C:\\"} after`,
			want: `{"path":"C:\\"}`,
		},
		{
			name: "leading prose before the value is dropped",
			in:   `Sure! Here you go: {"a":1}`,
			want: `{"a":1}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractJSON(tc.in)
			if got != tc.want {
				t.Errorf("ExtractJSON(%s)\n  got  %s\n  want %s", tc.in, got, tc.want)
			}
			// The point is not the span — it is that the span PARSES.
			var v any
			if err := json.Unmarshal([]byte(got), &v); err != nil {
				t.Errorf("⛔ the extracted span does not parse: %v\n  span: %s", err, got)
			}
		})
	}
}

// ⚠️ A TRUNCATED VALUE MUST NOT LOOK LIKE A PARSE ERROR IN THE PREFIX. When a model
// hits its token cap mid-object there is no closing brace at all. Returning from the
// opening delimiter means the caller's error names the truncation, rather than
// complaining about prose the model wrote before the JSON started.
func TestExtractJSON_UnbalancedReturnsFromTheOpener(t *testing.T) {
	got := ExtractJSON(`Here you go: {"summary":"it was cut off mid`)
	if !strings.HasPrefix(got, `{"summary"`) {
		t.Errorf("a truncated value must start at the opening delimiter; got %q", got)
	}
	if strings.Contains(got, "Here you go") {
		t.Errorf("the leading prose must still be dropped; got %q", got)
	}
}

// ⛔ THE INCIDENT THIS DOCUMENTS. ExtractJSON on PROSE is destructive: markdown is
// full of JSON delimiters, so a briefing containing a `[link](url)` bullet returns
// "[link]" and the rest of the document is thrown away. That shipped once and was
// caught only in local testing against real data.
//
// ⭐ This test asserts the DAMAGE, not a guard — because the fix cannot live in this
// function. ExtractJSON cannot tell prose from a JSON response by looking; only the
// CALLER knows which it asked for. The kit gets this right structurally
// (GenerateStructured calls it, RunOnceText does not), and an adapter serving both
// must branch on the REQUEST, never on what the text looks like.
//
// If someone later "fixes" this by adding prose detection here, that is a mistake:
// a heuristic that guesses wrong on a JSON response silently corrupts data, which is
// worse than the caller keeping a flag it already has.
func TestExtractJSON_OnProseIsDestructive_CallerMustNotUseIt(t *testing.T) {
	briefing := "# Today\n\n- Meeting at 10am, see [link](https://example.com/x)\n- Ship the release\n"

	got := ExtractJSON(briefing)

	if got == briefing {
		t.Fatal("ExtractJSON no longer mangles prose — if that is deliberate, this test and the ⛔ " +
			"note on ExtractJSON must be rewritten together; if it is accidental prose-detection, " +
			"revert it: guessing wrong on a JSON response corrupts data silently")
	}
	if got != "[link]" {
		t.Errorf("the documented failure returns %q; got %q — the doc comment cites this exact "+
			"example and must be updated if the behaviour moved", "[link]", got)
	}
}
