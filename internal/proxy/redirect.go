package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const gatewayProxyPath = "/proxy"

// StopRedirects is an http.Client CheckRedirect policy that returns the
// upstream 3xx response unchanged. The gateway can then resolve and rewrite
// Location without allowing net/http to follow an unvalidated hop.
func StopRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// RedirectPolicy returns a reusable CheckRedirect function for clients that
// should trap redirects at the gateway boundary.
func RedirectPolicy() func(*http.Request, []*http.Request) error {
	return StopRedirects
}

// ResolveRedirect resolves location against req.URL using net/url's RFC
// reference rules. The result must be an absolute HTTP(S) URL with a host;
// rejecting other schemes prevents a trapped Location from becoming a
// client-side escape hatch.
func ResolveRedirect(req *http.Request, location string) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("redirect request URL is nil")
	}
	location = strings.TrimSpace(location)
	if location == "" {
		return nil, errors.New("redirect location is empty")
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("invalid redirect location: %w", err)
	}
	if strings.HasPrefix(location, "//") && parsed.Host == "" {
		return nil, errors.New("redirect location has an empty authority")
	}
	resolved := req.URL.ResolveReference(parsed)
	if resolved == nil || !isHTTPOrHTTPS(resolved.Scheme) || resolved.Host == "" || resolved.Hostname() == "" || resolved.User != nil {
		return nil, errors.New("redirect location is not an absolute HTTP(S) URL")
	}
	return resolved, nil
}

// RewriteRedirect turns an upstream Location into a gateway URL. Relative
// references are made absolute against req.URL before percent-encoding the
// complete upstream URL as the proxy query value.
func RewriteRedirect(req *http.Request, location string) (string, error) {
	resolved, err := ResolveRedirect(req, location)
	if err != nil {
		return "", err
	}
	query := url.Values{}
	query.Set("url", resolved.String())
	return gatewayProxyPath + "?" + query.Encode(), nil
}

// RewriteResponseLocation rewrites an upstream response's Location header in
// place and returns an error for malformed or unsupported targets.
func RewriteResponseLocation(resp *http.Response) error {
	if resp == nil || resp.Header == nil {
		return nil
	}
	location := resp.Header.Get("Location")
	if location == "" || resp.Request == nil {
		return nil
	}
	rewritten, err := RewriteRedirect(resp.Request, location)
	if err != nil {
		return err
	}
	resp.Header.Set("Location", rewritten)
	return nil
}
