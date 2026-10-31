package providerkit

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type localHostHTTP struct {
	request pluginapi.HTTPRequest
	status  int
	body    []byte
	chunks  []pluginapi.HTTPStreamChunk
	headers http.Header
}

func (h *localHostHTTP) Do(ctx context.Context, q pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.request = q
	return pluginapi.HTTPResponse{StatusCode: h.status, Body: h.body, Headers: h.headers}, nil
}
func (h *localHostHTTP) DoStream(ctx context.Context, q pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	h.request = q
	out := make(chan pluginapi.HTTPStreamChunk, len(h.chunks))
	for _, c := range h.chunks {
		out <- c
	}
	close(out)
	return pluginapi.HTTPStreamResponse{StatusCode: h.status, Headers: h.headers, Chunks: out}, nil
}
func echoExecutor() Passthrough {
	return Passthrough{Provider: "echo", BuildRequest: func(_ context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
		return pluginapi.HTTPRequest{Method: "POST", URL: "https://example.com/messages", Body: r.Payload, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
	}}
}
func TestPassthroughExecuteAndTokenEstimate(t *testing.T) {
	h := &localHostHTTP{status: 200, body: []byte(`{"usage":{"total_tokens":7}}`), headers: http.Header{"X-Example": {"yes"}}}
	p := echoExecutor()
	out, err := p.Execute(context.Background(), pluginapi.ExecutorRequest{Payload: []byte(`{"model":"echo"}`), HTTPClient: h})
	if err != nil || string(out.Payload) != string(h.body) || out.Headers.Get("X-Example") != "yes" || string(h.request.Body) != `{"model":"echo"}` {
		t.Fatalf("response %+v %v", out, err)
	}
	count, err := p.CountTokens(context.Background(), pluginapi.ExecutorRequest{Payload: []byte("123456789")})
	if err != nil || string(count.Payload) != `{"total_tokens":3}` || count.Metadata["estimated"] != true {
		t.Fatalf("count %+v %v", count, err)
	}
}
func TestPassthroughErrorMapper(t *testing.T) {
	h := &localHostHTTP{status: 429, body: []byte("never echo upstream body"), headers: http.Header{"Retry-After": {"12"}}}
	p := echoExecutor()
	_, err := p.Execute(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h})
	e := MapError(err)
	if e.HTTPStatus != 429 || e.RetryAfterSeconds != 12 || !e.Retryable || e.Message == string(h.body) {
		t.Fatalf("mapped %+v", e)
	}
	p.ClassifyError = func(r pluginapi.HTTPResponse) error {
		return &pluginabi.Error{Code: "plan_expired", Message: "plan expired", HTTPStatus: 429, RetryAfterSeconds: 600}
	}
	_, err = p.Execute(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h})
	e = MapError(err)
	if e.Code != "plan_expired" || e.Retryable || e.RetryAfterSeconds != 600 {
		t.Fatal(e)
	}
	if _, err = p.Execute(context.Background(), pluginapi.ExecutorRequest{}); err == nil {
		t.Fatal("used fallback transport")
	}
}
func TestPassthroughStreamAndHTTP(t *testing.T) {
	h := &localHostHTTP{status: 200, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte("one")}, {Payload: []byte("two")}, {Err: errors.New("broken")}}}
	p := echoExecutor()
	resp, err := p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var streamErr error
	for c := range resp.Chunks {
		text += string(c.Payload)
		streamErr = c.Err
	}
	if text != "onetwo" || streamErr == nil {
		t.Fatalf("stream %q %v", text, streamErr)
	}
	h.body = []byte("http")
	out, err := p.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "GET", URL: "https://example.com/check", HTTPClient: h})
	if err != nil || string(out.Body) != "http" || h.request.URL != "https://example.com/check" {
		t.Fatalf("http %+v %v", out, err)
	}
}
func TestPassthroughStreamInitialHTTPError(t *testing.T) {
	h := &localHostHTTP{status: 401, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte("sensitive-error")}}}
	p := echoExecutor()
	_, err := p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h})
	if e := MapError(err); e == nil || e.HTTPStatus != 401 {
		t.Fatalf("stream status %+v", e)
	}
}
