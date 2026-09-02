# Bringing your own LLM provider

agentkit ships **no** provider client. It declares the contract it needs and you supply the
implementation — which is why importing the kit pulls one external dependency instead of a cloud SDK.

This page has copy-pasteable adapters. All of them implement the same interface, declared in
[`llm.go`](../llm.go):

```go
type StructuredCaller interface {
    CompleteJSON(ctx context.Context, req StructuredRequest) (string, error)
}
```

That is the entire obligation for the structured path. Implement it and `RunOnce`, `Pair`,
`RunToolLoop` and `RunToolLoopTiered` all work.

---

## The contract, precisely

| You receive | You must |
|---|---|
| `req.System` | the stable rules + output contract — map onto the provider's **system instruction** slot if it has one (this is what makes prompt caching effective) |
| `req.Dynamic` | the per-call data and imperative — the user-role content |
| `req.Model` | your model ID, in whatever vocabulary you chose |
| `req.MaxTokens`, `req.Temperature`, `req.Region` | pass through; `Region` may be ignored |
| `req.PromptType` | a metrics/log label — **never send it to the model** |

**Return the raw text.** Do not parse the JSON — the kit extracts and unmarshals it, because models
wrap JSON in markdown fences and append trailing prose no matter how firmly they are instructed not
to. Centralizing that normalization is why `ExtractJSON` is exported rather than copied per adapter.

⛔ **If one adapter serves BOTH `CompleteJSON` and `GenerateText`, branch on which was called — never
on what the text looks like.** `ExtractJSON` on prose is destructive: markdown is full of JSON
delimiters, so a document containing `- see [link](https://…)` comes back as `[link]` and the rest is
gone. That failure has shipped before, and it is invisible until someone reads the output. The kit
gets it right structurally — `GenerateStructured` normalizes, `RunOnceText` does not — and your
adapter needs the same discipline, using the request it already has rather than a guess.

**Wrap `ErrThrottled` on a 429.** This is the one error the kit treats specially:

```go
if resp.StatusCode == http.StatusTooManyRequests {
    return "", fmt.Errorf("provider throttled: %w", agentkit.ErrThrottled)
}
```

A throttle short-circuits — no retry, no fallback — so your caller can hand the work back to its
queue and let the queue be the backoff. Retrying in-process holds a worker slot and worsens the
contention that caused the throttle. Everything else is a normal error.

⚠️ **If you skip this, throttles look like parse failures**: the kit burns both primary attempts plus
the fallback hop against a provider that is already shedding load.

**Request JSON mode where the provider offers one** (`response_format`, `application/json`, a forced
tool schema). Without it you are relying on the prompt alone, and `ExtractJSON` will be doing more
work than it should.

---

## OpenAI

```go
package myllm

import (
	"context"
	"errors"
	"fmt"

	"github.com/mya-ai/agentkit"
	"github.com/openai/openai-go"
)

type OpenAICaller struct{ client openai.Client }

func (c OpenAICaller) CompleteJSON(ctx context.Context, req agentkit.StructuredRequest) (string, error) {
	resp, err := c.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: req.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(req.System),
			openai.UserMessage(req.Dynamic),
		},
		MaxTokens:   openai.Int(int64(req.MaxTokens)),
		Temperature: openai.Float(req.Temperature),
		// JSON mode: constrain the response so ExtractJSON has little to do.
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &openai.ResponseFormatJSONObjectParam{},
		},
	})
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 429 {
			// The kit releases the work instead of retrying into a busy provider.
			return "", fmt.Errorf("openai throttled: %w", agentkit.ErrThrottled)
		}
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}
```

## Anthropic

```go
type AnthropicCaller struct{ client anthropic.Client }

func (c AnthropicCaller) CompleteJSON(ctx context.Context, req agentkit.StructuredRequest) (string, error) {
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		// System is a top-level parameter here — exactly the split the kit already gives you.
		System:      []anthropic.TextBlockParam{{Text: req.System}},
		Temperature: anthropic.Float(req.Temperature),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Dynamic)),
		},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 429 {
			return "", fmt.Errorf("anthropic throttled: %w", agentkit.ErrThrottled)
		}
		return "", err
	}
	for _, block := range resp.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("anthropic: no text block in response")
}
```

For stricter JSON, define a tool whose `input_schema` is your output shape, set
`tool_choice` to force it, and return the marshalled tool input.

## Ollama (local models)

```go
type OllamaCaller struct{ baseURL string; http *http.Client }

func (c OllamaCaller) CompleteJSON(ctx context.Context, req agentkit.StructuredRequest) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  req.Model,
		"system": req.System,
		"prompt": req.Dynamic,
		"format": "json", // Ollama's JSON mode
		"stream": false,
		"options": map[string]any{
			"temperature": req.Temperature,
			"num_predict": req.MaxTokens,
		},
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// A local model has no quota, so there is no throttle case to map.
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama: status %d", resp.StatusCode)
	}
	var out struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Response, nil
}
```

---

## A fake, for tests

You do not need a provider to test an agent. This is the whole harness:

```go
type fakeCaller struct {
	responses []string // returned in order, one per call
	calls     int
	err       error
}

func (f *fakeCaller) CompleteJSON(ctx context.Context, req agentkit.StructuredRequest) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.calls >= len(f.responses) {
		return "", fmt.Errorf("fake: unexpected call %d", f.calls+1)
	}
	out := f.responses[f.calls]
	f.calls++
	return out, nil
}
```

Assert throttle handling by returning `fmt.Errorf("429: %w", agentkit.ErrThrottled)` and checking
your agent surfaces it with `errors.Is` rather than swallowing it.

See [`example/`](../example) for agents wired this way end to end.

---

## Prose generation

If your agent emits markdown rather than a typed struct, implement `TextCaller` as well:

```go
type TextCaller interface {
	GenerateText(ctx context.Context, req GenerateTextRequest) (LLMCallResult, error)
}
```

Same mapping, minus the JSON-mode request and the `Fallback` model. Populate
`LLMCallResult.FinishReason` if the provider reports one — it is what distinguishes "the model was
cut off at MAX_TOKENS" from "the model refused", a question that otherwise gets asked at the parse
site where only an unusable string is visible.

`RunOnceText` exists as a separate flavor because forcing prose through JSON mode wraps it in a
`{"description": "..."}` envelope. That was a real bug, not a hypothetical.

---

## Model IDs, prompts and regions

`req.Model` is whatever string your adapter understands — the kit never interprets it. Which model a
given (agent, role) uses is resolved through [`TemplateProvider`](../prompts.go), so it is
configuration rather than code:

```go
provider := agentkit.NewMapTemplateProvider(map[string]map[string]agentkit.PromptOverride{
    "review": {
        "worker":   {Model: "gpt-4o-mini"},
        "reviewer": {Model: "claude-sonnet-4", Template: customReviewerPrompt},
    },
})
```

An unset field means "use the agent's embedded default", and a nil provider means "use embedded
defaults for everything" — the zero-config path for local runs and evals. Load overrides from a file,
your config service, or environment variables at startup, and hot-swap them with `Set`.
