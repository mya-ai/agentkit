package agentkit

import (
	"strconv"
	"sync"
	"testing"
)

func TestMapTemplateProvider_NilIsEmbeddedDefaults(t *testing.T) {
	// A nil provider is the zero-config path (local runs, evals): every lookup must
	// return "" so the agent falls back to its embedded default rather than panicking.
	var p *MapTemplateProvider
	if got := p.Template("review", "worker"); got != "" {
		t.Fatalf("nil provider Template = %q, want %q", got, "")
	}
	if got := p.Model("review", "worker"); got != "" {
		t.Fatalf("nil provider Model = %q, want %q", got, "")
	}
	if got := p.Region("review", "worker"); got != "" {
		t.Fatalf("nil provider Region = %q, want %q", got, "")
	}
}

func TestMapTemplateProvider_ZeroValueUsable(t *testing.T) {
	// Documented in the type comment: the zero value is ready to Set into.
	var p MapTemplateProvider
	p.Set("review", "worker", PromptOverride{Template: "T", Model: "M", Region: "R"})
	if got := p.Template("review", "worker"); got != "T" {
		t.Fatalf("Template = %q, want T", got)
	}
}

func TestMapTemplateProvider_KeyedLookup(t *testing.T) {
	p := NewMapTemplateProvider(map[string]map[string]PromptOverride{
		"review": {
			"worker":   {Template: "WORKER_TMPL", Model: "WORKER_MODEL", Region: "WORKER_REGION"},
			"reviewer": {Template: "REVIEWER_TMPL", Model: "REVIEWER_MODEL"},
		},
		"digest": {"main": {Model: "DIGEST_MODEL"}},
	})

	cases := []struct{ agent, role, tmpl, model, region string }{
		{"review", "worker", "WORKER_TMPL", "WORKER_MODEL", "WORKER_REGION"},
		{"review", "reviewer", "REVIEWER_TMPL", "REVIEWER_MODEL", ""},
		{"digest", "main", "", "DIGEST_MODEL", ""},
	}
	for _, c := range cases {
		if got := p.Template(c.agent, c.role); got != c.tmpl {
			t.Errorf("Template(%s,%s) = %q, want %q", c.agent, c.role, got, c.tmpl)
		}
		if got := p.Model(c.agent, c.role); got != c.model {
			t.Errorf("Model(%s,%s) = %q, want %q", c.agent, c.role, got, c.model)
		}
		if got := p.Region(c.agent, c.role); got != c.region {
			t.Errorf("Region(%s,%s) = %q, want %q", c.agent, c.role, got, c.region)
		}
	}
}

func TestMapTemplateProvider_UnknownKeyFallsBack(t *testing.T) {
	// An unregistered (agent, role) means "no override" → the agent's embedded default.
	p := NewMapTemplateProvider(map[string]map[string]PromptOverride{
		"review": {"worker": {Template: "WORKER_TMPL"}},
	})
	if got := p.Template("unregistered", "worker"); got != "" {
		t.Fatalf("unknown agent = %q, want empty (embedded default)", got)
	}
	if got := p.Template("review", "unregistered"); got != "" {
		t.Fatalf("unknown role = %q, want empty (embedded default)", got)
	}
}

// ⭐ THE REGRESSION THIS LOCKS. The pre-extraction provider was a hardcoded switch
// over a fixed key list, so ADDING AN AGENT MEANT EDITING THE FRAMEWORK — and an
// unrecognized key returned "" indistinguishably from "no override configured".
// Registration is now data. If this test ever needs a kit edit to pass, that
// property has been lost.
func TestMapTemplateProvider_NewAgentNeedsNoKitChange(t *testing.T) {
	p := NewMapTemplateProvider(nil)
	p.Set("an-agent-the-kit-has-never-heard-of", "some-role",
		PromptOverride{Template: "TMPL", Model: "MODEL"})

	if got := p.Template("an-agent-the-kit-has-never-heard-of", "some-role"); got != "TMPL" {
		t.Fatalf("a newly registered agent must resolve; got %q", got)
	}
}

func TestTemplateProviderFunc_PartialAndNilFields(t *testing.T) {
	// Override only what you need; unset dimensions return "".
	f := TemplateProviderFunc{
		ModelFn: func(agent, role string) string { return agent + "/" + role },
	}
	if got := f.Model("review", "worker"); got != "review/worker" {
		t.Fatalf("Model = %q, want review/worker", got)
	}
	if got := f.Template("review", "worker"); got != "" {
		t.Fatalf("nil TemplateFn must return empty, got %q", got)
	}
	if got := f.Region("review", "worker"); got != "" {
		t.Fatalf("nil RegionFn must return empty, got %q", got)
	}
}

// A config watcher hot-swapping a prompt must not race readers. There is no
// assertion here on purpose: the race detector IS the assertion (go test -race).
func TestMapTemplateProvider_ConcurrentSetAndLookup(_ *testing.T) {
	p := NewMapTemplateProvider(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			p.Set("review", "worker", PromptOverride{Model: strconv.Itoa(n)})
		}(i)
		go func() {
			defer wg.Done()
			_ = p.Model("review", "worker")
		}()
	}
	wg.Wait()
}
