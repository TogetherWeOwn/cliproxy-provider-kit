# CLIProxy provider kit

Shared Go building blocks for first-class [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) provider plugins. This repository is a **library**, not an installable provider or a scheduler. The echo plugin is a loopback-only example for tests.

- SDK: `github.com/router-for-me/CLIProxyAPI/v8 v8.0.48`
- Native C ABI **1**, RPC schema **9**; Go **1.26.10+**, CGO and a C compiler
- Native libraries built with `-buildmode=c-shared`, **not** Go's `plugin` package
- Linux is the CI-tested target. Match the deployment architecture and libc; build provider release artifacts in `golang:1.26-bookworm` for a Debian-bookworm host. The exact host/plugin Go patch versions need not match across the C ABI.

## Provider repository usage

Add the kit at a reviewed commit (or a released version when available), and pin the SDK to v8.0.48. No kit release/tag is required to use a commit through Go's pseudo-version resolution.

```sh
go get github.com/TogetherWeOwn/cliproxy-provider-kit@<reviewed-commit>
go get github.com/router-for-me/CLIProxyAPI/v8@v8.0.48
```

Implement the SDK interfaces for your provider and pass them to `providerkit.New`. In the provider's `package main`, export a runtime factory **from `init`**. A c-shared library does not run `main`.

```go
package main

import (
    providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
    "github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
    "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func main() {}

func init() {
    if err := cabi.Export(func(host providerkit.Caller) (*providerkit.Runtime, error) {
        return providerkit.New(providerkit.Definition{
            Metadata: pluginapi.Metadata{
                Name: "example-provider", Version: "0.1.0",
                Author: "Example Organization",
                GitHubRepository: "https://github.com/example/provider",
            },
            Capabilities: pluginapi.Capabilities{
                AuthProvider: providerkit.APIKeyAuth{Provider: "example-provider"},
                // Add ModelRegistrar / ModelProvider, Executor, QuotaProvider,
                // and optionally UsagePlugin implemented by your provider.
            },
        }, host)
    }); err != nil {
        panic("cannot install provider factory")
    }
}
```

Use [`examples/echo/main.go`](examples/echo/main.go) for a complete executable example, including lifecycle configuration, static/per-auth models, auth headers, host HTTP execution and a cached quota provider. Its only permitted upstream is a literal loopback address supplied by the test harness. It has no production endpoint or real credentials.

### Public Go API

| API | Provider responsibility / behavior |
| --- | --- |
| `cabi.Export(cabi.Factory) error` | Install once during `main.init`; the factory receives the host callback boundary and constructs a fresh runtime per native initialization. |
| `New(Definition, Caller) (*Runtime, error)` | Supply validated SDK metadata, capabilities, and optional `Configure`, `OnQuiesce`, `OnShutdown` hooks. |
| `Runtime.Call(ctx, method, raw) []byte` / `Shutdown(ctx)` | RPC envelopes and logical shutdown; normally used by the C shim, not provider code. |
| `ValidateMetadata(pluginapi.Metadata) error` | Require nonempty Name/Version/Author/GitHubRepository and an HTTPS GitHub owner/repository URL. |
| `APIKeyAuth` | Stateless SDK `AuthProvider`; top-level type dispatch, validation, stable IDs, API-key-only login errors, no-op refresh. Optional `Validate` checks provider fields. |
| `AuthFields(storage, metadata)` / `APIKey(storage, metadata)` | Decode object storage with host-compatible metadata precedence; never include credential values in errors. |
| `Passthrough` | SDK executor using **only** the injected `HostHTTPClient`. Supply `BuildRequest`; optionally `PrepareHTTP`, `ClassifyError`, and `TokenCount`. |
| `MapError(error) *pluginabi.Error` | Preserve explicit code/status/retry fields; redact unclassified errors. |
| `RetryAfterSeconds(header, now) int` | Parse HTTP seconds/date retry guidance without a hidden clock. |
| `NewCachedQuota(provider, display, FetchQuotaFunc) *CachedQuota` | Non-resettable SDK `QuotaProvider`; successful per-auth cache for `QuotaCacheTTL` = 60 seconds, coalesced concurrent fetches, copy isolation. |
| `CachedQuota.Store(request, response) error` / `Invalidate(authID)` | Incorporate provider quota headers by auth identity, or invalidate header observations and all polled credential versions for an auth. |
| `NewQuotaHTTPClient(http.RoundTripper) *QuotaHTTPClient` | Read-only polling, 15-second total timeout, 1 MiB body cap, no redirects, redacted errors; `CloseIdleConnections` for teardown. The default transport honors `WithProxyURL`. |
| `AuthProxyURL(storage, metadata, hostProxyURL)` / `WithProxyURL(ctx, proxy)` / `QuotaProxy` | Resolve the auth's `proxy_url`, else the host's global `proxy-url`, attach it to a direct request context, or use it as a custom transport's `Proxy` func. |
| `NewHostHTTPClient(Caller, callbackID) pluginapi.HostHTTPClient` | Scoped callback adapter, normally injected by Runtime. Never retain it beyond its request/stream. |
| `Caller` / `CallerFunc` | Raw concurrent host callback boundary; useful for isolated contract tests. |

