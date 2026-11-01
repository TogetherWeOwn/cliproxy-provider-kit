// Package providerkit implements the CLIProxy provider plugin RPC contract.
// Provider implementations use the pinned SDK's interfaces directly.
package providerkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Caller is the raw host callback boundary. Results are pluginabi.Envelope JSON.
// Implementations must support concurrent calls. Do not log request/response data.
type Caller interface {
	Call(context.Context, string, []byte) ([]byte, error)
}

// CallerFunc adapts a callback function to Caller.
type CallerFunc func(context.Context, string, []byte) ([]byte, error)

func (f CallerFunc) Call(ctx context.Context, m string, b []byte) ([]byte, error) {
	return f(ctx, m, b)
}

// Definition wires SDK capabilities and lifecycle hooks. Configure receives only
// this plugin's configuration subtree, on both registration and reconfiguration.
// Hooks run serially after active calls/streams are canceled and drained.
// Capabilities and metadata must not be mutated after New. Capability methods
// must be concurrency-safe and honor context cancellation.
//
// ModelNamespace is the plugin's fixed vendor namespace (a bare segment such
// as "synthetic"; the kit appends the slash). Any provider that serves models
// made by another vendor must namespace its model ids so they never clash
// when the official provider is added later. A definition that registers or
// serves models (ModelRegistrar or ModelProvider set) must declare it; an
// empty, missing or malformed namespace is a startup error. Every static,
// discovered and dynamic model list is filtered through it: ids outside
// "<namespace>/" (including doubled "<namespace>/<namespace>/…") are dropped
// with a logged error and never served bare. The host auth-file `prefix` is
// an outer host clone and does not replace this namespace.
type Definition struct {
	Metadata     pluginapi.Metadata
	Capabilities pluginapi.Capabilities
	// ModelNamespace declares the fixed vendor namespace for every model id
	// this plugin registers or serves.
	ModelNamespace string
	Configure      func(context.Context, []byte) error
	OnQuiesce      func(context.Context) error
	OnShutdown     func(context.Context) error
}

// Runtime owns registration, dispatch and the lifetime of asynchronous work.
type Runtime struct {
	def       Definition
	host      Caller
	lifecycle sync.Mutex
	mu        sync.Mutex
	active    bool
	closed    bool
	root      context.Context
	cancel    context.CancelFunc
	work      sync.WaitGroup
}

// ValidateMetadata checks the metadata required by the host and public registry.
func ValidateMetadata(m pluginapi.Metadata) error {
	if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Version) == "" || strings.TrimSpace(m.Author) == "" || strings.TrimSpace(m.GitHubRepository) == "" {
		return fmt.Errorf("metadata requires Name, Version, Author and GitHubRepository")
	}
	u, err := url.Parse(m.GitHubRepository)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("GitHubRepository must be an HTTPS GitHub repository URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("GitHubRepository must identify owner/repository")
	}
	return nil
}

// New validates a definition. The runtime starts inactive until plugin.register.
// Supported capabilities are auth, models, executor, quota and usage; unrelated
// SDK capabilities are rejected instead of silently advertised without dispatch.
func New(d Definition, h Caller) (*Runtime, error) {
	if err := ValidateMetadata(d.Metadata); err != nil {
		return nil, err
	}
	c := d.Capabilities
	if c.ModelRegistrar != nil || c.ModelProvider != nil {
		if err := ValidateModelNamespace(d.ModelNamespace); err != nil {
			return nil, fmt.Errorf("model namespace: %w", err)
		}
	} else if d.ModelNamespace != "" {
		if err := ValidateModelNamespace(d.ModelNamespace); err != nil {
			return nil, fmt.Errorf("model namespace: %w", err)
		}
	}
	if c.Executor != nil && (len(c.ExecutorInputFormats) == 0 || len(c.ExecutorOutputFormats) == 0) {
		return nil, fmt.Errorf("executor requires input and output formats")
	}
	if c.ExecutorModelScope != "" && c.ExecutorModelScope != pluginapi.ExecutorModelScopeBoth && c.ExecutorModelScope != pluginapi.ExecutorModelScopeStatic && c.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		return nil, fmt.Errorf("invalid executor model scope")
	}
	if c.FrontendAuthProvider != nil || c.Scheduler != nil || c.ModelRouter != nil || c.RequestTranslator != nil || c.RequestNormalizer != nil || c.ResponseTranslator != nil || c.ResponseBeforeTranslator != nil || c.ResponseAfterTranslator != nil || c.RequestInterceptor != nil || c.RequestBodyPatcherBefore != nil || c.RequestBodyPatcherAfter != nil || c.RequestLifecyclePlugin != nil || c.ResponseInterceptor != nil || c.StreamChunkInterceptor != nil || c.WebSocketResponseObserver != nil || c.ThinkingApplier != nil || c.CommandLinePlugin != nil || c.ManagementAPI != nil {
		return nil, fmt.Errorf("unsupported capability in provider definition")
	}
	// Schema 10 multi-auth entries are not dispatched: auth.identifier answers a
	// single identifier, so extra identifiers would be silently dropped.
	if _, multi := c.AuthProvider.(pluginapi.MultiAuthProvider); multi || len(c.AuthProviders) > 0 {
		return nil, fmt.Errorf("multiple auth providers are not supported; use a single AuthProvider")
	}
	root, cancel := context.WithCancel(context.Background())
	return &Runtime{def: d, host: h, root: root, cancel: cancel}, nil
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  wireCapabilities   `json:"capabilities"`
}
type wireCapabilities struct {
	Auth      bool                         `json:"auth_provider"`
	Models    bool                         `json:"model_provider"`
	Registrar bool                         `json:"model_registrar"`
	Executor  bool                         `json:"executor"`
	Scope     pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	Input     []string                     `json:"executor_input_formats,omitempty"`
	Output    []string                     `json:"executor_output_formats,omitempty"`
	Quota     bool                         `json:"quota_provider"`
	Usage     bool                         `json:"usage_plugin"`
}

