# CLIProxy provider kit

Shared Go building blocks for first-class [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) provider plugins. This repository is a **library**, not an installable provider or a scheduler. The echo plugin is a loopback-only example for tests.

- SDK: `github.com/router-for-me/CLIProxyAPI/v8 v8.0.49`
- Native C ABI **1**, RPC schema **10**. Minimum host: CLIProxyAPI **v8.0.49** (schema 10); newer hosts are accepted, older hosts (schema 9 or below, v8.0.48 and earlier) are refused. Go **1.26.10+**, CGO and a C compiler
- Native libraries built with `-buildmode=c-shared`, **not** Go's `plugin` package
- Linux is the CI-tested target. Match the deployment architecture and libc; build provider release artifacts in `golang:1.26-bookworm` for a Debian-bookworm host. The exact host/plugin Go patch versions need not match across the C ABI.

## Provider repository usage

Add the kit at a reviewed commit (or a released version when available), and pin the SDK to v8.0.49. No kit release/tag is required to use a commit through Go's pseudo-version resolution.

```sh
go get github.com/TogetherWeOwn/cliproxy-provider-kit@<reviewed-commit>
go get github.com/router-for-me/CLIProxyAPI/v8@v8.0.49
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
                // API-key providers must omit AuthProvider: there is no login
                // flow (StartLogin/PollLogin fail with api_key_only), so a
                // registered provider would advertise a login card that can
                // only fail. Auth files load through the host's generic file
                // path instead; it does not call APIKeyAuth.ParseAuth or
                // Validate. Check provider-specific fields in the executor.
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

### Provider plugin standards

Owner rule (2026-11-01): any provider that serves models made by another
vendor **must namespace its model ids**, so they never clash with the official
provider's ids when it is added later. This is enforced by the kit, not left
as a convention:

- Declare the fixed namespace once in the `Definition`, as a bare segment:
  `ModelNamespace: "synthetic"`. A definition that registers or serves models
  (`ModelRegistrar` or `ModelProvider` set) with an empty, missing or
  malformed namespace fails in `New` — a startup error, never a silent
  fallback.
- The kit refuses to register any model whose id is not namespaced **exactly
  once** (`<namespace>/…`). That covers static, discovered and dynamic lists
  alike, including multi-provider `Registrations`: the offending model is
  dropped with a logged error and never served bare. Bare ids, foreign
  namespaces and doubled namespaces (`nvidia/nvidia/…`, usually a plugin
  prefixing an already-prefixed upstream id) are dropped; a second layer past
  the namespace (`cloudflare/@cf/…`) passes.
- The executor keeps stripping exactly one leading `<namespace>/` before the
  upstream request (plugin-side, as today). The host auth-file `prefix` is an
  outer host clone and does **not** replace the fixed model namespace.
- Prove it in the plugin's own test suite with the reusable conformance check,
  called with every id the plugin can emit (static fallback, discovery
  fixtures and dynamic refresh paths):

```go
providerkit.AssertModelNamespace(t, "synthetic", models)
```

`CheckModelNamespace` returns the same verdict as an error for harnesses
without `testing.T`. `ValidateModelNamespace` checks a bare segment on its
own.

### Public Go API

| API | Provider responsibility / behavior |
| --- | --- |
| `cabi.Export(cabi.Factory) error` | Install once during `main.init`; the factory receives the host callback boundary and constructs a fresh runtime per native initialization. |
| `New(Definition, Caller) (*Runtime, error)` | Supply validated SDK metadata, capabilities, and optional `Configure`, `OnQuiesce`, `OnShutdown` hooks. |
| `Runtime.Call(ctx, method, raw) []byte` / `Shutdown(ctx)` | RPC envelopes and logical shutdown; normally used by the C shim, not provider code. |
| `ValidateMetadata(pluginapi.Metadata) error` | Require nonempty Name/Version/Author/GitHubRepository and an HTTPS GitHub owner/repository URL. |
| `ModelNamespace` (`Definition` field) + `ValidateModelNamespace(string) error` | Fixed vendor namespace for every registered/served model id; required when model capabilities are set, enforced on all three model paths. |
| `CheckModelNamespace(ns, models) error` / `AssertModelNamespace(t, ns, models)` | Reusable conformance check for plugin test suites: every emitted id must carry `ns` exactly once. |
| `APIKeyAuth` | Stateless SDK `AuthProvider`; top-level type dispatch, validation, stable IDs, API-key-only login errors, no-op refresh. Optional `Validate` checks provider fields only when `ParseAuth` is actually called; the generic file path calls neither method, so validate provider-specific fields in the executor. There is no login flow: `StartLogin`/`PollLogin` always fail with `api_key_only`. API-key providers must omit `AuthProvider` from their capabilities (no login card is shown) and let auth files load through the host's generic file path; registering `APIKeyAuth` would advertise a login card that can only fail. |
| `KeyOwnership` / `ProvisionalParseExpiry(ctx)` | Shared filename-ownership duplicate guard keyed by a caller-computed credential-identity digest (key alone, or a composite such as account plus key when keys are shared across accounts). Host parses of real auth files commit ownership. The provisional marker (`withProvisionalParse`) is internal: providers on the generic file path have no pre-save validation flow, so an abandoned validation cannot hold a key. Hand-rolled guards consult `ProvisionalParseExpiry` for the same commit/reserve split. |
| `PrefixClaims.Claim(file, prefix, credential)` / `Release(file)` | Opt-in provider-owned collision tracker, not automatic host enforcement. Claim immediately replaces that file's previous claim; serialize it with committed auth changes, not speculative parses that the host may discard. Release only when the old auth is no longer active. |
| `NormalizePrefix(prefix) (string, error)` | Canonicalize a direct caller's prefix before both Claim and returning the auth to the host; Claim's internal comparison normalization cannot rewrite the caller's auth. `APIKeyAuth.ParseAuth` already returns this canonical prefix. |
| `AuthFields(storage, metadata)` / `APIKey(storage, metadata)` | Decode object storage with host-compatible metadata precedence; never include credential values in errors. |
| `Passthrough` | SDK executor using **only** the injected `HostHTTPClient`. Supply `BuildRequest`; optionally `PrepareHTTP`, `ClassifyError`, and `TokenCount`. `ExecuteStream` re-frames SSE upstreams (`text/event-stream`, or a missing Content-Type) into one host chunk per data event: bare JSON for OpenAI chat and same-protocol Gemini clients, `data:` lines for translators, complete named events and standalone comment lines for native Claude/Responses clients. Other Content-Types pass through unchanged; callers must not frame SSE again. |
| `MapError(error) *pluginabi.Error` | Preserve explicit code/status/retry fields; redact unclassified errors. |
| `RetryAfterSeconds(header, now) int` | Parse HTTP seconds/date retry guidance without a hidden clock. |
| `NewCachedQuota(provider, display, FetchQuotaFunc) *CachedQuota` / `NewCachedQuotaWithTTL(provider, display, FetchQuotaFunc, ttl) *CachedQuota` | Non-resettable SDK `QuotaProvider`; successful per-auth cache for `QuotaCacheTTL` = 60 seconds by default, or a per-plugin TTL with `NewCachedQuotaWithTTL` (non-positive falls back to the default), coalesced concurrent fetches, copy isolation. |
| `CachedQuota.Store(request, response) error` / `Invalidate(authID)` | Incorporate provider quota headers by auth identity, or invalidate header observations and all polled credential versions for an auth. |
| `NewModelCatalog(config ModelCatalogConfig) *ModelCatalog` / `NewModelCatalogWithTTL(config ModelCatalogConfig, ttl time.Duration) *ModelCatalog` / `OpenAIModelsFetcher(baseURL, path string, headers func(pluginapi.AuthModelRequest) http.Header) FetchModelsFunc` | Per-auth model discovery for `ModelProvider` (+ `ModelRegistrar`): foreground-only fetch through `req.HTTPClient` on every `ModelsForAuth` (a TTL never skips a fetch — it is a compatibility field, not a gate), single-flight for concurrent calls only, durable last-good fallback before static (an empty upstream is a failure, a filtered-empty is served), uniform glob/override/namespace pipeline with contributor precedence/dedup and executor-mappable ids, and `ModelCatalog.Invalidate(authID)` for auth lifecycle. Derived 15-second fetch deadline (`ModelsFetchTimeout`); 1 MiB post-return parse cap (`MaxModelsBody`), not a transport bound — redirects and buffering remain host-owned. |
| `NewQuotaHTTPClient(http.RoundTripper) *QuotaHTTPClient` | Read-only polling, 15-second total timeout, 1 MiB body cap, no redirects, redacted errors; `CloseIdleConnections` for teardown. The default transport, `NewQuotaTransport()`, honors `WithProxyURL` and speaks HTTP/1.1 so pooled connections never cross proxies. |
| `AuthProxyURL(storage, metadata, hostProxyURL)` / `WithProxyURL(ctx, proxy)` / `QuotaProxy` | Resolve the auth's `proxy_url`, else the host's global `proxy-url`, attach it to a direct request context. `QuotaProxy` is the `Proxy` func behind `NewQuotaTransport`; build custom transports from `NewQuotaTransport()` rather than setting `QuotaProxy` alone, because a transport shared across proxies must not reuse HTTP/2 connections. |
| `NewHostHTTPClient(Caller, callbackID) pluginapi.HostHTTPClient` | Scoped callback adapter, normally injected by Runtime. Never retain it beyond its request/stream. |
| `Caller` / `CallerFunc` | Raw concurrent host callback boundary; useful for isolated contract tests. |

`Definition.Capabilities` supports auth, model registration, model discovery, executor, quota, usage and management. A `ManagementAPI` must also implement `ManagementHandler` to dispatch RPC route calls. Registration serializes route metadata, not process-local handlers; the host installs its own RPC handler for each declared management route and resource. Other SDK capabilities are rejected rather than advertised without handlers, including the schema 10 multi-auth forms (`Capabilities.AuthProviders`, or an `AuthProvider` implementing `MultiAuthProvider`): the kit exposes one auth identifier per plugin. Executor definitions must declare input/output formats and may set scope `both`, `static`, or `oauth` (default `both`). Let the host translate protocols to those formats; the passthrough helper does not translate or rewrite business payloads after host payload rules.

A registered `AuthProvider` may additionally implement `CredentialProviders() []string` to advertise credential-provider ownership through the `credential_providers` field of `auth.identifier`. The kit forwards those values verbatim; the pinned host interprets and normalizes them. These are ownership claims, not extra login identifiers or a guarantee that the host will install or select the plugin. Providers must claim only credential types they are intended to own. Providers without the extension still return only `identifier`; definitions without an `AuthProvider` remain without that capability.

### Auth files and persistence

One object per account, using the provider key as top-level `type`:

```json
{"type":"example-provider","api_key":"example-key","label":"Example","prefix":"example-provider","weight":2,"priority":0,"disabled":false}
```

Create real auth files privately with mode `0600`; never commit them. The default ID is `provider:` plus the first 12 hex characters of SHA-256 of the trimmed API key, or an explicit string `id`. Changing filename does not change identity; duplicate files with the same key derive the same identity. Account/file deduplication and rejection policy belong to provider repos, not this stateless parser.

Storage remains a JSON **object** with provider fields; host-owned id/label/prefix/proxy_url/disabled/weight/priority move to SDK metadata/attributes. The host merges metadata over storage and forces `type`. `APIKey` and `AuthFields` preserve that precedence on execute/refresh/quota calls. The `prefix` is normalized exactly like the host filestore: surrounding whitespace and `/` are trimmed, and a `prefix` still containing `/` is rejected with `invalid_auth` — the plugin path pins whatever `ParseAuth` returns verbatim, so a slashed prefix could otherwise merge two pinning namespaces while looking unpinned. Wrong provider type returns `Handled:false`. Malformed objects, duplicate top-level fields, missing keys, or wrongly typed routing fields return an error without a half-auth.

### Executor, errors and streaming

`BuildRequest` must choose a trusted endpoint and add the provider's auth headers. Preserve the final `ExecutorRequest.Payload`; CLIProxy has already applied its payload policy. `PrepareHTTP` can inject auth and restrict the URL for SDK `HttpRequest`; without it that method forwards the supplied HTTP request unchanged. Do not copy client credentials blindly into upstream headers. The kit never constructs an independent executor transport.

`ExecuteStream` opens HTTP through `host.http.do_stream`, reads raw chunks incrementally, and returns headers before completion. Runtime forwards chunks asynchronously with `host.stream.emit` and exactly one `host.stream.close`. It does not buffer the entire stream. Upstream setup failures return a normal RPC error; late stream failures carry structured `error_detail`. Host operation IDs propagate cancellation to blocked HTTP setup/read calls. Providers must honor contexts, and consumers must drain a stream or cancel its context.

`ExecuteStream` normalizes SSE (`text/event-stream`, matched as a media type with optional parameters) into one host chunk per event. It ignores one stream-leading UTF-8 BOM and splits LF/CRLF/CR events across arbitrary HTTP reads. OpenAI output ends at `[DONE]` and cancels the upstream operation without waiting for HTTP EOF; the sentinel and any later bytes are not forwarded because the host emits its own terminator. OpenAI chat/completions and same-protocol Gemini clients receive bare JSON: their host HTTP writers add the SSE envelope (Gemini default, `alt=sse` and `$alt=sse`) or write raw bytes (other Gemini alt modes). Explicit non-SSE bodies such as Gemini `alt=json` pass through unchanged. Translators receive one `data:` line per data event, without comments or metadata. Native Claude/Responses clients receive complete SSE events and standalone comment lines, preserving original fields, order, data-line whitespace, comments and metadata-only events (including `id:` and `retry:`), with line endings normalized to LF. Native multiline data need not be JSON; downstream clients interpret SSE fields and reconnection metadata. The host records the client route in `Metadata["request_path"]`; `Format` and `SourceFormat` both describe the plugin protocol, not the client. Direct SDK callers without a route hint receive `data:` lines for OpenAI output and complete events for other output; supply the route hint for cross-protocol non-OpenAI calls.

Upstream comments are preserved only on native SSE routes. A completed standalone comment line is forwarded immediately, without waiting for a blank line, later data or EOF. Comments following pending data/metadata fields stay buffered with those fields to preserve source order and truncated-event semantics. Comments cannot keep a translated, OpenAI bare-JSON or Gemini bare-JSON route alive: the pinned host consumes data chunks there, not raw SSE comments. Such deployments need the host's `streaming.keepalive-seconds` set above zero for downstream heartbeats (and `streaming.send-keepalive-before-ft` for pre-first-token heartbeats). Both are disabled by default; the kit does not override server configuration or guarantee idle-connection liveness.

A missing Content-Type retains the SSE assumption. Headerless bare JSON/NDJSON is unsupported and returns `invalid_stream` rather than silently dropping records. Explicit SSE follows SSE field semantics instead: JSON-looking unknown fields (including colonless `true` or `42`) are ignored by data parsing and preserved on native routes. Other explicit Content-Types are forwarded unchanged; the kit does not split their records or promise that arbitrary raw reads meet every host translator's chunk contract. Pass-through plugins must not frame SSE again. A truncated event cut short by an upstream error is dropped while the error propagates. Native events without a blank-line terminator are also discarded at clean EOF, except completed standalone comment lines already delivered; translator/bare-JSON output retains its historical EOF flush. A colonless `data` field is an empty data line. The 8 MiB event budget counts data values plus one separator per data line, excluding `data:` prefixes, identically for complete and split reads. Native events charge all other field/comment bytes plus their separators to that same cumulative budget, including metadata-only events and already-forwarded comments until the actual blank-line delimiter; retaining repeated fields is bounded, not an unbounded metadata buffer. Field prefixes add at most six bytes per data line to native output. A separate line bound also caps comments and other fields. Oversized events, or non-native multiline data that is not one JSON value, return `invalid_stream` and release the upstream feed. Partial-line buffering scans only new bytes and reuses its allocation.

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

`FetchQuotaFunc` fetches and parses the provider's official quota format, returning normalized SDK groups/buckets/summary/subscription. The kit does not invent plan limits, reset support, or dollars. Errors are not cached and stale observations are not silently returned. Polled results are keyed by provider, auth identity, and a hash of storage/metadata/attributes, preventing credential changes from reusing old **polled** quota. Header observations are a separate auth-identity tier, preferred while fresh. When the caller includes the credential context (storage/metadata/attributes) in the `Store` request, the observation is bound to that credential generation and is never served to a fetch carrying a different credential, which closes the stale-observation window when a key is rotated under a reused identity. Observations stored without credential context (for example from usage records, which carry none) stay unbound and serve every generation of the identity until the TTL expires. Both tiers expire after the cache TTL (60 seconds by default, configurable per plugin with `NewCachedQuotaWithTTL`) and are pruned on fetch; the cache is in-process and starts empty after reload.

Direct polls follow the host's proxy precedence. `CachedQuota` passes `FetchQuotaFunc` a context carrying the auth's `proxy_url` (else `Host.ProxyURL`), and `QuotaHTTPClient` sends the request through it (with its default transport, or a custom one built from `NewQuotaTransport()`): `http`, `https`, `socks5` or `socks5h`, with `direct`/`none` bypassing proxies and an empty setting inheriting the environment. An invalid setting fails the request instead of connecting directly. Other direct calls, such as per-auth model discovery, should wrap their context with `WithProxyURL(ctx, AuthProxyURL(...))`.

A provider `UsagePlugin` can normalize a quota response header and call `CachedQuota.Store` using only the identity from `pluginapi.UsageRecord`:

```go
err := cache.Store(pluginapi.QuotaFetchRequest{
    AuthID: record.AuthID, AuthIndex: record.AuthIndex, Provider: record.Provider,
}, normalizedQuota)
```

`Store` and `FetchQuota` use `AuthID`, falling back to `AuthIndex` only when `AuthID` is absent. Supply `AuthID` whenever available in the usage record so it matches the host-enriched fetch. `Store` accepts storage/metadata/attributes and binds the observation to that credential generation when present, but never requires them: usage records carry auth identity without credential context, and store unbound observations served to every generation until the TTL expires. Binding compares the complete storage/metadata/attributes context, not only `api_key`: metadata can override storage credentials, and provider callbacks may use other fields. A partial context stored before host enrichment intentionally falls back to polling (or an existing matching poll), even if only a label or path changed. Supply the same complete context to both calls to retain the bound-observation fast path; partial context is never downgraded to unbound. An older in-flight poll cannot overwrite the stored header observation. Use one cache instance for both interfaces. Invalidate an identity when reusing it for a different credential without binding context, and recreate/invalidate the cache when configuration changes endpoint or quota semantics.

**Transport distinction:** v8.0.49's `HostHTTPClient` follows redirects, without a per-request flag to prohibit them or cap reads. Therefore, the strict polling helper is separate: `QuotaHTTPClient.Do` allows GET and POST (quota endpoints that mint or refresh a read-only view, such as GraphQL or key-envelope reads, use POST without becoming an inference or mutation path), uses a 15-second total timeout, never follows redirects, caps successful bodies at 1 MiB, and discards error bodies/URL/transport text. Nil uses `NewQuotaTransport()`: a clone of the standard transport that applies the `WithProxyURL` setting (else normal environment proxies) and speaks HTTP/1.1 only. Pass a configured `http.RoundTripper` when deployment proxy policy requires more; it must not follow redirects itself. The client passes it the request and context unchanged, so it honors the `WithProxyURL` setting only if it routes through `QuotaProxy` (as `NewQuotaTransport()` does); otherwise its own proxy behavior applies. An invalid `WithProxyURL` setting still fails the request with any transport. This helper is **not** for inference. If using the host client for quota instead, the host's redirect semantics apply and strict no-redirect polling is not guaranteed. Neither helper logs keys; host request logging remains controlled by the host operator.

### Lifecycle and ABI ownership

`plugin.register` / `plugin.reconfigure` accept a host schema of 10 or newer (an older host is refused with `unsupported_schema`), answer with schema 10 so the host gates newer features, and return registration metadata/capability flags. Configure gets the plugin config subtree as YAML bytes, not the whole host config. Reconfiguration first cancels and drains active work; a failed Configure leaves the runtime inactive until a successful configuration. `plugin.quiesce` stops admission and drains work. `plugin.shutdown` and the native shutdown function perform idempotent logical shutdown. Do not call lifecycle methods recursively from a capability/hook.

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

The real loader is unexported inside `internal/pluginhost`. The test-only module at [`integration/hostcompat`](integration/hostcompat) intentionally uses import path `github.com/router-for-me/CLIProxyAPI/v8/providerkitcompat` so Go permits imports of the upstream internal host/config/registry packages. This is an internal-package testing harness, **not** a fork, an upstream package, or a replacement loader. Its module requires exactly v8.0.49 with normal Go checksum verification; there is no local `replace` or patched core source.

`TestRealHostLoader` builds the local echo c-shared library, then calls the pinned host's `Host.ApplyConfig`: this runs the original `dynamicLibraryLoader.Open` (`dlopen/dlsym`), `registerRPCPlugin`, HTTP callbacks, streaming bridge and executor/auth/quota adapters. Assertions cover metadata/capabilities, host-merged auth persistence, quota, mock execution, incremental streaming, retry-after mapping, reconfiguration, shutdown and resident-image reinitialization. The host test process is disposable; it is not a running proxy service and opens no public listener.

A second test, `TestNativeABIContract`, compiles [`testdata/abi_check.c`](integration/hostcompat/testdata/abi_check.c) with the system `cc` against [`cabi/abi.h`](cabi/abi.h) and runs it in its own process. It checks that `cliproxy_plugin_init` rejects missing tables, a wrong ABI version, a missing host `free_buffer` and a second initialization; that invalid native call arguments return `invalid_request`; that `plugin.register` works with schema 10 and `plugin.quiesce` stops admission; that shutdown is idempotent; and that the resident image can be initialized again.

Source contracts pinned to [v8.0.49](https://github.com/router-for-me/CLIProxyAPI/tree/v8.0.49):

- `sdk/pluginabi/types.go`: ABI/schema constants, method names, envelope/error contract.
- `sdk/pluginapi/types.go`: auth/model/executor/host HTTP/quota/usage interfaces.
- `internal/pluginhost/loader_unix.go`: C layout, buffer allocation/freeing, logical unload.
- `internal/pluginhost/rpc_schema.go`, `rpc_client.go`: registration and scoped requests.
- `internal/pluginhost/host_callbacks.go`, `rpc_client_stream.go`, `stream_bridge.go`: host HTTP operations and stream emission.
- `internal/pluginhost/auth_provider.go`: storage/metadata merge and auth conversion.
- `internal/pluginhost/http_bridge.go`: redirect behavior and host request logging.

## Provider releases (not kit releases)

This library has no installable `.so`, `registry.json`, migration command, or plugin-store entry of its own. Provider repositories own those artifacts and credential migration. For the v8.0.49 GitHub store contract, release `<plugin-id>_<version>_linux_amd64.zip` with `<plugin-id>.so` and checksums, and publish a schema-1 `registry.json` with the provider's GitHub-release installation metadata. Verify the exact contract in `internal/pluginstore/github.go` before releasing a provider. Keep all credentials out of artifacts and fixtures. Stage each provider before any production cutover; native compatibility executors may take precedence over plugin executors.

The C ABI layout is adapted from CLIProxyAPI's MIT-licensed simple Go plugin example and loader. The original copyright notices are retained in [LICENSE](LICENSE), alongside this kit's MIT license.
