package providerkit

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type testCatalog struct{}

func (testCatalog) RegisterModels(context.Context, pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{Registrations: []pluginapi.ProviderModels{{Provider: "echo", Models: []pluginapi.ModelInfo{{ID: "echo/echo-model"}}}}}, nil
}
func (testCatalog) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: "echo", Models: []pluginapi.ModelInfo{{ID: "echo/echo-model"}}}, nil
}
func (testCatalog) ModelsForAuth(_ context.Context, r pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	if r.HTTPClient == nil {
		return pluginapi.ModelResponse{}, errors.New("no scoped client")
	}
	return pluginapi.ModelResponse{Provider: r.AuthProvider, Models: []pluginapi.ModelInfo{{ID: "echo/auth-model"}}}, nil
}

type usageCounter struct{ n atomic.Int32 }

func (u *usageCounter) HandleUsage(context.Context, pluginapi.UsageRecord) { u.n.Add(1) }
func TestRuntimeCapabilityDispatch(t *testing.T) {
	usage := &usageCounter{}
	q := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		return quotaFixture(.75), nil
	})
	r := newRuntime(t, Definition{Metadata: metadata(), ModelNamespace: "echo", Capabilities: pluginapi.Capabilities{AuthProvider: APIKeyAuth{Provider: "echo"}, ModelProvider: testCatalog{}, ModelRegistrar: testCatalog{}, Executor: &testExecutor{}, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, QuotaProvider: q, UsagePlugin: usage}}, nil)
	register(t, r)
	for _, tc := range []struct {
		method string
		req    any
		key    string
		want   string
	}{
		{"auth.identifier", map[string]any{}, "identifier", `"echo"`},
		{"auth.parse", pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","api_key":"example-key"}`)}, "Handled", `true`},
		{"auth.refresh", pluginapi.AuthRefreshRequest{AuthID: "a", StorageJSON: []byte(`{"api_key":"example-key"}`)}, "Auth", ``},
		{"model.register", pluginapi.ModelRegistrationRequest{}, "registrations", ``},
		{"model.static", pluginapi.StaticModelRequest{}, "Provider", `"echo"`},
		{"model.for_auth", pluginapi.AuthModelRequest{AuthProvider: "echo"}, "Provider", `"echo"`},
		{"executor.identifier", map[string]any{}, "identifier", `"echo"`},
		{"executor.execute", pluginapi.ExecutorRequest{Payload: []byte("echo")}, "Payload", `"ZWNobw=="`},
		{"executor.count_tokens", pluginapi.ExecutorRequest{}, "Payload", ``},
		{"executor.http_request", pluginapi.ExecutorHTTPRequest{}, "StatusCode", `200`},
		{"quota.identifier", map[string]any{}, "identifier", `"echo"`},
		{"quota.describe", pluginapi.QuotaDescribeRequest{}, "supported_providers", `["echo"]`},
		{"quota.fetch", pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo"}, "groups", ``},
		{"usage.handle", pluginapi.UsageRecord{}, "", ``},
	} {
		t.Run(tc.method, func(t *testing.T) {
			env := rpc(t, r, tc.method, tc.req)
			if !env.OK {
				t.Fatal(env.Error)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(env.Result, &fields); err != nil {
				t.Fatal(err)
			}
			if tc.key != "" {
				v, ok := fields[tc.key]
				if !ok || tc.want != "" && string(v) != tc.want {
					t.Fatalf("result %s", env.Result)
				}
			}
		})
	}
	if usage.n.Load() != 1 {
		t.Fatal("usage was not dispatched")
	}
	for _, method := range []string{"auth.login.start", "auth.login.poll", "quota.reset"} {
		if env := rpc(t, r, method, map[string]any{}); env.OK || env.Error.HTTPStatus != 400 {
			t.Fatalf("expected explicit unsupported operation: %+v", env)
		}
	}
}
func TestFailedReconfigureLeavesInactive(t *testing.T) {
	r := newRuntime(t, Definition{Metadata: metadata(), Configure: func(_ context.Context, b []byte) error {
		if string(b) == "bad" {
			return errors.New("private-config-error")
		}
		return nil
	}}, nil)
	register(t, r)
	if env := rpc(t, r, "plugin.reconfigure", map[string]any{"schema_version": 10, "config_yaml": []byte("bad")}); env.OK || env.Error.Message != "plugin operation failed" {
		t.Fatal(env)
	}
	if env := rpc(t, r, "auth.identifier", map[string]any{}); env.OK || env.Error.Code != "not_ready" {
		t.Fatal(env)
	}
	register(t, r)
	if env := rpc(t, r, "unrecognized", map[string]any{}); env.OK || env.Error.Code != "unknown_method" {
		t.Fatal(env)
	}
}
func TestShutdownClosesIdleAsyncStream(t *testing.T) {
	closed := make(chan struct{})
	var n atomic.Int32
	h := CallerFunc(func(_ context.Context, m string, b []byte) ([]byte, error) {
		if m == "host.stream.close" {
			if n.Add(1) != 1 {
				t.Error("double close")
			}
			close(closed)
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{Executor: &testExecutor{chunks: make(chan pluginapi.ExecutorStreamChunk)}, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}}}, h)
	register(t, r)
	if env := rpc(t, r, "executor.execute_stream", map[string]any{"stream_id": "s"}); !env.OK {
		t.Fatal(env.Error)
	}
	r.Shutdown(context.Background())
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("idle stream not closed by shutdown")
	}
}
