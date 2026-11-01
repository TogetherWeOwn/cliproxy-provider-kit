//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

type executors map[string]coreauth.ProviderExecutor

func (m executors) Executor(p string) (coreauth.ProviderExecutor, bool) { e, ok := m[p]; return e, ok }
func (m executors) RegisterExecutor(e coreauth.ProviderExecutor)        { m[e.Identifier()] = e }
func (m executors) UnregisterExecutor(p string)                         { delete(m, p) }

type models struct{}

func (models) RegisterClient(string, string, []*registry.ModelInfo) {}
func (models) UnregisterClient(string)                              {}
func (models) GetModelProviders(string) []string                    { return nil }

// This is not a reimplementation of dlopen or RPC: Host.ApplyConfig invokes the
// unmodified v8.0.49 dynamicLibraryLoader, registerRPCPlugin and real adapters.
func TestRealHostLoader(t *testing.T) {
	var requests atomic.Int32
	var streamReleased atomic.Bool
	continueStream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer example-key" {
			t.Error("auth header was not injected")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var input map[string]any
		if err = json.Unmarshal(body, &input); err != nil {
			t.Error(err)
			return
		}
		if input["model"] != "echo-model" {
			t.Errorf("model: %v", input["model"])
		}
		if input["error"] == true {
			w.Header().Set("Retry-After", "23")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"fixture limit"}`))
			return
		}
		if input["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"id\":\"echo\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			// Do not finish the response until the client receives the first chunk.
			// A whole-response buffering implementation would deadlock this gate.
			select {
			case <-continueStream:
			case <-r.Context().Done():
				return
			}
			for i := 0; i < 40; i++ {
				select {
				case <-r.Context().Done():
					return
				default:
				}
				w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"!\"}}]}\n\n"))
				w.(http.Flusher).Flush()
			}
			w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"))
			streamReleased.Store(true)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"echo","object":"chat.completion","model":"echo-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer upstream.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archDir := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(archDir, 0755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(archDir, "echo.so")
	if runtime.GOOS == "darwin" {
		lib = filepath.Join(archDir, "echo.dylib")
	}
	build := exec.Command("go", "build", "-buildmode=c-shared", "-o", lib, "./examples/echo")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("c-shared build: %v\n%s", err, out)
	}
	enabled := true
	var raw yaml.Node
	if err = yaml.Unmarshal([]byte("enabled: true\nupstream_url: "+upstream.URL+"\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"echo": {Enabled: &enabled, Raw: *raw.Content[0]}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host.ApplyConfig(ctx, cfg)
	if !host.PluginLoaded("echo") || !host.PluginRegistered("echo") {
		t.Fatalf("real loader did not register plugin: %+v", host.RegisteredPlugins())
	}
	if !host.HasAuthProvider("echo") || !host.HasQuotaProvider("echo") {
		t.Fatal("capabilities missing")
	}
	infos := host.RegisteredPlugins()
	if len(infos) != 1 || infos[0].Metadata.Version != "0.1.0" || !infos[0].SupportsQuota {
		t.Fatalf("registration: %+v", infos)
	}
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "echo", FileName: "example.json", RawJSON: []byte(`{"type":"echo","api_key":"example-key","label":"Example"}`)})
	if err != nil || !handled || auth == nil || auth.Provider != "echo" {
		t.Fatalf("parse %v %v %v", auth, handled, err)
	}
	// Exercise the real host merge/persistence path, not just SDK unmarshalling.
	authPath := filepath.Join(cfg.AuthDir, "saved.json")
	if err = auth.Storage.SaveTokenToFile(authPath); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if json.Unmarshal(saved, &fields) != nil || fields["type"] != "echo" || fields["api_key"] != "example-key" || fields["label"] != "Example" {
		t.Fatalf("host storage merge failed")
	}
	quota, handled, err := host.FetchQuota(ctx, pluginapi.QuotaFetchRequest{AuthID: auth.ID, Provider: "echo", StorageJSON: saved})
	if err != nil || !handled || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].RemainingFraction != 1 {
		t.Fatalf("quota %+v %v %v", quota, handled, err)
	}
	m := executors{}
	host.RegisterModels(ctx, models{})
	host.RegisterExecutors(m, models{})
	executor, ok := m["echo"]
	if !ok {
		t.Fatal("real executor adapter not registered")
	}
	payload := []byte(`{"model":"echo-model","messages":[{"role":"user","content":"hello"}]}`)
	req := coreexecutor.Request{Model: "echo-model", Payload: payload, Format: sdktranslator.FormatOpenAI}
	opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI}
	resp, err := executor.Execute(ctx, auth, req, opts)
	if err != nil || !strings.Contains(string(resp.Payload), `"total_tokens":5`) {
		t.Fatalf("execute %s %v", resp.Payload, err)
	}
	count, err := executor.CountTokens(ctx, auth, req, opts)
	if err != nil || !strings.Contains(string(count.Payload), "total_tokens") {
		t.Fatalf("count %s %v", count.Payload, err)
	}
	streamReq := req
	streamReq.Payload = []byte(`{"model":"echo-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	streamOpts := opts
	streamOpts.Stream = true
	stream, err := executor.ExecuteStream(ctx, auth, streamReq, streamOpts)
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	select {
	case first, ok := <-stream.Chunks:
		if !ok || first.Err != nil || len(first.Payload) == 0 {
			t.Fatalf("missing initial chunk: %+v", first)
		}
		streamed.Write(first.Payload)
		if streamReleased.Load() {
			t.Fatal("upstream finished before initial chunk was observed")
		}
		close(continueStream)
	case <-ctx.Done():
		t.Fatal("stream did not deliver before upstream completion")
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	if !strings.Contains(streamed.String(), "[DONE]") || !strings.Contains(streamed.String(), "hello") || !streamReleased.Load() {
		t.Fatalf("incomplete stream %s", streamed.String())
	}
	limited := req
	limited.Payload = []byte(`{"model":"echo-model","error":true}`)
	_, err = executor.Execute(ctx, auth, limited, opts)
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != 429 {
		t.Fatalf("status mapping %T %v", err, err)
	}
	retry, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retry.RetryAfter() == nil || *retry.RetryAfter() != 23*time.Second {
		t.Fatalf("retry-after %T %v", err, err)
	}
	if requests.Load() != 3 {
		t.Fatalf("unexpected mock request count %d", requests.Load())
	}
	// Force reconfiguration through the host lifecycle, then prove calls still work.
	var changed yaml.Node
	yaml.Unmarshal([]byte("enabled: true\nupstream_url: "+upstream.URL+"\nfixture_revision: 2\n"), &changed)
	cfg.Plugins.Configs["echo"] = config.PluginInstanceConfig{Enabled: &enabled, Raw: *changed.Content[0]}
	host.ApplyConfig(ctx, cfg)
	host.RegisterModels(ctx, models{})
	host.RegisterExecutors(m, models{})
	executor = m["echo"]
	if _, err = executor.Execute(ctx, auth, req, opts); err != nil {
		t.Fatalf("after reconfigure: %v", err)
	}
	host.ShutdownAll()
	if host.PluginRegistered("echo") {
		t.Fatal("plugin survived shutdown")
	}
	// The host keeps Go c-shared images resident; reinitialization must create a
	// fresh runtime when the same image is loaded again after logical shutdown.
	host.ApplyConfig(ctx, cfg)
	if !host.PluginRegistered("echo") {
		t.Fatal("resident image did not reinitialize")
	}
}
