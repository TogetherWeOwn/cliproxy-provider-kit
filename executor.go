package providerkit

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Passthrough is a thin executor. BuildRequest selects the provider protocol,
// endpoint and authentication; it must preserve the host's final business payload
// (the host already applied payload rules). No protocol translation is done here.
// HttpRequest copies the supplied HTTP request unless PrepareHTTP is provided.
// Every network operation uses the injected HostHTTPClient; there is no fallback.
// Provider-specific quota/exhaustion classification belongs in ClassifyError.
// Hooks are immutable after construction and must support concurrent calls.
type Passthrough struct {
	Provider      string
	BuildRequest  func(context.Context, pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error)
	PrepareHTTP   func(context.Context, pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error)
	ClassifyError func(pluginapi.HTTPResponse) error
	TokenCount    func(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error)
}

func (p Passthrough) Identifier() string { return p.Provider }
func (p Passthrough) classify(resp pluginapi.HTTPResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if p.ClassifyError != nil {
		if err := p.ClassifyError(resp); err != nil {
			return err
		}
	}
	e := rpcError("upstream_error", "upstream request failed", resp.StatusCode)
	e.Retryable = resp.StatusCode == 429 || resp.StatusCode >= 500
	e.RetryAfterSeconds = RetryAfterSeconds(resp.Headers.Get("Retry-After"), time.Now())
	return e
}
func (p Passthrough) request(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
	if r.HTTPClient == nil {
		return pluginapi.HTTPRequest{}, rpcError("host_unavailable", "host HTTP client is required", 503)
	}
	if p.BuildRequest == nil {
		return pluginapi.HTTPRequest{}, rpcError("invalid_executor", "BuildRequest is required", 500)
	}
	return p.BuildRequest(ctx, r)
}
func (p Passthrough) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	req, err := p.request(ctx, r)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	resp, err := r.HTTPClient.Do(ctx, req)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if err = p.classify(resp); err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	return pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers}, nil
}
func (p Passthrough) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	req, err := p.request(ctx, r)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	resp, err := r.HTTPClient.DoStream(streamCtx, req)
	if err != nil {
		cancel()
		return pluginapi.ExecutorStreamResponse{}, err
	}
	if resp.Chunks == nil {
		cancel()
		return pluginapi.ExecutorStreamResponse{}, rpcError("invalid_stream", "host returned no HTTP stream", 502)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer cancel()
		body := make([]byte, 0)
		for {
			select {
			case <-streamCtx.Done():
				return pluginapi.ExecutorStreamResponse{}, streamCtx.Err()
			case chunk, ok := <-resp.Chunks:
				if !ok {
					return pluginapi.ExecutorStreamResponse{}, p.classify(pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: body})
				}
				if len(chunk.Payload) > maxQuotaBody-len(body) {
					return pluginapi.ExecutorStreamResponse{}, p.classify(pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: body})
				}
				body = append(body, chunk.Payload...)
				if chunk.Err != nil {
					return pluginapi.ExecutorStreamResponse{}, p.classify(pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: body})
				}
			}
		}
	}
	out := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-streamCtx.Done():
				return
			case chunk, ok := <-resp.Chunks:
				if !ok {
					return
				}
				select {
				case out <- pluginapi.ExecutorStreamChunk{Payload: chunk.Payload, Err: chunk.Err}:
				case <-streamCtx.Done():
					return
				}
				if chunk.Err != nil {
					return
				}
			}
		}
	}()
	return pluginapi.ExecutorStreamResponse{Headers: resp.Headers, Chunks: out}, nil
}

// CountTokens uses TokenCount when supplied. Otherwise it returns an explicitly
// estimated one-token-per-four-Unicode-codepoints upper-rounded payload count,
// not a model tokenizer or a billable upstream token-count request.
func (p Passthrough) CountTokens(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	if p.TokenCount != nil {
		return p.TokenCount(ctx, r)
	}
	n := (utf8.RuneCount(r.Payload) + 3) / 4
	raw, err := json.Marshal(struct {
		Total int `json:"total_tokens"`
	}{n})
	return pluginapi.ExecutorResponse{Payload: raw, Metadata: map[string]any{"estimated": true}}, err
}
func (p Passthrough) HttpRequest(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	if r.HTTPClient == nil {
		return pluginapi.ExecutorHTTPResponse{}, rpcError("host_unavailable", "host HTTP client is required", 503)
	}
	req := pluginapi.HTTPRequest{Method: r.Method, URL: r.URL, Headers: r.Headers.Clone(), Body: r.Body}
	if p.PrepareHTTP != nil {
		var err error
		req, err = p.PrepareHTTP(ctx, r)
		if err != nil {
			return pluginapi.ExecutorHTTPResponse{}, err
		}
	}
	resp, err := r.HTTPClient.Do(ctx, req)
	if err != nil {
		return pluginapi.ExecutorHTTPResponse{}, err
	}
	if err = p.classify(resp); err != nil {
		return pluginapi.ExecutorHTTPResponse{}, err
	}
	return pluginapi.ExecutorHTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

var _ pluginapi.ProviderExecutor = Passthrough{}
