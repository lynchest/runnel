package circuit

import (
	"strings"
	"sync"
)

// Registry keeps one circuit breaker for each domain. Get and SetConfig are
// safe to call concurrently.
type Registry struct {
	mu sync.Mutex

	defaults Config
	clock    Clock
	configs  map[string]Config
	breakers map[string]*Breaker
}

// NewRegistry creates a per-domain registry. A nil clock selects the wall
// clock and is shared by every breaker created by the registry.
func NewRegistry(defaults Config, clock Clock) *Registry {
	if clock == nil {
		clock = wallClock{}
	}
	return &Registry{
		defaults: normalizeConfig(defaults),
		clock:    clock,
		configs:  make(map[string]Config),
		breakers: make(map[string]*Breaker),
	}
}

// SetConfig sets the policy used when domain is first requested. It should be
// called before Get; existing breakers retain their current state and policy.
func (r *Registry) SetConfig(domain string, config Config) {
	r.mu.Lock()
	r.configs[canonicalDomain(domain)] = config
	r.mu.Unlock()
}

// Get returns the same breaker for every request for a canonical domain,
// creating it atomically on first use.
func (r *Registry) Get(domain string) *Breaker {
	key := canonicalDomain(domain)
	r.mu.Lock()
	defer r.mu.Unlock()

	if breaker := r.breakers[key]; breaker != nil {
		return breaker
	}
	config := r.defaults
	if override, ok := r.configs[key]; ok {
		config = mergeConfig(config, override)
	}
	breaker := NewBreaker(config, r.clock)
	r.breakers[key] = breaker
	return breaker
}

// Lookup returns an already-created breaker without creating one.
func (r *Registry) Lookup(domain string) (*Breaker, bool) {
	r.mu.Lock()
	breaker, ok := r.breakers[canonicalDomain(domain)]
	r.mu.Unlock()
	return breaker, ok
}

// Len returns the number of instantiated domain breakers.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.breakers)
}

// Snapshots returns an atomically captured snapshot for every instantiated
// domain. The returned map is independent of registry state.
func (r *Registry) Snapshots() map[string]Snapshot {
	if r == nil {
		return map[string]Snapshot{}
	}
	r.mu.Lock()
	result := make(map[string]Snapshot, len(r.breakers))
	for domain, breaker := range r.breakers {
		if breaker != nil {
			result[domain] = breaker.Snapshot()
		}
	}
	r.mu.Unlock()
	return result
}

// Reset resets an instantiated domain breaker. It returns false when the
// domain has not yet been used.
func (r *Registry) Reset(domain string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	breaker, ok := r.breakers[canonicalDomain(domain)]
	r.mu.Unlock()
	if !ok || breaker == nil {
		return false
	}
	breaker.Reset()
	return true
}

// ResetAll resets every instantiated breaker without removing registry
// entries, so future requests retain their configured policies.
func (r *Registry) ResetAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	breakers := make([]*Breaker, 0, len(r.breakers))
	for _, breaker := range r.breakers {
		if breaker != nil {
			breakers = append(breakers, breaker)
		}
	}
	r.mu.Unlock()
	for _, breaker := range breakers {
		breaker.Reset()
	}
}

func canonicalDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

func mergeConfig(base, override Config) Config {
	if override.InitialCooldown > 0 {
		base.InitialCooldown = override.InitialCooldown
	}
	if override.CooldownMultiplier > 0 {
		base.CooldownMultiplier = override.CooldownMultiplier
	}
	if override.MaxCooldown > 0 {
		base.MaxCooldown = override.MaxCooldown
	}
	return normalizeConfig(base)
}