`Definition.Capabilities` supports auth, model registration, model discovery, executor, quota and usage. Other SDK capabilities are rejected rather than advertised without handlers. Executor definitions must declare input/output formats and may set scope `both`, `static`, or `oauth` (default `both`). Let the host translate protocols to those formats; the passthrough helper does not translate or rewrite business payloads after host payload rules.

### Auth files and persistence

One object per account, using the provider key as top-level `type`:

```json
{"type":"example-provider","api_key":"example-key","label":"Example","prefix":"example-provider","weight":2,"priority":0,"disabled":false}
```

Create real auth files privately with mode `0600`; never commit them. The default ID is `provider:` plus the first 12 hex characters of SHA-256 of the trimmed API key, or an explicit string `id`. Changing filename does not change identity; duplicate files with the same key derive the same identity. Account/file deduplication and rejection policy belong to provider repos, not this stateless parser.

Storage remains a JSON **object** with provider fields; host-owned id/label/prefix/proxy_url/disabled/weight/priority move to SDK metadata/attributes. The host merges metadata over storage and forces `type`. `APIKey` and `AuthFields` preserve that precedence on execute/refresh/quota calls. Wrong provider type returns `Handled:false`. Malformed objects, duplicate top-level fields, missing keys, or wrongly typed routing fields return an error without a half-auth.

### Executor, errors and streaming

`BuildRequest` must choose a trusted endpoint and add the provider's auth headers. Preserve the final `ExecutorRequest.Payload`; CLIProxy has already applied its payload policy. `PrepareHTTP` can inject auth and restrict the URL for SDK `HttpRequest`; without it that method forwards the supplied HTTP request unchanged. Do not copy client credentials blindly into upstream headers. The kit never constructs an independent executor transport.

`ExecuteStream` opens HTTP through `host.http.do_stream`, reads raw chunks incrementally, and returns headers before completion. Runtime forwards chunks asynchronously with `host.stream.emit` and exactly one `host.stream.close`. It does not buffer the entire stream. Upstream setup failures return a normal RPC error; late stream failures carry structured `error_detail`. Host operation IDs propagate cancellation to blocked HTTP setup/read calls. Providers must honor contexts, and consumers must drain a stream or cancel its context.

Classify provider errors explicitly, including exhaustion versus transient throttling, using **safe public text**:

```go
return &pluginabi.Error{
    Code: "quota_exhausted", Message: "subscription window exhausted",
    HTTPStatus: 429, Retryable: true, RetryAfterSeconds: 60,
}
```

