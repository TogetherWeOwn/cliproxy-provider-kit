package providerkit

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type proxyContextKey struct{}

// AuthProxyURL resolves the outbound proxy for direct (non-host) requests made
// on behalf of one auth, with the host's precedence: the auth's proxy_url
// (metadata overrides storage), then the host's global proxy-url. An empty
// result means "inherit": the environment proxy settings apply.
func AuthProxyURL(storage []byte, metadata map[string]any, hostProxyURL string) string {
	value, present := metadata["proxy_url"]
	if !present {
		if fields, err := object(storage); err == nil {
			value = fields["proxy_url"]
		}
	}
	// A present metadata value, even an empty one, hides the storage value.
	if text, _ := value.(string); strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(hostProxyURL)
}

// WithProxyURL attaches a proxy setting to ctx for QuotaHTTPClient requests.
// It accepts the host's proxy syntax: "" inherits the environment, "direct" or
// "none" bypasses proxies, otherwise an http, https, socks5 or socks5h URL.
func WithProxyURL(ctx context.Context, proxyURL string) context.Context {
	return context.WithValue(ctx, proxyContextKey{}, strings.TrimSpace(proxyURL))
}

var errInvalidProxy = errors.New("invalid proxy setting")

// parseProxy returns (nil, true, nil) for an explicit direct connection and
// (nil, false, nil) to inherit the environment. Errors never include the URL,
// which may carry proxy credentials.
func parseProxy(raw string) (*url.URL, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false, nil
	}
	if strings.EqualFold(raw, "direct") || strings.EqualFold(raw, "none") {
		return nil, true, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, false, errInvalidProxy
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return u, false, nil
	}
	return nil, false, errInvalidProxy
}

// QuotaProxy is an http.Transport Proxy function that applies the setting from
// WithProxyURL and otherwise falls back to the environment. Custom transports
// should start from NewQuotaTransport rather than set it alone: a transport
// shared across proxies must not reuse HTTP/2 connections.
func QuotaProxy(r *http.Request) (*url.URL, error) {
	raw, ok := r.Context().Value(proxyContextKey{}).(string)
	if !ok {
		return http.ProxyFromEnvironment(r)
	}
	u, direct, err := parseProxy(raw)
	if err != nil || direct || u != nil {
		return u, err
	}
	return http.ProxyFromEnvironment(r)
}
