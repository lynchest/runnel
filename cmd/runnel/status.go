package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultGatewayURL = "http://127.0.0.1:8090"

type statusCircuits struct {
	Circuits map[string]struct {
		State string `json:"state"`
	} `json:"circuits"`
}

func runStatus(stdout, stderr io.Writer, client *http.Client) int {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	for _, gateway := range statusGatewayCandidates() {
		health, err := statusGET(client, gateway+"/_healthz")
		if err != nil {
			continue
		}
		if health.StatusCode != http.StatusOK {
			_ = health.Body.Close()
			continue
		}
		_ = health.Body.Close()

		circuitsResponse, err := statusGET(client, gateway+"/_circuit")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: read circuit status: %v\n", err)
			return 1
		}
		var circuits statusCircuits
		decodeErr := json.NewDecoder(circuitsResponse.Body).Decode(&circuits)
		_ = circuitsResponse.Body.Close()
		if decodeErr != nil || circuitsResponse.StatusCode != http.StatusOK {
			_, _ = fmt.Fprintln(stderr, "error: gateway returned invalid circuit status")
			return 1
		}

		states := map[string]int{"CLOSED": 0, "OPEN": 0, "HALF_OPEN": 0}
		for _, item := range circuits.Circuits {
			states[strings.ToUpper(item.State)]++
		}
		_, _ = fmt.Fprintf(stdout, "Gateway: %s\nHealth: ok\nCircuits: %d total, %d closed, %d open, %d half-open\n",
			gateway, len(circuits.Circuits), states["CLOSED"], states["OPEN"], states["HALF_OPEN"])
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "error: cannot reach runnel gateway; check RUNNEL_URL")
	return 2
}

func statusGatewayCandidates() []string {
	candidates := []string{defaultGatewayURL}
	if configured := strings.TrimRight(strings.TrimSpace(os.Getenv("RUNNEL_URL")), "/"); configured != "" && configured != defaultGatewayURL {
		candidates = append([]string{configured}, candidates...)
	}
	return candidates
}

func statusGET(client *http.Client, endpoint string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "runnel-status")
	return client.Do(request)
}