func (r *Runtime) registration() registration {
	c := r.def.Capabilities
	scope := c.ExecutorModelScope
	if c.Executor != nil && scope == "" {
		scope = pluginapi.ExecutorModelScopeBoth
	}
	return registration{pluginabi.SchemaVersion, r.def.Metadata, wireCapabilities{c.AuthProvider != nil, c.ModelProvider != nil, c.ModelRegistrar != nil, c.Executor != nil, scope, c.ExecutorInputFormats, c.ExecutorOutputFormats, c.QuotaProvider != nil, c.UsagePlugin != nil}}
}

// Call always returns a valid RPC envelope. Panics on the synchronous dispatch
// path are contained and redacted. Provider-created goroutines must contain
// their own panics (Go cannot recover another goroutine's panic).
func (r *Runtime) Call(ctx context.Context, method string, raw []byte) (out []byte) {
	defer func() {
		if recover() != nil {
			out = envelope(nil, rpcError("plugin_panic", "plugin operation panicked", 500))
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return envelope(nil, rpcError("not_ready", "runtime unavailable", 503))
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure, pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		result, err := r.lifecycleCall(ctx, method, raw)
		return envelope(result, err)
	}
	ctx, finish, err := r.begin(ctx)
	if err != nil {
		return envelope(nil, err)
	}
	defer func() {
		if finish != nil {
			finish()
		}
	}()
	result, async, err := r.dispatch(ctx, method, raw, finish)
	if async {
		finish = nil
	}
	return envelope(result, err)
}
func (r *Runtime) begin(parent context.Context) (context.Context, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || r.closed {
		return nil, nil, rpcError("not_ready", "plugin is not active", 503)
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(r.root, cancel)
	r.work.Add(1)
	var once sync.Once
	return ctx, func() { once.Do(func() { stop(); cancel(); r.work.Done() }) }, nil
}
func (r *Runtime) drain() { r.mu.Lock(); r.active = false; r.cancel(); r.mu.Unlock(); r.work.Wait() }
func (r *Runtime) lifecycleCall(ctx context.Context, m string, raw []byte) (any, error) {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if m == pluginabi.MethodPluginShutdown {
		return struct{}{}, r.shutdownLocked(ctx)
	}
	if closed {
		return nil, rpcError("not_ready", "plugin has shut down", 503)
	}
	if m == pluginabi.MethodPluginQuiesce {
		r.drain()
		if r.def.OnQuiesce != nil {
			return struct{}{}, r.def.OnQuiesce(ctx)
		}
		return struct{}{}, nil
	}
	var req struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	// A newer host still accepts a plugin that answers with an older schema
	// (it gates newer features per plugin), so only an older host is refused.
	if req.SchemaVersion < pluginabi.SchemaVersion {
		return nil, rpcError("unsupported_schema", "this kit requires RPC schema 10 or newer", 400)
	}
	r.drain()
	if r.def.Configure != nil {
		if err := r.def.Configure(ctx, bytes.Clone(req.ConfigYAML)); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	r.root, r.cancel = context.WithCancel(context.Background())
	r.active = true
	r.mu.Unlock()
	return r.registration(), nil
}

// Shutdown cancels and drains work and runs OnShutdown once. Logical shutdown
// does not unload the c-shared Go runtime from the host address space.
func (r *Runtime) Shutdown(ctx context.Context) {
	if r == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	_ = r.shutdownLocked(ctx)
}
func (r *Runtime) shutdownLocked(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	r.drain()
	if r.def.OnShutdown != nil {
		return r.def.OnShutdown(ctx)
	}
	return nil
}
func decode(raw []byte, v any) error {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || b[0] != '{' || json.Unmarshal(b, v) != nil {
		return rpcError("invalid_request", "request must be a valid JSON object", 400)
	}
	return nil
}
func envelope(result any, err error) []byte {
	env := pluginabi.Envelope{OK: err == nil, Error: MapError(err)}
	if err == nil {
		raw, e := json.Marshal(result)
		if e != nil {
			return envelope(nil, e)
		}
		env.Result = raw
	}
	raw, e := json.Marshal(env)
	if e != nil {
		return []byte(`{"ok":false,"error":{"code":"encoding_error","message":"response encoding failed","http_status":500}}`)
	}
	return raw
}

func invoke[Q, R any](ctx context.Context, raw []byte, fn func(context.Context, Q) (R, error)) (any, error) {
	var q Q
	if err := decode(raw, &q); err != nil {
		return nil, err
	}
	return fn(ctx, q)
}
func invokeHTTP[Q, R any](ctx context.Context, raw []byte, h Caller, set func(*Q, pluginapi.HostHTTPClient), fn func(context.Context, Q) (R, error)) (any, error) {
	var q Q
	if err := decode(raw, &q); err != nil {
		return nil, err
	}
	var scope struct {
		ID string `json:"host_callback_id"`
	}
	_ = json.Unmarshal(raw, &scope)
	set(&q, NewHostHTTPClient(h, scope.ID))
	return fn(ctx, q)
}
func (r *Runtime) dispatch(ctx context.Context, m string, raw []byte, finish func()) (any, bool, error) {
	c := r.def.Capabilities
	var result any
	var err error
	switch m {
	case pluginabi.MethodAuthIdentifier:
		if c.AuthProvider != nil {
			return map[string]string{"identifier": c.AuthProvider.Identifier()}, false, nil
		}
	case pluginabi.MethodAuthParse:
		if c.AuthProvider != nil {
			result, err = invoke(ctx, raw, c.AuthProvider.ParseAuth)
			return result, false, err
		}
	case pluginabi.MethodAuthLoginStart:
		if c.AuthProvider != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.AuthLoginStartRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, c.AuthProvider.StartLogin)
			return result, false, err
		}
	case pluginabi.MethodAuthLoginPoll:
		if c.AuthProvider != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.AuthLoginPollRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, c.AuthProvider.PollLogin)
			return result, false, err
		}
	case pluginabi.MethodAuthRefresh:
		if c.AuthProvider != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.AuthRefreshRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, c.AuthProvider.RefreshAuth)
			return result, false, err
		}
	case pluginabi.MethodModelRegister:
		if c.ModelRegistrar != nil {
			result, err = invoke(ctx, raw, func(ctx context.Context, q pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
				resp, err := c.ModelRegistrar.RegisterModels(ctx, q)
				if err != nil {
					return resp, err
				}
				resp.Models = filterModels(r.def.ModelNamespace, resp.Models)
				for i := range resp.Registrations {
					resp.Registrations[i].Models = filterModels(r.def.ModelNamespace, resp.Registrations[i].Models)
				}
				return resp, nil
			})
			return result, false, err
		}
	case pluginabi.MethodModelStatic:
		if c.ModelProvider != nil {
			result, err = invoke(ctx, raw, func(ctx context.Context, q pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
				resp, err := c.ModelProvider.StaticModels(ctx, q)
				if err != nil {
					return resp, err
				}
				resp.Models = filterModels(r.def.ModelNamespace, resp.Models)
				for i := range resp.Registrations {
					resp.Registrations[i].Models = filterModels(r.def.ModelNamespace, resp.Registrations[i].Models)
				}
				return resp, nil
			})
			return result, false, err
		}
	case pluginabi.MethodModelForAuth:
		if c.ModelProvider != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.AuthModelRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, func(ctx context.Context, q pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
				resp, err := c.ModelProvider.ModelsForAuth(ctx, q)
				if err != nil {
					return resp, err
				}
				resp.Models = filterModels(r.def.ModelNamespace, resp.Models)
				for i := range resp.Registrations {
					resp.Registrations[i].Models = filterModels(r.def.ModelNamespace, resp.Registrations[i].Models)
				}
				return resp, nil
			})
			return result, false, err
		}
	case pluginabi.MethodExecutorIdentifier:
		if c.Executor != nil {
			return map[string]string{"identifier": c.Executor.Identifier()}, false, nil
		}
	case pluginabi.MethodExecutorExecute:
		if c.Executor != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.ExecutorRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, c.Executor.Execute)
			return result, false, err
		}
	case pluginabi.MethodExecutorCountTokens:
		if c.Executor != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.ExecutorRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, c.Executor.CountTokens)
			return result, false, err
		}
	case pluginabi.MethodExecutorHTTPRequest:
		if c.Executor != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.ExecutorHTTPRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, c.Executor.HttpRequest)
			return result, false, err
		}
	case pluginabi.MethodExecutorExecuteStream:
		if c.Executor != nil {
			var req struct {
				pluginapi.ExecutorRequest
				StreamID   string `json:"stream_id"`
				CallbackID string `json:"host_callback_id"`
			}
			if err = decode(raw, &req); err != nil {
				return nil, false, err
			}
			if r.host == nil || req.StreamID == "" {
				return nil, false, rpcError("invalid_request", "stream bridge and stream_id are required", 400)
			}
			req.HTTPClient = NewHostHTTPClient(r.host, req.CallbackID)
			resp, e := c.Executor.ExecuteStream(ctx, req.ExecutorRequest)
			if e != nil {
				return nil, false, e
			}
			if resp.Chunks == nil {
				return nil, false, rpcError("invalid_stream", "executor returned no stream channel", 500)
			}
			go r.pump(ctx, req.StreamID, resp.Chunks, finish)
			return struct {
				Headers http.Header `json:"headers,omitempty"`
			}{resp.Headers}, true, nil
		}
	case pluginabi.MethodQuotaIdentifier:
		if c.QuotaProvider != nil {
			return map[string]string{"identifier": c.QuotaProvider.Identifier()}, false, nil
		}
	case pluginabi.MethodQuotaDescribe:
		if c.QuotaProvider != nil {
			result, err = invoke(ctx, raw, c.QuotaProvider.DescribeQuota)
			return result, false, err
		}
	case pluginabi.MethodQuotaFetch:
		if c.QuotaProvider != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.QuotaFetchRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, c.QuotaProvider.FetchQuota)
			return result, false, err
		}
	case pluginabi.MethodQuotaReset:
		if c.QuotaProvider != nil {
			result, err = invokeHTTP(ctx, raw, r.host, func(q *pluginapi.QuotaResetRequest, h pluginapi.HostHTTPClient) { q.HTTPClient = h }, c.QuotaProvider.ResetQuota)
			return result, false, err
		}
	case pluginabi.MethodUsageHandle:
		if c.UsagePlugin != nil {
			var q pluginapi.UsageRecord
			if err = decode(raw, &q); err == nil {
				c.UsagePlugin.HandleUsage(ctx, q)
			}
			return struct{}{}, false, err
		}
	default:
		return nil, false, rpcError("unknown_method", "unknown RPC method", 404)
	}
	return nil, false, rpcError("unsupported_method", "capability is not enabled", 501)
}
func (r *Runtime) pump(ctx context.Context, id string, chunks <-chan pluginapi.ExecutorStreamChunk, finish func()) {
	var terminal error
	defer finish()
	defer func() {
		if recover() != nil {
			terminal = rpcError("plugin_panic", "stream operation panicked", 500)
		}
		_ = hostCall(context.Background(), r.host, pluginabi.MethodHostStreamClose, struct {
			ID    string           `json:"stream_id"`
			Error *pluginabi.Error `json:"error_detail,omitempty"`
		}{id, MapError(terminal)}, nil)
	}()
	for {
		select {
		case <-ctx.Done():
			terminal = ctx.Err()
			return
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			if chunk.Err != nil {
				terminal = chunk.Err
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			terminal = hostCall(ctx, r.host, pluginabi.MethodHostStreamEmit, struct {
				ID      string `json:"stream_id"`
				Payload []byte `json:"payload"`
			}{id, chunk.Payload}, nil)
			if terminal != nil {
				return
			}
		}
	}
}
