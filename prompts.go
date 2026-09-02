package agentkit

import "sync"

// TemplateProvider is the kit's keyed prompt/model/region resolver, looked up by
// (agent, role) rather than by a hand-written getter per prompt. It is what lets a
// new agent get live-tunable prompts WITHOUT anyone editing the kit.
//
// Each method returns "" when there is no override, and "" means "use my embedded
// default". That resolution lives in the AGENT, which owns its embedded defaults;
// the kit owns only the lookup mechanism. A nil TemplateProvider is legal
// everywhere the kit accepts one — that is the zero-config path (embedded
// defaults, no remote config), which is how local runs and evals work.
//
// role is the agent-defined role string: "worker"/"reviewer" for a Pair, "main"
// for a single-shot agent, or whatever your agent calls its roles.
type TemplateProvider interface {
	Template(agent, role string) string // prompt template override; "" = embedded default
	Model(agent, role string) string    // model override; "" = the agent's default
	Region(agent, role string) string   // provider region/location override; "" = default
}

// PromptOverride is one resolved (agent, role) entry. A zero field means "no
// override for this dimension" — you can override the model without touching the
// template, which is the common case when moving an agent to a cheaper model.
type PromptOverride struct {
	Template string
	Model    string
	Region   string
}

// MapTemplateProvider is a TemplateProvider backed by an in-memory map, safe for
// concurrent use. It is the kit's DEFAULT implementation and covers most needs:
// load your overrides from a file, a config service, or environment variables at
// startup, and Set them.
//
// ⭐ This replaced a hardcoded switch over a fixed list of (agent, role) keys —
// the kind of design that forces a framework edit to add an agent. Registration is
// data, so adding an agent is a call, not a patch.
//
// The zero value is ready to use:
//
//	var p agentkit.MapTemplateProvider
//	p.Set("review", "worker", agentkit.PromptOverride{Model: "fast-model"})
//
// ⚠️ IT HOLDS A SNAPSHOT, AND THAT IS THE TRAP. If your prompts live in a config
// service that can be updated without a redeploy, loading them into a map at
// startup silently FREEZES them: an operator ships a prompt change, nothing errors,
// and the agents keep running the old text indefinitely. Nothing fails, so nothing
// alerts — the worst shape a bug can take.
//
// Use this when your overrides are static for the process lifetime (a config file,
// environment variables, a test). When they can change underneath you, either call
// Set from whatever watches for changes, or use TemplateProviderFunc to read
// through to the live source on every lookup.
type MapTemplateProvider struct {
	mu sync.RWMutex
	m  map[promptKey]PromptOverride
}

type promptKey struct{ agent, role string }

// NewMapTemplateProvider builds a provider from a nested map, which is the shape
// config files decode into:
//
//	agentkit.NewMapTemplateProvider(map[string]map[string]agentkit.PromptOverride{
//	    "review": {"worker": {Model: "fast-model"}},
//	})
func NewMapTemplateProvider(overrides map[string]map[string]PromptOverride) *MapTemplateProvider {
	p := &MapTemplateProvider{m: make(map[promptKey]PromptOverride)}
	for agent, roles := range overrides {
		for role, ov := range roles {
			p.m[promptKey{agent, role}] = ov
		}
	}
	return p
}

// Set registers (or replaces) the override for one (agent, role). Safe to call
// concurrently with lookups, so a config watcher can hot-swap a prompt.
func (p *MapTemplateProvider) Set(agent, role string, ov PromptOverride) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[promptKey]PromptOverride)
	}
	p.m[promptKey{agent, role}] = ov
}

func (p *MapTemplateProvider) lookup(agent, role string) PromptOverride {
	if p == nil {
		return PromptOverride{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.m[promptKey{agent, role}]
}

// Template implements TemplateProvider. A nil *MapTemplateProvider returns "" —
// the embedded-default path — so a consumer need not branch on nil.
func (p *MapTemplateProvider) Template(agent, role string) string {
	return p.lookup(agent, role).Template
}

// Model implements TemplateProvider.
func (p *MapTemplateProvider) Model(agent, role string) string {
	return p.lookup(agent, role).Model
}

// Region implements TemplateProvider.
func (p *MapTemplateProvider) Region(agent, role string) string {
	return p.lookup(agent, role).Region
}

// TemplateProviderFunc adapts three functions into a TemplateProvider, for
// wrapping a config client you already have without declaring a type.
//
// ⭐ THIS IS THE ONE TO REACH FOR WHEN PROMPTS CAN CHANGE AT RUNTIME. Each lookup
// calls through to your source, so a prompt updated in your config service takes
// effect on the next call — no restart, and no silently-stale snapshot:
//
//	agentkit.TemplateProviderFunc{
//	    TemplateFn: func(agent, role string) string { return cfg.Prompt(agent + "/" + role) },
//	}
//
// A nil field returns "" for that dimension, so override only what you need.
type TemplateProviderFunc struct {
	TemplateFn func(agent, role string) string
	ModelFn    func(agent, role string) string
	RegionFn   func(agent, role string) string
}

// Template implements TemplateProvider.
func (f TemplateProviderFunc) Template(agent, role string) string {
	if f.TemplateFn == nil {
		return ""
	}
	return f.TemplateFn(agent, role)
}

// Model implements TemplateProvider.
func (f TemplateProviderFunc) Model(agent, role string) string {
	if f.ModelFn == nil {
		return ""
	}
	return f.ModelFn(agent, role)
}

// Region implements TemplateProvider.
func (f TemplateProviderFunc) Region(agent, role string) string {
	if f.RegionFn == nil {
		return ""
	}
	return f.RegionFn(agent, role)
}

// Compile-time proof that both shipped implementations satisfy the interface.
var (
	_ TemplateProvider = (*MapTemplateProvider)(nil)
	_ TemplateProvider = TemplateProviderFunc{}
)
