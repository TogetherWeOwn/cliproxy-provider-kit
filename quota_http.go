package providerkit

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const maxQuotaBody = 1 << 20

// QuotaHTTPTimeout bounds the entire quota HTTP exchange, including body reads.
const QuotaHTTPTimeout = 15 * time.Second

// QuotaHTTPClient is a bounded, secret-redacting read-only polling helper.
// It performs no logging. Errors omit URLs, headers, bodies and transport text.
// This is deliberately separate from HostHTTPClient: ABI 1 / schema 9 has no
// no-redirect setting. Executors must always use the host client instead.
// The transport may implement a provider's deployment proxy policy. Its RoundTrip
// method must NOT follow redirects itself (the standard transports do not).
type QuotaHTTPClient struct{ client *http.Client }

// NewQuotaHTTPClient sets a 15s timeout, no redirects and a 1MiB response cap.
// With nil transport it uses a clone of the standard transport (including normal
// environment proxy behavior), not a mutable shared http.Client.
func NewQuotaHTTPClient(transport http.RoundTripper) *QuotaHTTPClient {
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &QuotaHTTPClient{client: &http.Client{Transport: transport, Timeout: QuotaHTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Do reads a quota/discovery resource. Only GET is allowed, to keep this helper
// out of the inference execution path. Successful bodies are returned to the
// provider parser; callers must never log them or credential-bearing headers.
func (c *QuotaHTTPClient) Do(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	if r.Method != "" && r.Method != http.MethodGet {
		return pluginapi.HTTPResponse{}, rpcError("invalid_request", "quota HTTP helper only supports GET", 400)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return pluginapi.HTTPResponse{}, rpcError("quota_http_error", "invalid quota request", 400)
	}
	req.Header = r.Headers.Clone()
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return pluginapi.HTTPResponse{}, ctx.Err()
		}
		return pluginapi.HTTPResponse{}, rpcError("quota_http_error", "quota HTTP request failed", 502)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := rpcError("quota_http_error", "quota endpoint returned an unsuccessful status", resp.StatusCode)
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			e.HTTPStatus = 502
			e.Code = "quota_redirect"
			e.Message = "quota endpoint redirects are not allowed"
		}
		e.Retryable = resp.StatusCode == 429 || resp.StatusCode >= 500
		e.RetryAfterSeconds = RetryAfterSeconds(resp.Header.Get("Retry-After"), time.Now())
		return pluginapi.HTTPResponse{}, e
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return pluginapi.HTTPResponse{}, ctx.Err()
		}
		return pluginapi.HTTPResponse{}, rpcError("quota_http_error", "quota response read failed", 502)
	}
	if len(body) > maxQuotaBody {
		return pluginapi.HTTPResponse{}, rpcError("quota_response_too_large", "quota response exceeds 1 MiB", 502)
	}
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
}

// CloseIdleConnections releases pooled polling connections during plugin shutdown.
func (c *QuotaHTTPClient) CloseIdleConnections() { c.client.CloseIdleConnections() }
