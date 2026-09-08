package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lynchest/runnel/internal/queue"
)

// ProbeStrategy controls how a selected request bypasses an upstream cache.
type ProbeStrategy string

const (
	ProbeHeadersOnly ProbeStrategy = "headers_only"
	ProbeQueryParam  ProbeStrategy = "query_param"
)

var ErrInvalidProbeStrategy = errors.New("invalid probe strategy")

// ProbeSelector chooses the least expensive queued GET and applies the
// configured cache-bypass strategy to it.
type ProbeSelector struct {
	Strategy ProbeStrategy
}

// NewProbeSelector creates a selector. An empty strategy uses headers_only,
// matching the documented default.
func NewProbeSelector(strategy ProbeStrategy) *ProbeSelector {
	strategy = ProbeStrategy(strings.TrimSpace(string(strategy)))
	if strategy == "" {
		strategy = ProbeHeadersOnly
	}
	return &ProbeSelector{Strategy: strategy}
}

// Select returns the lowest-weight GET in items. Ties preserve queue order;
// non-GET items are never selected.
func (s *ProbeSelector) Select(items []queue.Item) (queue.Item, bool) {
	var selected queue.Item
	found := false
	for _, item := range items {
		if !isProbeGET(item) {
			continue
		}
		if !found || item.Weight < selected.Weight {
			selected = item
			found = true
		}
	}
	return selected, found
}

// Take removes the lowest-weight GET from q, admits it, and applies this
// selector's strategy to the request carried by the ticket.
func (s *ProbeSelector) Take(ctx context.Context, q *queue.Queue) (*queue.Ticket, error) {
	if q == nil {
		return nil, errors.New("probe queue is nil")
	}
	ticket, err := q.TakeLightestGETWith(ctx, func(item *queue.Item) error {
		if item.Request == nil {
			return nil
		}
		return s.Apply(item.Request)
	})
	if err != nil {
		return nil, err
	}
	return ticket, nil
}

// Apply adds a cache-bypass marker to req. headers_only preserves the URL and
// asks intermediaries to revalidate; query_param preserves existing query
// values and adds a timestamp-valued _probe parameter.
func (s *ProbeSelector) Apply(req *http.Request) error {
	if req == nil {
		return errors.New("probe request is nil")
	}
	strategy := ProbeHeadersOnly
	if s != nil && s.Strategy != "" {
		strategy = s.Strategy
	}
	switch strategy {
	case ProbeHeadersOnly:
		if req.Header == nil {
			req.Header = make(http.Header)
		}
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Pragma", "no-cache")
		return nil
	case ProbeQueryParam:
		if req.URL == nil {
			return errors.New("probe request URL is nil")
		}
		clone := *req.URL
		values := clone.Query()
		values.Set("_probe", strconv.FormatInt(time.Now().UnixNano(), 10))
		clone.RawQuery = values.Encode()
		req.URL = &clone
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrInvalidProbeStrategy, strategy)
	}
}

func isProbeGET(item queue.Item) bool {
	method := item.Method
	if method == "" && item.Request != nil {
		method = item.Request.Method
	}
	return strings.EqualFold(method, http.MethodGet)
}
