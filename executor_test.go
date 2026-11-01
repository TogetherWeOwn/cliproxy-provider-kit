package providerkit

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

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
	// ExecuteStream re-frames upstream SSE: one data-framed chunk per event
	// here (no request_path metadata), and the terminal error still arrives.
	h := &localHostHTTP{status: 200, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte("data: one\n\n")}, {Payload: []byte("data: two")}, {Payload: []byte("\n\n"), Err: errors.New("broken")}}}
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
	if text != "data: onedata: two" || streamErr == nil {
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

type funcHostHTTP struct {
	do       func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	doStream func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error)
}

func (h funcHostHTTP) Do(ctx context.Context, q pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return h.do(ctx, q)
}
func (h funcHostHTTP) DoStream(ctx context.Context, q pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return h.doStream(ctx, q)
}

func TestPassthroughRequestFailuresNeverReachHost(t *testing.T) {
	var hostCalls int
	h := funcHostHTTP{do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		hostCalls++
		return pluginapi.HTTPResponse{StatusCode: 200}, nil
	}}
	if _, err := (Passthrough{Provider: "echo"}).Execute(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h}); MapError(err).Code != "invalid_executor" {
		t.Fatalf("nil BuildRequest %v", err)
	}
	built := errors.New("build failed")
	p := Passthrough{Provider: "echo", BuildRequest: func(context.Context, pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
		return pluginapi.HTTPRequest{}, built
	}}
	if _, err := p.Execute(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h}); !errors.Is(err, built) {
		t.Fatalf("Execute build error %v", err)
	}
	if _, err := p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h}); !errors.Is(err, built) {
		t.Fatalf("ExecuteStream build error %v", err)
	}
	prepared := errors.New("prepare failed")
	p.PrepareHTTP = func(context.Context, pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
		return pluginapi.HTTPRequest{}, prepared
	}
	if _, err := p.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{HTTPClient: h}); !errors.Is(err, prepared) {
		t.Fatalf("PrepareHTTP error %v", err)
	}
	// No fallback transport: a missing host client is an error, not a direct request.
	if _, err := p.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{URL: "https://example.com"}); MapError(err).Code != "host_unavailable" || MapError(err).HTTPStatus != 503 {
		t.Fatalf("nil HTTP client %v", err)
	}
	if hostCalls != 0 {
		t.Fatalf("failed request preparation reached the host %d times", hostCalls)
	}
}

func TestPassthroughHTTPRequestPreparesAndClassifies(t *testing.T) {
	h := &localHostHTTP{status: 200, body: []byte("ok")}
	p := echoExecutor()
	p.PrepareHTTP = func(_ context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
		return pluginapi.HTTPRequest{Method: r.Method, URL: r.URL, Headers: http.Header{"Authorization": {"Bearer example-key"}}}, nil
	}
	if _, err := p.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "GET", URL: "https://example.com/v1/models", HTTPClient: h}); err != nil || h.request.Headers.Get("Authorization") != "Bearer example-key" {
		t.Fatalf("prepared request not sent: %+v %v", h.request, err)
	}
	for _, status := range []int{199, 302, 401, 500} {
		h.status, h.body = status, []byte("upstream body with example-key")
		out, err := p.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Method: "GET", URL: "https://example.com", HTTPClient: h})
		if e := MapError(err); e == nil || e.Code != "upstream_error" || len(out.Body) != 0 || strings.Contains(e.Message, "example-key") {
			t.Fatalf("status %d passed through: %+v %+v", status, out, e)
		}
		if _, err = p.Execute(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h}); err == nil {
			t.Fatalf("Execute accepted status %d", status)
		}
	}
	// A classifier that declines a response must fall back to the default mapping, not success.
	h.status = 401
	p.ClassifyError = func(pluginapi.HTTPResponse) error { return nil }
	if _, err := p.Execute(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h}); MapError(err).HTTPStatus != 401 {
		t.Fatalf("declined classification %v", err)
	}
	transport := errors.New("dial failed")
	failing := funcHostHTTP{do: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 200}, transport
	}}
	if out, err := p.Execute(context.Background(), pluginapi.ExecutorRequest{HTTPClient: failing}); !errors.Is(err, transport) || out.Payload != nil {
		t.Fatalf("transport error %+v %v", out, err)
	}
}

