package limiter

import (
	"math/rand"
	"sync"
	"time"
)

// JitterSource defines an interface providing anti-bot randomized jitter duration.
type JitterSource interface {
	Jitter() time.Duration
}

// Jitter produces anti-bot randomized delay durations within configured [min, max] bounds.
// It is concurrency-safe and uses an isolated local math/rand.Rand instance to avoid
// mutating or contending on global math/rand state.
type Jitter struct {
	mu  sync.Mutex
	rng *rand.Rand
	min time.Duration
	max time.Duration
}

// NewJitter constructs a Jitter generator.
// If min < 0, it is normalized to 0.
// If max < min, max is clamped to min.
func NewJitter(min, max time.Duration) *Jitter {
	src := rand.NewSource(time.Now().UnixNano())
	return NewJitterWithSource(min, max, rand.New(src))
}

// NewJitterWithSource constructs a Jitter generator with an injected math/rand.Rand source
// for deterministic testing.
func NewJitterWithSource(min, max time.Duration, rng *rand.Rand) *Jitter {
	if min < 0 {
		min = 0
	}
	if max < min {
		max = min
	}
	return &Jitter{
		rng: rng,
		min: min,
		max: max,
	}
}

// Jitter returns a random duration between [min, max] (inclusive).
func (j *Jitter) Jitter() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.min >= j.max {
		return j.min
	}

	delta := int64(j.max - j.min)
	// Int63n(n) returns [0, n). Adding 1 to delta allows inclusive upper bound.
	rnd := j.rng.Int63n(delta + 1)
	return j.min + time.Duration(rnd)
}

// Bounds returns the configured minimum and maximum jitter durations.
func (j *Jitter) Bounds() (min, max time.Duration) {
	return j.min, j.max
}
