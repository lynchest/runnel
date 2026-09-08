package circuit

import (
	"sync"
	"testing"
	"time"
)

func TestRegistryReturnsOneBreakerPerDomain(t *testing.T) {
	clock := newTestClock(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))
	registry := NewRegistry(Config{
		InitialCooldown:    3 * time.Second,
		CooldownMultiplier: 2,
		MaxCooldown:        30 * time.Second,
	}, clock)

	const callers = 50
	results := make(chan *Breaker, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			results <- registry.Get("API.Example.com.")
		}()
	}
	wg.Wait()
	close(results)

	var first *Breaker
	for breaker := range results {
		if first == nil {
			first = breaker
			continue
		}
		if breaker != first {
			t.Fatal("registry returned different breakers for one domain")
		}
	}
	if registry.Len() != 1 {
		t.Fatalf("registry length = %d, want 1", registry.Len())
	}

	other := registry.Get("other.example.com")
	if other == first {
		t.Fatal("different domains share a breaker")
	}
	first.OnFailure()
	if !other.CanExecute() {
		t.Fatal("an open breaker for one domain blocked another domain")
	}
}

func TestRegistryAppliesPerDomainConfig(t *testing.T) {
	clock := newTestClock(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))
	registry := NewRegistry(Config{
		InitialCooldown:    time.Second,
		CooldownMultiplier: 2,
		MaxCooldown:        time.Second,
	}, clock)
	registry.SetConfig("api.example.com", Config{
		InitialCooldown: 9 * time.Second,
		MaxCooldown:     9 * time.Second,
	})

	configured := registry.Get("API.Example.com.")
	configured.OnFailure()
	if got := configured.Snapshot().RemainingCooldown; got != 9*time.Second {
		t.Fatalf("per-domain policy cooldown = %s, want 9s", got)
	}

	defaulted := registry.Get("other.example.com")
	defaulted.OnFailure()
	if got := defaulted.Snapshot().RemainingCooldown; got != time.Second {
		t.Fatalf("default policy cooldown = %s, want 1s", got)
	}
}
