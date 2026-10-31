package providerkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: "echo", Version: "0.1.0", Author: "Example Organization", GitHubRepository: "https://github.com/example/echo"}
}
func rpc(t *testing.T, r *Runtime, method string, req any) pluginabi.Envelope {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out := r.Call(context.Background(), method, raw)
	var env pluginabi.Envelope
	if err = json.Unmarshal(out, &env); err != nil {
		t.Fatalf("bad envelope %s: %v", out, err)
	}
	return env
}
func newRuntime(t *testing.T, d Definition, h Caller) *Runtime {
	t.Helper()
	r, err := New(d, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Shutdown(context.Background()) })
	return r
}
func register(t *testing.T, r *Runtime) {
	t.Helper()
	e := rpc(t, r, "plugin.register", map[string]any{"schema_version": 9})
	if !e.OK {
		t.Fatalf("register: %+v", e.Error)
	}
}
func TestRuntimeLifecycle(t *testing.T) {
	var configs []string
	var stopped int
	r := newRuntime(t, Definition{Metadata: metadata(), Configure: func(_ context.Context, b []byte) error { configs = append(configs, string(b)); return nil }, OnShutdown: func(context.Context) error { stopped++; return nil }}, nil)
	e := rpc(t, r, "model.static", nil)
	if e.OK || e.Error.Code != "not_ready" {
		t.Fatalf("before register: %+v", e)
	}
	e = rpc(t, r, "plugin.register", map[string]any{"schema_version": 9, "config_yaml": []byte("enabled: true\n")})
	if !e.OK {
		t.Fatal(e.Error)
	}
	var reg struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
	}
	json.Unmarshal(e.Result, &reg)
	if reg.SchemaVersion != 9 || reg.Metadata.Name != "echo" {
		t.Fatalf("registration: %+v", reg)
	}
	if e = rpc(t, r, "plugin.quiesce", nil); !e.OK {
		t.Fatal(e.Error)
	}
	if e = rpc(t, r, "auth.identifier", nil); e.OK || e.Error.Code != "not_ready" {
		t.Fatalf("quiesced: %+v", e)
	}
	if e = rpc(t, r, "plugin.reconfigure", map[string]any{"schema_version": 9, "config_yaml": []byte("enabled: false\n")}); !e.OK {
		t.Fatal(e.Error)
	}
	if len(configs) != 2 || configs[0] != "enabled: true\n" || configs[1] != "enabled: false\n" {
		t.Fatalf("config: %q", configs)
	}
	if e = rpc(t, r, "unknown", nil); e.OK || e.Error.Code != "unknown_method" {
		t.Fatalf("unknown: %+v", e)
	}
	r.Shutdown(context.Background())
	r.Shutdown(context.Background())
	if stopped != 1 {
		t.Fatalf("shutdown count %d", stopped)
	}
	if e = rpc(t, r, "plugin.register", map[string]any{"schema_version": 9}); e.OK {
		t.Fatal("resurrected shutdown runtime")
	}
}
func TestMetadataValidation(t *testing.T) {
	for _, field := range []string{"name", "version", "author", "repo"} {
		t.Run(field, func(t *testing.T) {
			m := metadata()
			switch field {
			case "name":
				m.Name = " "
			case "version":
				m.Version = ""
			case "author":
				m.Author = ""
			case "repo":
				m.GitHubRepository = "https://example.com/not-github"
			}
			if _, err := New(Definition{Metadata: m}, nil); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}
func TestSchemaAndMalformedRequests(t *testing.T) {
	r := newRuntime(t, Definition{Metadata: metadata()}, nil)
	for _, raw := range []string{`{"schema_version":8}`, `{"schema_version":10}`, `null`, `[]`, `{"schema_version":9} trailing`} {
		var e pluginabi.Envelope
		json.Unmarshal(r.Call(context.Background(), "plugin.register", []byte(raw)), &e)
		if e.OK {
			t.Fatalf("accepted %s", raw)
		}
	}
	register(t, r)
	if e := rpc(t, r, "auth.parse", map[string]any{}); e.OK || e.Error.Code != "unsupported_method" {
		t.Fatalf("missing cap: %+v", e)
	}
}
func TestErrorMapping(t *testing.T) {
	e := MapError(fmt.Errorf("wrapped: %w", &pluginabi.Error{Code: "exhausted", Message: "limit reached", HTTPStatus: 429, Retryable: true, RetryAfterSeconds: 75}))
	if e.Code != "exhausted" || e.HTTPStatus != 429 || !e.Retryable || e.RetryAfterSeconds != 75 {
		t.Fatalf("mapped %+v", e)
	}
	if e := MapError(errors.New("credential=never-echo-me")); strings.Contains(e.Message, "never-echo-me") || e.HTTPStatus != 500 {
		t.Fatalf("unsafe generic error: %+v", e)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		s    string
		want int
	}{{"90", 90}, {"-1", 0}, {"garbage", 0}, {now.Add(5 * time.Second).Format(http.TimeFormat), 5}} {
		if got := RetryAfterSeconds(tc.s, now); got != tc.want {
			t.Fatalf("retry %q: %d", tc.s, got)
		}
	}
}

type testExecutor struct {
	start        chan struct{}
	chunks       chan pluginapi.ExecutorStreamChunk
	panicExecute bool
}

func (e *testExecutor) Identifier() string { return "echo" }
func (e *testExecutor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	if e.panicExecute {
		panic("sensitive panic")
	}
	if e.start != nil {
		close(e.start)
		<-ctx.Done()
		return pluginapi.ExecutorResponse{}, ctx.Err()
	}
	return pluginapi.ExecutorResponse{Payload: r.Payload}, nil
}
func (e *testExecutor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return pluginapi.ExecutorStreamResponse{Headers: http.Header{"Content-Type": {"text/event-stream"}}, Chunks: e.chunks}, nil
}
func (e *testExecutor) CountTokens(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{Payload: []byte(`{"total_tokens":1}`)}, nil
}
func (e *testExecutor) HttpRequest(context.Context, pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{StatusCode: 200}, nil
}
func TestQuiesceCancelsInFlight(t *testing.T) {
	e := &testExecutor{start: make(chan struct{})}
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{Executor: e, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}}, nil)
	register(t, r)
	done := make(chan struct{})
	go func() { defer close(done); r.Call(context.Background(), "executor.execute", []byte(`{}`)) }()
	<-e.start
	if env := rpc(t, r, "plugin.quiesce", nil); !env.OK {
		t.Fatal(env.Error)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("quiesce left call alive")
	}
}
func TestRuntimePanicIsRedacted(t *testing.T) {
	e := &testExecutor{panicExecute: true}
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{Executor: e, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}}, nil)
	register(t, r)
	env := rpc(t, r, "executor.execute", map[string]any{})
	if env.OK || strings.Contains(string(env.Error.Message), "sensitive") {
		t.Fatalf("panic leaked: %+v", env)
	}
}
func TestAsyncStreamEmitsAndCloses(t *testing.T) {
	chunks := make(chan pluginapi.ExecutorStreamChunk)
	e := &testExecutor{chunks: chunks}
	var mu sync.Mutex
	var methods []string
	var detail *pluginabi.Error
	done := make(chan struct{})
	h := CallerFunc(func(ctx context.Context, m string, b []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, m)
		var req struct {
			StreamID    string           `json:"stream_id"`
			Payload     []byte           `json:"payload"`
			ErrorDetail *pluginabi.Error `json:"error_detail"`
		}
		json.Unmarshal(b, &req)
		if req.StreamID != "stream-1" {
			return nil, errors.New("wrong stream")
		}
		if m == "host.stream.emit" && string(req.Payload) != "data: hello\n\n" {
			return nil, errors.New("bad bytes")
		}
		if m == "host.stream.close" {
			detail = req.ErrorDetail
			close(done)
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{Executor: e, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}}, h)
	register(t, r)
	env := rpc(t, r, "executor.execute_stream", map[string]any{"stream_id": "stream-1"})
	if !env.OK {
		t.Fatal(env.Error)
	}
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("data: hello\n\n")}
	chunks <- pluginapi.ExecutorStreamChunk{Err: &pluginabi.Error{Code: "exhausted", Message: "limit", HTTPStatus: 429, RetryAfterSeconds: 30}}
	close(chunks)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 || methods[0] != "host.stream.emit" || methods[1] != "host.stream.close" || detail == nil || detail.HTTPStatus != 429 || detail.RetryAfterSeconds != 30 {
		t.Fatalf("methods %v error %+v", methods, detail)
	}
}