These fields drive the host's native per-auth cooldown. A non-retryable provider error can still carry a status and cooldown hint. Do not relabel every 403 as bad credentials or every 429 as exhaustion. Never put raw upstream bodies, keys, URLs with credentials, or transport errors in public messages. Unclassified errors and recovered panic values are redacted by the kit. Panics in provider-created goroutines must be recovered by that provider; Go cannot recover another goroutine's panic.

Default `CountTokens` is an explicitly marked estimate (ceil of Unicode payload codepoints / 4), not a model tokenizer or an upstream call. Supply `TokenCount` for a real tokenizer or a protocol-specific result. No helper automatically retries inference, rotates accounts, or bypasses provider entitlements. Multiple auth files use CLIProxy's native selection and cooldown; the kit does not claim a global Scheduler capability.

### Quota polling and cache

`FetchQuotaFunc` fetches and parses the provider's official quota format, returning normalized SDK groups/buckets/summary/subscription. The kit does not invent plan limits, reset support, or dollars. Errors are not cached and stale observations are not silently returned. Polled results are keyed by provider, auth identity, and a hash of storage/metadata/attributes, preventing credential changes from reusing old **polled** quota. Header observations are a separate auth-identity tier, preferred while fresh. Both tiers expire after 60 seconds and are pruned on fetch; the cache is in-process and starts empty after reload.

Direct polls follow the host's proxy precedence. `CachedQuota` passes `FetchQuotaFunc` a context carrying the auth's `proxy_url` (else `Host.ProxyURL`), and `QuotaHTTPClient` sends the request through it: `http`, `https`, `socks5` or `socks5h`, with `direct`/`none` bypassing proxies and an empty setting inheriting the environment. An invalid setting fails the request instead of connecting directly. Other direct calls, such as per-auth model discovery, should wrap their context with `WithProxyURL(ctx, AuthProxyURL(...))`.

A provider `UsagePlugin` can normalize a quota response header and call `CachedQuota.Store` using only the identity from `pluginapi.UsageRecord`:

```go
err := cache.Store(pluginapi.QuotaFetchRequest{
    AuthID: record.AuthID, AuthIndex: record.AuthIndex, Provider: record.Provider,
}, normalizedQuota)
```

`Store` and `FetchQuota` use `AuthID`, falling back to `AuthIndex` only when `AuthID` is absent. Supply `AuthID` whenever available in the usage record so it matches the host-enriched fetch. `Store` does not need or key on storage/metadata/attributes: the host may supply physical auth-file bytes and additional metadata only on `FetchQuota`. An older in-flight poll cannot overwrite the stored header observation. Use one cache instance for both interfaces. Invalidate an identity when reusing it for a different credential (header observations cannot detect credential changes), and recreate/invalidate the cache when configuration changes endpoint or quota semantics.

**Transport distinction:** v8.0.48's `HostHTTPClient` follows redirects, without a per-request flag to prohibit them or cap reads. Therefore the strict polling helper is separate: `QuotaHTTPClient.Do` allows GET only, uses a 15-second total timeout, never follows redirects, caps successful bodies at 1 MiB, and discards error bodies/URL/transport text. Pass a configured `http.RoundTripper` when deployment proxy policy requires it; nil clones the standard transport (normal environment proxies). This helper is **not** for inference. If using the host client for quota instead, the host's redirect semantics apply and strict no-redirect polling is not guaranteed. Neither helper logs keys; host request logging remains controlled by the host operator.

### Lifecycle and ABI ownership

`plugin.register` / `plugin.reconfigure` accept schema 9 and return registration metadata/capability flags. Configure gets the plugin config subtree as YAML bytes, not the whole host config. Reconfiguration first cancels and drains active work; a failed Configure leaves the runtime inactive until a successful configuration. `plugin.quiesce` stops admission and drains work. `plugin.shutdown` and the native shutdown function perform idempotent logical shutdown. Do not call lifecycle methods recursively from a capability/hook.

