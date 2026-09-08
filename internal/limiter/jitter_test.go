package limiter_test

import (
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/limiter"
)

func TestJitterBounds(t *testing.T) {
	min := 50 * time.Millisecond
	max := 150 * time.Millisecond

	j := limiter.NewJitter(min, max)
	gotMin, gotMax := j.Bounds()
	if gotMin != min || gotMax != max {
		t.Fatalf("expected bounds [%v, %v], got [%v, %v]", min, max, gotMin, gotMax)
	}

	seenDifferent := false
	var first time.Duration

	for i := 0; i < 200; i++ {
		val := j.Jitter()
		if val < min || val > max {
			t.Fatalf("jitter %v out of bounds [%v, %v]", val, min, max)
		}
		if i == 0 {
			first = val
		} else if val != first {
			seenDifferent = true
		}
	}

	if !seenDifferent {
		t.Fatalf("expected jitter to produce varied outputs, got constant %v", first)
	}
}

func TestJitterEdgeCases(t *testing.T) {
	// min > max
	j1 := limiter.NewJitter(200*time.Millisecond, 100*time.Millisecond)
	bMin, bMax := j1.Bounds()
	if bMin != 200*time.Millisecond || bMax != 200*time.Millisecond {
		t.Fatalf("expected min=max=200ms when max < min, got [%v, %v]", bMin, bMax)
	}
	if val := j1.Jitter(); val != 200*time.Millisecond {
		t.Fatalf("expected exact 200ms, got %v", val)
	}

	// negative min
	j2 := limiter.NewJitter(-50*time.Millisecond, 100*time.Millisecond)
	bMin, bMax = j2.Bounds()
	if bMin != 0 || bMax != 100*time.Millisecond {
		t.Fatalf("expected min=0, max=100ms when min negative, got [%v, %v]", bMin, bMax)
	}
}

func TestJitterDeterministicSeam(t *testing.T) {
	min := 10 * time.Millisecond
	max := 50 * time.Millisecond

	j1 := limiter.NewJitterWithSource(min, max, rand.New(rand.NewSource(42)))
	j2 := limiter.NewJitterWithSource(min, max, rand.New(rand.NewSource(42)))

	for i := 0; i < 20; i++ {
		v1 := j1.Jitter()
		v2 := j2.Jitter()
		if v1 != v2 {
			t.Fatalf("iteration %d: expected identical deterministic outputs, got %v and %v", i, v1, v2)
		}
	}
}

func TestJitterConcurrencySafe(t *testing.T) {
	j := limiter.NewJitter(1*time.Millisecond, 20*time.Millisecond)
	var wg sync.WaitGroup

	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				val := j.Jitter()
				if val < 1*time.Millisecond || val > 20*time.Millisecond {
					t.Errorf("jitter out of bounds: %v", val)
				}
			}
		}()
	}

	wg.Wait()
}
