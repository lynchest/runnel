package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const localhostGateway = "http://127.0.0.1:8090"

type headerFlags http.Header

func (h *headerFlags) String() string { return "" }

func (h *headerFlags) Set(value string) error {
	name, fieldValue, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("header must be in 'Name: Value' form")
	}
	http.Header(*h).Add(strings.TrimSpace(name), strings.TrimSpace(fieldValue))
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithClient(args, stdout, stderr, &http.Client{Timeout: 35 * time.Second})
}

func runWithClient(args []string, stdout, stderr io.Writer, client *http.Client) int {
	flags := flag.NewFlagSet("runnel-get", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "raw", "output format: raw or markdown")
	noCache := flags.Bool("no-cache", false, "bypass the gateway cache")
	fresh := flags.Bool("fresh", false, "alias for --no-cache")
	shortFresh := flags.Bool("f", false, "alias for --no-cache")
	headers := headerFlags(http.Header{})
	flags.Var(&headers, "H", "request header in 'Name: Value' form (repeatable)")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(flags.Output(), "Usage: runnel-get [--output raw|markdown] [-H 'Header: Value'] <URL>")
		_, _ = fmt.Fprintln(flags.Output(), "Example: runnel-get --output markdown 'https://example.com'")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 1
	}
	if *output != "raw" && *output != "markdown" {
		_, _ = fmt.Fprintln(stderr, "error: --output must be raw or markdown")
		return 1
	}
	if *noCache || *fresh || *shortFresh {
		http.Header(headers).Set("Cache-Control", "no-cache")
	}

	gateway := findGateway(client)
	if gateway == "" {
		_, _ = fmt.Fprintln(stderr, "error: cannot reach runnel gateway; check RUNNEL_URL")
		return 2
	}

	proxyURL := gateway + "/proxy?url=" + url.QueryEscape(flags.Arg(0))
	req, err := http.NewRequest(http.MethodGet, proxyURL, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "request error: %v\n", err)
		return 1
	}
	req.Header = http.Header(headers).Clone()
	resp, err := client.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "connection error: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "response error: %v\n", err)
		return 1
	}
	if resp.StatusCode >= 400 {
		_, _ = fmt.Fprintf(stderr, "HTTP error: %s\n", resp.Status)
		_, _ = stderr.Write(body)
		return resp.StatusCode
	}
	if *output == "markdown" && strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		body, err = htmlToMarkdown(body, resp.Header.Get("Content-Type"))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "HTML conversion error: %v\n", err)
			return 1
		}
	}
	if _, err := stdout.Write(body); err != nil {
		_, _ = fmt.Fprintf(stderr, "output error: %v\n", err)
		return 1
	}
	return 0
}

func findGateway(client *http.Client) string {
	candidates := []string{localhostGateway}
	if configured := strings.TrimRight(os.Getenv("RUNNEL_URL"), "/"); configured != "" {
		candidates = append([]string{configured}, candidates...)
	}
	for _, gateway := range candidates {
		probeClient := *client
		probeClient.Timeout = 1500 * time.Millisecond
		req, _ := http.NewRequest(http.MethodGet, gateway+"/_healthz", nil)
		req.Header.Set("User-Agent", "runnel-probe")
		resp, err := probeClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return gateway
			}
		}
	}
	return ""
}