Capabilities must be concurrency-safe, immutable after construction, and cancelable. Quiesce waits for admitted work; a provider ignoring cancellation can prevent draining. Shutdown cancels streams, but does not unload a resident Go runtime. A native reinitialization uses the factory again.

The shim exports `cliproxy_plugin_init`, `cliproxy_plugin_call`, `cliproxy_plugin_free_buffer`, and `cliproxy_plugin_shutdown`; the init function populates ABI 1's `call/free_buffer/shutdown` table. JSON is copied at the C boundary. Plugin-allocated response buffers are freed by the plugin allocator, host callback buffers by the host allocator, exactly once. No Go pointers cross the ABI.

## Build and verify

```sh
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
export CGO_ENABLED=1

go vet ./...
go test -race -count=1 ./...
go build -trimpath -buildmode=c-shared -o "$scratch/echo.so" ./examples/echo
# Separate module: root ./... deliberately does not include this harness.
go -C integration/hostcompat vet ./...
go -C integration/hostcompat test -race -count=1 -v ./...
```

CI runs formatting, module verification, both vet/race suites, a c-shared build, gitleaks, and a generic credential-pattern scan, without provider credentials or repository secrets. All network behavior under test uses `httptest` loopback upstreams; module/tool downloads use public Go infrastructure only. Tests never call provider inference or quota services.

### Why the host compatibility harness is a separate module

The real loader is unexported inside `internal/pluginhost`. The test-only module at [`integration/hostcompat`](integration/hostcompat) intentionally uses import path `github.com/router-for-me/CLIProxyAPI/v8/providerkitcompat` so Go permits imports of the upstream internal host/config/registry packages. This is an internal-package testing harness, **not** a fork, an upstream package, or a replacement loader. Its module requires exactly v8.0.48 with normal Go checksum verification; there is no local `replace` or patched core source.

The test builds the local echo c-shared library, then calls the pinned host's `Host.ApplyConfig`: this runs the original `dynamicLibraryLoader.Open` (`dlopen/dlsym`), `registerRPCPlugin`, HTTP callbacks, streaming bridge and executor/auth/quota adapters. Assertions cover metadata/capabilities, host-merged auth persistence, quota, mock execution, incremental streaming, retry-after mapping, reconfiguration, shutdown and resident-image reinitialization. The host test process is disposable; it is not a running proxy service and opens no public listener.

Source contracts pinned to [v8.0.48](https://github.com/router-for-me/CLIProxyAPI/tree/v8.0.48):

- `sdk/pluginabi/types.go`: ABI/schema constants, method names, envelope/error contract.
- `sdk/pluginapi/types.go`: auth/model/executor/host HTTP/quota/usage interfaces.
- `internal/pluginhost/loader_unix.go`: C layout, buffer allocation/freeing, logical unload.
- `internal/pluginhost/rpc_schema.go`, `rpc_client.go`: registration and scoped requests.
- `internal/pluginhost/host_callbacks.go`, `rpc_client_stream.go`, `stream_bridge.go`: host HTTP operations and stream emission.
- `internal/pluginhost/auth_provider.go`: storage/metadata merge and auth conversion.
- `internal/pluginhost/http_bridge.go`: redirect behavior and host request logging.

## Provider releases (not kit releases)

This library has no installable `.so`, `registry.json`, migration command, or plugin-store entry of its own. Provider repositories own those artifacts and credential migration. For the v8.0.48 GitHub store contract, release `<plugin-id>_<version>_linux_amd64.zip` with `<plugin-id>.so` and checksums, and publish a schema-1 `registry.json` with the provider's GitHub-release installation metadata. Verify the exact contract in `internal/pluginstore/github.go` before releasing a provider. Keep all credentials out of artifacts and fixtures. Stage each provider before any production cutover; native compatibility executors may take precedence over plugin executors.

The C ABI layout is adapted from CLIProxyAPI's MIT-licensed simple Go plugin example and loader. The original copyright notices are retained in [LICENSE](LICENSE), alongside this kit's MIT license.
