package circuit

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseRetryAfterExactSupportedFormats(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{name: "delta seconds", header: "60", want: 60 * time.Second},
		{name: "delta seconds with OWS", header: "  7  ", want: 7 * time.Second},
		{name: "RFC1123", header: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second},
		{name: "RFC850", header: now.Add(91 * time.Second).Format(time.RFC850), want: 91 * time.Second},
		{name: "asctime", header: now.Add(92 * time.Second).Format(time.ANSIC), want: 92 * time.Second},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseRetryAfter(test.header, now)
			if err != nil {
				t.Fatalf("ParseRetryAfter(%q) returned error: %v", test.header, err)
			}
			if got != test.want {
				t.Fatalf("ParseRetryAfter(%q) = %s, want %s", test.header, got, test.want)
			}
		})
	}
}

func TestParseRetryAfterExplicitEpochPolicy(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	future := now.Add(75 * time.Second)

	for _, header := range []string{
		"unix:" + strconv.FormatInt(future.Unix(), 10),
		"epoch:" + strconv.FormatInt(future.Unix(), 10),
	} {
		got, err := ParseRetryAfter(header, now)
		if err != nil {
			t.Fatalf("ParseRetryAfter(%q) returned error: %v", header, err)
		}
		if got != 75*time.Second {
			t.Fatalf("ParseRetryAfter(%q) = %s, want 1m15s", header, got)
		}
	}

	// An unprefixed long integer remains delta-seconds under the explicit
	// prefix-only epoch policy.
	if got, err := ParseRetryAfter("1700000000", now); err != nil || got != 1700000000*time.Second {
		t.Fatalf("unprefixed epoch-like value = (%s, %v), want delta-seconds", got, err)
	}
	if got, err := ParseRetryAfter("0", now); err != nil || got != 0 {
		t.Fatalf("zero Retry-After = (%s, %v), want (0, nil)", got, err)
	}
	if got, err := ParseRetryAfter(now.Add(-time.Second).Format(http.TimeFormat), now); err != nil || got != 0 {
		t.Fatalf("past Retry-After date = (%s, %v), want (0, nil)", got, err)
	}
}

func TestParseRetryAfterRejectsInvalidValues(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, header := range []string{"", "-1", "+1", "1.5", "not-a-date", "unix:nope"} {
		_, err := ParseRetryAfter(header, now)
		if err == nil {
			t.Errorf("ParseRetryAfter(%q) unexpectedly succeeded", header)
		} else if !errors.Is(err, ErrInvalidRetryAfter) {
			t.Errorf("ParseRetryAfter(%q) error = %v, want ErrInvalidRetryAfter", header, err)
		}
	}
}
