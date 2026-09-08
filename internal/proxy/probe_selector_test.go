package proxy

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/lynchest/runnel/internal/queue"
)

func TestProbeSelectorChoosesLightestGET(t *testing.T) {
	selector := NewProbeSelector(ProbeHeadersOnly)
	items := []queue.Item{
		{Method: http.MethodPost, Weight: 1},
		{Method: http.MethodGet, Weight: 20},
		{Method: http.MethodGet, Weight: 4},
		{Method: http.MethodGet, Weight: 4},
	}
	selected, ok := selector.Select(items)
	if !ok {
		t.Fatal("selector did not find a GET")
	}
	if selected.Weight != 4 || selected.ID != items[2].ID {
		t.Fatalf("selected item = %+v, want first lightest GET", selected)
	}

	if _, ok := selector.Select([]queue.Item{{Method: http.MethodPost, Weight: 0}}); ok {
		t.Fatal("selector selected a non-GET request")
	}
}

func TestProbeSelectorHeadersOnlyBypassesCache(t *testing.T) {
	request := httptestRequest(t, http.MethodGet, "https://api.example.test/items?x=1")
	selector := NewProbeSelector(ProbeHeadersOnly)
	before := request.URL.String()
	if err := selector.Apply(request); err != nil {
		t.Fatal(err)
	}
	if request.URL.String() != before {
		t.Fatalf("headers_only changed URL from %q to %q", before, request.URL)
	}
	if got := request.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if got := request.Header.Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
}

func TestProbeSelectorQueryParamPreservesQueryAndAddsTimestamp(t *testing.T) {
	request := httptestRequest(t, http.MethodGet, "https://api.example.test/items?x=1")
	selector := NewProbeSelector(ProbeQueryParam)
	if err := selector.Apply(request); err != nil {
		t.Fatal(err)
	}
	values := request.URL.Query()
	if values.Get("x") != "1" {
		t.Fatalf("existing query was changed: %q", request.URL.RawQuery)
	}
	probe := values.Get("_probe")
	if probe == "" {
		t.Fatal("query_param did not add _probe")
	}
	if _, err := strconv.ParseInt(probe, 10, 64); err != nil {
		t.Fatalf("_probe = %q is not a timestamp: %v", probe, err)
	}
	if got := request.Header.Get("Cache-Control"); got != "" {
		t.Fatalf("query_param unexpectedly set Cache-Control = %q", got)
	}
}

func TestProbeSelectorTakeAppliesStrategyToQueueTicket(t *testing.T) {
	q := queue.New(2, time.Second)
	request := httptestRequest(t, http.MethodGet, "https://api.example.test/items")
	ctx := context.Background()
	if _, err := q.Add(ctx, queue.NewItem(request, 1)); err != nil {
		t.Fatal(err)
	}
	selector := NewProbeSelector(ProbeQueryParam)
	ticket, err := selector.Take(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Item.Request.URL.Query().Get("_probe") == "" {
		t.Fatal("Take did not apply query-buster strategy")
	}
}

func httptestRequest(t *testing.T, method, rawURL string) *http.Request {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{Method: method, URL: u, Header: make(http.Header)}
}
