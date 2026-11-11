package providerkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
	e := rpc(t, r, "plugin.register", map[string]any{"schema_version": 10})
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
	e = rpc(t, r, "plugin.register", map[string]any{"schema_version": 10, "config_yaml": []byte("enabled: true\n")})
	if !e.OK {
		t.Fatal(e.Error)
	}
	var reg struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
	}
	json.Unmarshal(e.Result, &reg)
	if reg.SchemaVersion != 10 || reg.Metadata.Name != "echo" {
		t.Fatalf("registration: %+v", reg)
	}
	if e = rpc(t, r, "plugin.quiesce", nil); !e.OK {
		t.Fatal(e.Error)
	}
	if e = rpc(t, r, "auth.identifier", nil); e.OK || e.Error.Code != "not_ready" {
		t.Fatalf("quiesced: %+v", e)
	}
	if e = rpc(t, r, "plugin.reconfigure", map[string]any{"schema_version": 10, "config_yaml": []byte("enabled: false\n")}); !e.OK {
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
	if e = rpc(t, r, "plugin.register", map[string]any{"schema_version": 10}); e.OK {
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

// A newer host (schema 11) registers the plugin, which answers with the kit's
// own schema so the host gates newer features.
func TestNewerHostSchemaIsAccepted(t *testing.T) {
	r := newRuntime(t, Definition{Metadata: metadata()}, nil)
	var e pluginabi.Envelope
	json.Unmarshal(r.Call(context.Background(), "plugin.register", []byte(`{"schema_version":11}`)), &e)
	if !e.OK {
		t.Fatalf("schema 11 host refused: %+v", e.Error)
	}
	var resp struct {
		SchemaVersion uint32 `json:"schema_version"`
	}
	json.Unmarshal(e.Result, &resp)
	if resp.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("answered schema %d, want %d", resp.SchemaVersion, pluginabi.SchemaVersion)
	}
}

// A host older than the kit's schema (schema 9, CLIProxyAPI v8.0.48) is
// refused on both register and reconfigure.
func TestOlderHostSchemaIsRefused(t *testing.T) {
	r := newRuntime(t, Definition{Metadata: metadata()}, nil)
	for _, method := range []string{"plugin.register", "plugin.reconfigure"} {
		var e pluginabi.Envelope
		json.Unmarshal(r.Call(context.Background(), method, []byte(`{"schema_version":9}`)), &e)
		if e.OK || e.Error == nil || e.Error.Code != "unsupported_schema" {
			t.Fatalf("%s: schema 9 host not refused with unsupported_schema: %+v", method, e)
		}
	}
}
func TestSchemaAndMalformedRequests(t *testing.T) {
	r := newRuntime(t, Definition{Metadata: metadata()}, nil)
	for _, raw := range []string{`{"schema_version":8}`, `null`, `[]`, `{"schema_version":10} trailing`} {
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

// gatedExecutor blocks in Execute until released (or the context is
// canceled), so tests can hold work in flight across quiesce.
type gatedExecutor struct {
	startOnce sync.Once
	started   chan struct{}
	release   chan struct{}
	stubborn  bool
	payload   []byte
	chunks    chan pluginapi.ExecutorStreamChunk
	finished  atomic.Bool
}

func newGatedExecutor(payload string) *gatedExecutor {
	return &gatedExecutor{started: make(chan struct{}), release: make(chan struct{}), payload: []byte(payload)}
}
func (e *gatedExecutor) Identifier() string { return "echo" }
func (e *gatedExecutor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	e.startOnce.Do(func() { close(e.started) })
	if e.stubborn {
		// Ignore cancellation, like a provider stuck in a blocking call:
		// canceling the runtime does not unwind this execution.
		<-e.release
		e.finished.Store(true)
		return pluginapi.ExecutorResponse{Payload: e.payload}, nil
	}
	select {
	case <-e.release:
		e.finished.Store(true)
		return pluginapi.ExecutorResponse{Payload: e.payload}, nil
	case <-ctx.Done():
		return pluginapi.ExecutorResponse{}, ctx.Err()
	}
}
func (e *gatedExecutor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return pluginapi.ExecutorStreamResponse{Headers: http.Header{"Content-Type": {"text/event-stream"}}, Chunks: e.chunks}, nil
}
func (e *gatedExecutor) CountTokens(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{Payload: []byte(`{"total_tokens":1}`)}, nil
}
func (e *gatedExecutor) HttpRequest(context.Context, pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{StatusCode: 200}, nil
}

// callEnv is rpc without testing.T, usable from goroutines.
func callEnv(r *Runtime, method string, req any) pluginabi.Envelope {
	raw, _ := json.Marshal(req)
	out := r.Call(context.Background(), method, raw)
	var env pluginabi.Envelope
	_ = json.Unmarshal(out, &env)
	return env
}

func waitForNotReady(t *testing.T, r *Runtime) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		env := callEnv(r, "executor.identifier", nil)
		if !env.OK && env.Error != nil && env.Error.Code == "not_ready" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission never stopped: %+v", env)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func gatedCaps(e *gatedExecutor) pluginapi.Capabilities {
	return pluginapi.Capabilities{Executor: e, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}
}

func TestQuiesceDrainsInFlightExecution(t *testing.T) {
	e := newGatedExecutor("drained-result")
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: gatedCaps(e)}, nil)
	register(t, r)
	execDone := make(chan pluginabi.Envelope, 1)
	go func() { execDone <- callEnv(r, "executor.execute", map[string]any{"payload": []byte("in-flight")}) }()
	select {
	case <-e.started:
	case <-time.After(2 * time.Second):
		t.Fatal("execute never started")
	}
	quiesceDone := make(chan pluginabi.Envelope, 1)
	go func() { quiesceDone <- callEnv(r, "plugin.quiesce", nil) }()
	select {
	case env := <-quiesceDone:
		t.Fatalf("quiesce returned while work in flight: %+v", env)
	case <-time.After(50 * time.Millisecond):
	}
	close(e.release)
	select {
	case env := <-execDone:
		if !env.OK {
			t.Fatalf("in-flight execute failed: %+v", env.Error)
		}
		var resp struct {
			Payload []byte `json:"Payload"`
		}
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatalf("bad execute result: %v", err)
		}
		// ExecutorResponse serializes Payload as base64 string.
		if string(resp.Payload) != "drained-result" {
			t.Fatalf("execute payload %q", resp.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight execute never finished")
	}
	select {
	case env := <-quiesceDone:
		if !env.OK {
			t.Fatalf("quiesce: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce never finished")
	}
}

func TestQuiesceDrainsInFlightStream(t *testing.T) {
	chunks := make(chan pluginapi.ExecutorStreamChunk, 8)
	e := newGatedExecutor("unused")
	e.chunks = chunks
	var mu sync.Mutex
	var emitted [][]byte
	closes := 0
	streamClosed := make(chan struct{})
	h := CallerFunc(func(ctx context.Context, m string, b []byte) ([]byte, error) {
		var req struct {
			StreamID    string           `json:"stream_id"`
			Payload     []byte           `json:"payload"`
			ErrorDetail *pluginabi.Error `json:"error_detail"`
		}
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		defer mu.Unlock()
		switch m {
		case "host.stream.emit":
			emitted = append(emitted, append([]byte(nil), req.Payload...))
		case "host.stream.close":
			closes++
			close(streamClosed)
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: gatedCaps(e)}, h)
	register(t, r)
	if env := callEnv(r, "executor.execute_stream", map[string]any{"stream_id": "stream-quiesce"}); !env.OK {
		t.Fatalf("execute_stream: %+v", env.Error)
	}
	quiesceDone := make(chan pluginabi.Envelope, 1)
	go func() { quiesceDone <- callEnv(r, "plugin.quiesce", nil) }()
	waitForNotReady(t, r)
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("chunk-one")}
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("chunk-two")}
	close(chunks)
	select {
	case <-streamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	select {
	case env := <-quiesceDone:
		if !env.OK {
			t.Fatalf("quiesce: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce never finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if closes != 1 {
		t.Fatalf("stream closes %d, want 1", closes)
	}
	if len(emitted) != 2 || string(emitted[0]) != "chunk-one" || string(emitted[1]) != "chunk-two" {
		t.Fatalf("emitted %q", emitted)
	}
}

func TestQuiesceRefusesNewWork(t *testing.T) {
	e := newGatedExecutor("ok")
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: gatedCaps(e)}, nil)
	register(t, r)
	execDone := make(chan pluginabi.Envelope, 1)
	go func() { execDone <- callEnv(r, "executor.execute", map[string]any{}) }()
	select {
	case <-e.started:
	case <-time.After(2 * time.Second):
		t.Fatal("execute never started")
	}
	quiesceDone := make(chan pluginabi.Envelope, 1)
	go func() { quiesceDone <- callEnv(r, "plugin.quiesce", nil) }()
	waitForNotReady(t, r)
	select {
	case env := <-execDone:
		t.Fatalf("in-flight execute finished early: %+v", env)
	default:
	}
	close(e.release)
	select {
	case env := <-execDone:
		if !env.OK {
			t.Fatalf("in-flight execute: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight execute never finished")
	}
	select {
	case env := <-quiesceDone:
		if !env.OK {
			t.Fatalf("quiesce: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce never finished")
	}
	if env := callEnv(r, "executor.identifier", nil); env.OK || env.Error.Code != "not_ready" {
		t.Fatalf("after quiesce: %+v", env)
	}
}

func TestQuiesceDrainTimeoutCancelsStraggler(t *testing.T) {
	e := newGatedExecutor("never")
	e.release = nil // block on context only
	r := newRuntime(t, Definition{Metadata: metadata(), DrainTimeout: 100 * time.Millisecond, Capabilities: gatedCaps(e)}, nil)
	register(t, r)
	execDone := make(chan pluginabi.Envelope, 1)
	go func() { execDone <- callEnv(r, "executor.execute", map[string]any{}) }()
	select {
	case <-e.started:
	case <-time.After(2 * time.Second):
		t.Fatal("execute never started")
	}
	start := time.Now()
	env := callEnv(r, "plugin.quiesce", nil)
	elapsed := time.Since(start)
	if env.OK || env.Error == nil || env.Error.Code != "drain_timeout" {
		t.Fatalf("quiesce: %+v", env)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("quiesce exceeded its bound: %v", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("quiesce failed before its deadline: %v", elapsed)
	}
	select {
	case execEnv := <-execDone:
		if execEnv.OK {
			t.Fatal("straggler was not canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("straggler never unwound after cancel")
	}
}

func TestOnQuiesceRunsAfterDrain(t *testing.T) {
	t.Run("error aborts the swap", func(t *testing.T) {
		e := newGatedExecutor("ok")
		var calls atomic.Int32
		var sawFinished atomic.Bool
		r := newRuntime(t, Definition{
			Metadata:     metadata(),
			Capabilities: gatedCaps(e),
			OnQuiesce: func(context.Context) error {
				calls.Add(1)
				sawFinished.Store(e.finished.Load())
				return &pluginabi.Error{Code: "quiesce_denied", Message: "swap refused by hook", HTTPStatus: 503}
			},
		}, nil)
		register(t, r)
		execDone := make(chan pluginabi.Envelope, 1)
		go func() { execDone <- callEnv(r, "executor.execute", map[string]any{}) }()
		select {
		case <-e.started:
		case <-time.After(2 * time.Second):
			t.Fatal("execute never started")
		}
		quiesceDone := make(chan pluginabi.Envelope, 1)
		go func() { quiesceDone <- callEnv(r, "plugin.quiesce", nil) }()
		select {
		case env := <-quiesceDone:
			t.Fatalf("quiesce returned before drain: %+v", env)
		case <-time.After(50 * time.Millisecond):
		}
		if calls.Load() != 0 {
			t.Fatal("OnQuiesce ran before drain finished")
		}
		close(e.release)
		select {
		case env := <-execDone:
			if !env.OK {
				t.Fatalf("in-flight execute: %+v", env.Error)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("in-flight execute never finished")
		}
		select {
		case env := <-quiesceDone:
			if env.OK || env.Error == nil || env.Error.Code != "quiesce_denied" {
				t.Fatalf("quiesce: %+v", env)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("quiesce never finished")
		}
		if calls.Load() != 1 || !sawFinished.Load() {
			t.Fatalf("OnQuiesce calls %d sawFinished %v", calls.Load(), sawFinished.Load())
		}
	})
	t.Run("nil error allows the swap", func(t *testing.T) {
		var calls atomic.Int32
		r := newRuntime(t, Definition{
			Metadata:     metadata(),
			Capabilities: gatedCaps(newGatedExecutor("ok")),
			OnQuiesce:    func(context.Context) error { calls.Add(1); return nil },
		}, nil)
		register(t, r)
		if env := callEnv(r, "plugin.quiesce", nil); !env.OK {
			t.Fatalf("quiesce: %+v", env.Error)
		}
		if calls.Load() != 1 {
			t.Fatalf("OnQuiesce calls %d", calls.Load())
		}
	})
}
func TestQuiesceIdleNeverTimesOut(t *testing.T) {
	// Even with an absurdly short drain deadline an idle runtime must
	// quiesce successfully: there is no work to wait for.
	r := newRuntime(t, Definition{Metadata: metadata(), DrainTimeout: time.Nanosecond}, nil)
	register(t, r)
	for i := 0; i < 5; i++ {
		if env := callEnv(r, "plugin.quiesce", nil); !env.OK {
			t.Fatalf("idle quiesce %d: %+v", i, env)
		}
	}
}

func TestQuiesceTimeoutThenReregister(t *testing.T) {
	// A provider that ignores cancellation must not crash the host when a
	// timed-out quiesce is followed by the rollback re-registration and new
	// work. (Regression: the WaitGroup-based waiter panicked on reuse.)
	e := newGatedExecutor("stubborn-result")
	e.stubborn = true
	r := newRuntime(t, Definition{Metadata: metadata(), DrainTimeout: 50 * time.Millisecond, Capabilities: gatedCaps(e)}, nil)
	register(t, r)
	execDone := make(chan pluginabi.Envelope, 1)
	go func() { execDone <- callEnv(r, "executor.execute", map[string]any{}) }()
	select {
	case <-e.started:
	case <-time.After(2 * time.Second):
		t.Fatal("execute never started")
	}
	// Timed-out quiesce cancels the root but the stubborn call keeps running.
	if env := callEnv(r, "plugin.quiesce", nil); env.OK || env.Error.Code != "drain_timeout" {
		t.Fatalf("quiesce: %+v", env)
	}
	// A repeated quiesce before re-registration must not panic either.
	if env := callEnv(r, "plugin.quiesce", nil); env.OK || env.Error.Code != "drain_timeout" {
		t.Fatalf("second quiesce: %+v", env)
	}
	// Rollback re-registration drains the stubborn call: release it, then the
	// registration completes and admits new work.
	regDone := make(chan pluginabi.Envelope, 1)
	go func() { regDone <- callEnv(r, "plugin.register", map[string]any{"schema_version": 10}) }()
	close(e.release)
	select {
	case env := <-execDone:
		if !env.OK {
			t.Fatalf("stubborn execute: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stubborn execute never finished")
	}
	select {
	case env := <-regDone:
		if !env.OK {
			t.Fatalf("re-register: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("re-register never finished")
	}
	if env := callEnv(r, "executor.identifier", nil); !env.OK {
		t.Fatalf("work after re-register refused: %+v", env)
	}
}

func TestQuiesceCanceledReturnsPromptly(t *testing.T) {
	// A canceled quiesce caller does not wait for the full drain: the call
	// returns promptly, in-flight work is not canceled, and admission stays
	// off until the next registration.
	e := newGatedExecutor("kept-result")
	r := newRuntime(t, Definition{Metadata: metadata(), DrainTimeout: 30 * time.Second, Capabilities: gatedCaps(e)}, nil)
	register(t, r)
	execDone := make(chan pluginabi.Envelope, 1)
	go func() { execDone <- callEnv(r, "executor.execute", map[string]any{}) }()
	select {
	case <-e.started:
	case <-time.After(2 * time.Second):
		t.Fatal("execute never started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	quiesceDone := make(chan pluginabi.Envelope, 1)
	go func() {
		raw, _ := json.Marshal(nil)
		var env pluginabi.Envelope
		_ = json.Unmarshal(r.Call(ctx, "plugin.quiesce", raw), &env)
		quiesceDone <- env
	}()
	waitForNotReady(t, r)
	cancel()
	select {
	case env := <-quiesceDone:
		if env.OK || env.Error.Code != "canceled" {
			t.Fatalf("canceled quiesce: %+v", env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled quiesce never returned")
	}
	select {
	case env := <-execDone:
		t.Fatalf("in-flight execute was canceled by quiesce cancel: %+v", env)
	default:
	}
	close(e.release)
	select {
	case env := <-execDone:
		if !env.OK {
			t.Fatalf("in-flight execute: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight execute never finished")
	}
	if env := callEnv(r, "executor.identifier", nil); env.OK || env.Error.Code != "not_ready" {
		t.Fatalf("after canceled quiesce: %+v", env)
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

type schedulerStub struct{ pluginapi.Scheduler }
type authStub struct{ pluginapi.AuthProvider }
type multiAuthStub struct{ pluginapi.AuthProvider }

func (multiAuthStub) Identifiers() []string { return []string{"a", "b"} }

func TestNewRejectsUnsafeDefinitions(t *testing.T) {
	executor := &testExecutor{}
	for name, c := range map[string]pluginapi.Capabilities{
		"executor without input formats":  {Executor: executor, ExecutorOutputFormats: []string{"openai"}},
		"executor without output formats": {Executor: executor, ExecutorInputFormats: []string{"openai"}},
		"invalid model scope":             {Executor: executor, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, ExecutorModelScope: "everything"},
		// Advertising a capability the runtime never dispatches would leave host calls unanswered.
		"undispatched capability": {Scheduler: schedulerStub{}},
		// Schema 10 multi-auth registration would be silently reduced to one identifier.
		"auth providers list": {AuthProviders: []pluginapi.AuthProvider{authStub{}}},
		"multi auth provider": {AuthProvider: multiAuthStub{}},
	} {
		if _, err := New(Definition{Metadata: metadata(), Capabilities: c}, nil); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	for _, repo := range []string{"http://github.com/example/echo", "https://user@github.com/example/echo", "https://github.com/example/echo?x=1", "https://github.com/example/echo#readme", "https://github.com/example", "https://github.com/example/echo/tree"} {
		m := metadata()
		m.GitHubRepository = repo
		if err := ValidateMetadata(m); err == nil {
			t.Fatalf("accepted repository %q", repo)
		}
	}
}
func TestRegistrationAdvertisesOnlyConfiguredCapabilities(t *testing.T) {
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{Executor: &testExecutor{}, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"claude"}}}, nil)
	env := rpc(t, r, "plugin.register", map[string]any{"schema_version": 10})
	if !env.OK {
		t.Fatal(env.Error)
	}
	var reg struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	c := reg.Capabilities
	if c["executor"] != true || c["executor_model_scope"] != string(pluginapi.ExecutorModelScopeBoth) || c["auth_provider"] != false || c["quota_provider"] != false || c["usage_plugin"] != false {
		t.Fatalf("capabilities %v", c)
	}
	if in, _ := json.Marshal(c["executor_input_formats"]); string(in) != `["openai"]` {
		t.Fatalf("input formats %s", in)
	}
}
func TestRuntimeThreadsHostCallbackScope(t *testing.T) {
	var scopes []string
	h := CallerFunc(func(_ context.Context, m string, raw []byte) ([]byte, error) {
		var req struct {
			Scope string `json:"host_callback_id"`
		}
		json.Unmarshal(raw, &req)
		scopes = append(scopes, m+"="+req.Scope)
		switch m {
		case "host.http.operation_open":
			return []byte(`{"ok":true,"result":{"operation_id":"op"}}`), nil
		case "host.http.do":
			return []byte(`{"ok":true,"result":{"StatusCode":200}}`), nil
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{Executor: echoExecutor(), ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}}, h)
	register(t, r)
	// The callback ID scopes host-side credentials; losing it would run the request unscoped.
	if env := rpc(t, r, "executor.http_request", map[string]any{"Method": "GET", "URL": "https://example.com", "host_callback_id": "scope-9"}); !env.OK {
		t.Fatal(env.Error)
	}
	if len(scopes) != 3 || scopes[0] != "host.http.operation_open=scope-9" || scopes[1] != "host.http.do=scope-9" || scopes[2] != "host.http.cancel=scope-9" {
		t.Fatalf("scopes %q", scopes)
	}
}
func TestStreamDispatchRejectsInvalidSetup(t *testing.T) {
	ok := CallerFunc(func(context.Context, string, []byte) ([]byte, error) { return []byte(`{"ok":true,"result":{}}`), nil })
	caps := func(e pluginapi.ProviderExecutor) pluginapi.Capabilities {
		return pluginapi.Capabilities{Executor: e, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}
	}
	for name, tc := range map[string]struct {
		host Caller
		exec pluginapi.ProviderExecutor
		req  map[string]any
		code string
	}{
		"no stream id":        {ok, &testExecutor{chunks: make(chan pluginapi.ExecutorStreamChunk)}, map[string]any{}, "invalid_request"},
		"no host bridge":      {nil, &testExecutor{chunks: make(chan pluginapi.ExecutorStreamChunk)}, map[string]any{"stream_id": "s"}, "invalid_request"},
		"nil executor stream": {ok, &testExecutor{}, map[string]any{"stream_id": "s"}, "invalid_stream"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: caps(tc.exec)}, tc.host)
			register(t, r)
			if env := rpc(t, r, "executor.execute_stream", tc.req); env.OK || env.Error.Code != tc.code {
				t.Fatalf("env %+v", env)
			}
			// A rejected stream must not leave work registered, or quiesce would hang.
			done := make(chan struct{})
			go func() {
				defer close(done)
				if env := rpc(t, r, "plugin.quiesce", nil); !env.OK {
					t.Errorf("quiesce: %+v", env)
				}
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("rejected stream left work in flight")
			}
		})
	}
}
func TestStreamEmitFailureClosesWithError(t *testing.T) {
	chunks := make(chan pluginapi.ExecutorStreamChunk, 4)
	var mu sync.Mutex
	var emits int
	var detail *pluginabi.Error
	closed := make(chan struct{})
	h := CallerFunc(func(_ context.Context, m string, raw []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch m {
		case "host.stream.emit":
			emits++
			return []byte(`{"ok":false,"error":{"code":"client_gone","message":"client disconnected"}}`), nil
		case "host.stream.close":
			var req struct {
				Detail *pluginabi.Error `json:"error_detail"`
			}
			json.Unmarshal(raw, &req)
			detail = req.Detail
			close(closed)
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{Executor: &testExecutor{chunks: chunks}, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}}, h)
	register(t, r)
	chunks <- pluginapi.ExecutorStreamChunk{}
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("one")}
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("two")}
	if env := rpc(t, r, "executor.execute_stream", map[string]any{"stream_id": "s"}); !env.OK {
		t.Fatal(env.Error)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stream not closed after emit failure")
	}
	mu.Lock()
	defer mu.Unlock()
	if emits != 1 || detail == nil || detail.Code != "host_error" {
		t.Fatalf("emits %d detail %+v", emits, detail)
	}
}
