package circuit

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidRetryAfter indicates that a Retry-After value is neither a
// non-negative delta-seconds value nor a supported HTTP date.
var ErrInvalidRetryAfter = errors.New("invalid Retry-After value")

// ParseRetryAfter converts a Retry-After header into a duration relative to
// now. Numeric values are delta-seconds. For the optional Unix epoch form,
// callers must use an explicit "unix:" or "epoch:" prefix; this avoids the
// ambiguity between a very large delta-seconds value and epoch seconds.
// HTTP dates are delegated to net/http.ParseTime, which supports RFC1123,
// RFC850, and asctime/ANSIC.
func ParseRetryAfter(value string, now time.Time) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("%w: empty value", ErrInvalidRetryAfter)
	}

	if epoch, ok, err := parseExplicitEpoch(value, now); ok {
		return epoch, err
	}
	if isDecimal(value) {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrInvalidRetryAfter, err)
		}
		duration, ok := secondsDuration(seconds)
		if !ok {
			return 0, fmt.Errorf("%w: delta-seconds overflow", ErrInvalidRetryAfter)
		}
		return duration, nil
	}

	parsed, err := http.ParseTime(value)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidRetryAfter, err)
	}
	if !parsed.After(now) {
		return 0, nil
	}
	return parsed.Sub(now), nil
}

func parseExplicitEpoch(value string, now time.Time) (time.Duration, bool, error) {
	lower := strings.ToLower(value)
	for _, prefix := range []string{"unix:", "epoch:"} {
		if strings.HasPrefix(lower, prefix) {
			seconds := strings.TrimSpace(value[len(prefix):])
			if !isDecimal(seconds) {
				return 0, true, fmt.Errorf("%w: invalid epoch value", ErrInvalidRetryAfter)
			}
			duration, err := parseEpochSeconds(seconds, now)
			return duration, true, err
		}
	}
	return 0, false, nil
}

func parseEpochSeconds(value string, now time.Time) (time.Duration, error) {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: epoch overflow", ErrInvalidRetryAfter)
	}
	epoch := time.Unix(seconds, 0)
	if !epoch.After(now) {
		return 0, nil
	}
	return epoch.Sub(now), nil
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