func TestPassthroughStreamErrorBodyIsBounded(t *testing.T) {
	var classified []byte
	p := echoExecutor()
	p.ClassifyError = func(r pluginapi.HTTPResponse) error {
		classified = r.Body
		return &pluginabi.Error{Code: "exhausted", Message: "quota exhausted", HTTPStatus: r.StatusCode}
	}
	stream := func(status int, chunks ...pluginapi.HTTPStreamChunk) funcHostHTTP {
		return funcHostHTTP{doStream: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
			out := make(chan pluginapi.HTTPStreamChunk, len(chunks))
			for _, c := range chunks {
				out <- c
			}
			close(out)
			return pluginapi.HTTPStreamResponse{StatusCode: status, Chunks: out}, nil
		}}
	}
	_, err := p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: stream(429, pluginapi.HTTPStreamChunk{Payload: []byte(`{"error":`)}, pluginapi.HTTPStreamChunk{Payload: []byte(`"exhausted"}`)})})
	if e := MapError(err); e.Code != "exhausted" || string(classified) != `{"error":"exhausted"}` {
		t.Fatalf("classifier saw %q: %+v", classified, e)
	}
	half := make([]byte, maxQuotaBody/2+1)
	_, err = p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: stream(500, pluginapi.HTTPStreamChunk{Payload: half}, pluginapi.HTTPStreamChunk{Payload: half}, pluginapi.HTTPStreamChunk{Payload: half})})
	if e := MapError(err); e.Code != "exhausted" || len(classified) != len(half) {
		t.Fatalf("unbounded error body: %d bytes, %+v", len(classified), e)
	}
	_, err = p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: stream(200)})
	if err != nil {
		t.Fatal(err)
	}
	bad := funcHostHTTP{doStream: func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
		return pluginapi.HTTPStreamResponse{StatusCode: 200}, nil
	}}
	if _, err = p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: bad}); MapError(err).Code != "invalid_stream" {
		t.Fatalf("nil chunks %v", err)
	}
	refused := errors.New("refused")
	bad.doStream = func(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
		return pluginapi.HTTPStreamResponse{}, refused
	}
	if _, err = p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: bad}); !errors.Is(err, refused) {
		t.Fatalf("DoStream error %v", err)
	}
}

func TestPassthroughStreamReleasesHostOperation(t *testing.T) {
	var hostCtx context.Context
	upstream := make(chan pluginapi.HTTPStreamChunk)
	h := funcHostHTTP{doStream: func(ctx context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
		hostCtx = ctx
		return pluginapi.HTTPStreamResponse{StatusCode: 200, Chunks: upstream}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := echoExecutor().ExecuteStream(ctx, pluginapi.ExecutorRequest{HTTPClient: h})
	if err != nil {
		t.Fatal(err)
	}
	// ExecuteStream re-frames upstream SSE, so the fixture speaks SSE: one
	// data-framed chunk per event without a request_path signal.
	upstream <- pluginapi.HTTPStreamChunk{Payload: []byte("data: one\n\n")}
	if c := <-resp.Chunks; string(c.Payload) != "data: one" {
		t.Fatalf("chunk %+v", c)
	}
	// The consumer goes away while upstream is idle: the stream must end and cancel the host operation.
	cancel()
	select {
	case _, ok := <-resp.Chunks:
		if ok {
			t.Fatal("chunk after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("stream outlived its context")
	}
	select {
	case <-hostCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("host stream context never canceled")
	}

	// Normal completion must also release the host-side operation.
	upstream = make(chan pluginapi.HTTPStreamChunk, 1)
	upstream <- pluginapi.HTTPStreamChunk{Payload: []byte("data: done\n\n")}
	close(upstream)
	resp, err = echoExecutor().ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h})
	if err != nil {
		t.Fatal(err)
	}
	for range resp.Chunks {
	}
	select {
	case <-hostCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("completed stream leaked its host operation")
	}
}
