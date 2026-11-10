package providerkit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ModelsFetchTimeout is the kit-owned deadline for one model-discovery
// fetch. ModelsForAuth derives context.WithTimeout(reqCtx,
// ModelsFetchTimeout) explicitly; it never assumes a deadline on the
// incoming callback. When the parent already carries an earlier deadline,
// the derived context yields the earlier of the two.
const ModelsFetchTimeout = 15 * time.Second

// MaxModelsBody bounds a discovery response parse (default 1 MiB). The
// check runs against the buffered HTTPResponse.Body length after the host
// client returns: an oversized buffered body is treated as a failed fetch
// (last-good/static fallback) and is not parsed. This is a post-return
// parse cap, not a transport memory bound: the host may already have
// buffered an arbitrarily large body in memory before the kit sees it.
// Redirects and hop policy remain host-owned; the kit cannot disable them
// through HostHTTPClient and does not bypass host proxy/logging policy to
// try.
const MaxModelsBody = 1 << 20

// maxLastGoodGenerations bounds the durable last-good store. Entries are
// evicted oldest-first once the store is full; TTL never evicts them.
const maxLastGoodGenerations = 256

// FetchModelsFunc returns candidate models for one auth. Provider-owned
// protocol parsing stays outside the kit: custom Fetch implementations that
// use req.HTTPClient directly are responsible for their own post-return
// parse-size checks (see MaxModelsBody) and must honor context
// cancellation. Fetch runs foreground-only inside ModelsForAuth with the
// request-scoped host client; the kit starts no goroutine and retains no
// client past the callback.
type FetchModelsFunc func(ctx context.Context, req pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error)

// OpenAIModelsFetcher fetches an OpenAI-style {"data":[{"id":…}]} list
// through the host HTTP client (foreground use inside ModelsForAuth,
// proxy/logging host-owned). baseURL plus path form the request URL (a
// single slash joins the parts; an empty path requests baseURL alone).
// headers, when non-nil, supplies per-request headers such as the auth
// bearer derived from the request; the kit never derives credentials
// itself. A non-2xx status, a malformed body, or a body larger than
// MaxModelsBody is a fetch error (never parsed, never cached); the
// catalog falls back to last-good/static per the uniform pipeline.
func OpenAIModelsFetcher(baseURL, path string, headers func(pluginapi.AuthModelRequest) http.Header) FetchModelsFunc {
	return func(ctx context.Context, req pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
		if req.HTTPClient == nil {
			return nil, rpcError("models_fetch_failed", "model discovery client is unavailable", 502)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		url := strings.TrimSuffix(baseURL, "/")
		if path != "" {
			url += "/" + strings.TrimPrefix(path, "/")
		}
		hdr := make(http.Header)
		if headers != nil {
			for k, vs := range headers(req) {
				for _, v := range vs {
					hdr.Add(k, v)
				}
			}
		}
		if hdr.Get("Accept") == "" {
			hdr.Set("Accept", "application/json")
		}
		resp, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: url, Headers: hdr})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, rpcError("models_fetch_failed", "model discovery fetch failed", 502)
		}
		if len(resp.Body) > MaxModelsBody {
			return nil, rpcError("models_fetch_failed", "model discovery response exceeds the parse cap", 502)
		}
		var wire struct {
			Data []struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				Created int64  `json:"created"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp.Body, &wire); err != nil {
			return nil, rpcError("models_fetch_failed", "model discovery response parse failed", 502)
		}
		var out []pluginapi.ModelInfo
		for _, item := range wire.Data {
			if item.ID == "" {
				continue
			}
			out = append(out, pluginapi.ModelInfo{ID: item.ID, Object: item.Object, Created: item.Created, OwnedBy: item.OwnedBy})
		}
		return out, nil
	}
}

// ModelOverride tunes one upstream model. Zero/empty fields keep the
// discovered value; it never deletes a model. ContributorID, when
// non-empty, makes the emitted id that value (owner rule: the contributor
// id is preferred whenever a model has one). Overrides key by upstream
// (pre-namespace) id.
type ModelOverride struct {
	DisplayName         string
	ContextLength       int64
	MaxCompletionTokens int64
	ContributorID       string
	Thinking            *pluginapi.ThinkingSupport
}

// ModelCatalogConfig configures a ModelCatalog. Do not mutate the fields
// after NewModelCatalog; copy isolation covers the static list, overrides
// and globs taken at construction.
type ModelCatalogConfig struct {
	// Namespace is the kit vendor namespace; upstream ids are prefixed
	// with it before serving (the same rule the runtime enforces with
	// filterModels, so it must match Definition.ModelNamespace). An empty
	// namespace serves ids unchanged.
	Namespace string
	// Static is the fallback served through the same glob/override/
	// namespace pipeline as fresh and last-good models, and the list
	// served by RegisterModels and StaticModels.
	Static []pluginapi.ModelInfo
	// Fetch fetches fresh candidates per auth. Nil means static-only: no
	// fetch is attempted and ModelsForAuth serves the static list.
	Fetch FetchModelsFunc
	// TTL is a compatibility field. It does NOT gate ModelsForAuth
	// fetches: every invocation attempts a fresh fetch, so a large TTL
	// cannot lose a one-shot catalog-change callback. It bounds the
	// freshness-marker store for diagnostics only and never erases the
	// durable last-good store.
	TTL time.Duration
	// Overrides keys by upstream (pre-namespace) id.
	Overrides map[string]ModelOverride
	// Allow and Deny are path globs matched against the upstream id;
	// deny wins and an empty allowlist allows everything else. An invalid
	// pattern simply never matches.
	Allow, Deny []string
	// Validate is the post-fetch hook (glob and overrides applied, not
	// yet namespace-prefixed); e.g. probe validation that stays
	// plugin-side. It runs only on the fresh path: last-good and static
	// results were already validated or are trusted as configured. An
	// error is a failed fetch and never replaces last-good. Its
	// returned ids preserve the upstream glob decision: ids the
	// pipeline already approved survive (including ContributorID
	// rewrites whose source was allowed), deny always wins, and ids
	// Validate invented face the full allow/deny check, so Validate
	// cannot bypass the glob policy.
	Validate func(ctx context.Context, req pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error)
}

// ModelCatalog implements pluginapi.ModelProvider and
// pluginapi.ModelRegistrar: RegisterModels and StaticModels serve Static
// through the uniform pipeline, and ModelsForAuth runs fetch → glob →
// overrides → Validate hook → cache → serve with req.HTTPClient.
//
// Every ModelsForAuth invocation attempts a fresh fetch (no TTL skip);
// only concurrent invocations sharing one key coalesce through
// single-flight. A waiter whose shared leader failed only because the
// leader's own context was cancelled retries as a new leader rather than
// inheriting another caller's cancellation. Results fall back fresh →
// durable last-good → static, so a transient fetch error never drops a
// known model: an empty upstream result (before glob filtering) is a
// failed fetch, while a filtered-empty result is a legitimate policy
// outcome that is served and cached. A refresh whose candidates are all
// rejected by the namespace filter is likewise a failed fetch, so an
// incompatible upstream payload never erases last-good. Errors are never
// cached and a failed fetch, failed Validate or oversized body never
// replaces last-good. The last-good store has no TTL eviction and
// persists until a newer successful fetch replaces it or Invalidate
// runs; it is still bounded at maxLastGoodGenerations credential
// generations (FIFO eviction), so plugins sharing one catalog across
// many auths should call Invalidate on credential rotation.
// Fetch verification is catalog availability only: it does not bypass
// credential validity, auth disablement, provider entitlements or
// executor authorization, which the host and executor still enforce
// per-request.
//
// Concurrency-safe; the constructor starts no goroutine and there is no
// background fetch. A catalog is fixed once installed; recreate it on
// reconfiguration, exactly like CachedQuota.
type ModelCatalog struct {
	namespace string
	static    []pluginapi.ModelInfo
	fetch     FetchModelsFunc
	ttl       time.Duration
	timeout   time.Duration
	now       func() time.Time
	overrides map[string]ModelOverride
	allow     []string
	deny      []string
	validate  func(ctx context.Context, req pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error)
	// waitHook is a test-only seam: invoked (without mu held) when a
	// concurrent waiter attaches to an in-flight fetch. Nil in use.
	waitHook      func()
	mu            sync.Mutex
	lastGood      map[modelCatalogKey][]pluginapi.ModelInfo
	lastGoodOrder []modelCatalogKey
	freshness     map[modelCatalogKey]time.Time
	inflight      map[modelCatalogKey]*modelFlight
}

type modelCatalogKey struct {
	AuthID, Provider string
	Credentials      [32]byte
}

type modelFlight struct {
	ready          chan struct{}
	models         []pluginapi.ModelInfo
	err            error
	leaderCanceled bool
}

// NewModelCatalog creates an empty model catalog for its lifetime.
func NewModelCatalog(config ModelCatalogConfig) *ModelCatalog {
	return &ModelCatalog{
		namespace: config.Namespace,
		static:    cloneModelInfos(config.Static),
		fetch:     config.Fetch,
		ttl:       config.TTL,
		timeout:   ModelsFetchTimeout,
		now:       time.Now,
		overrides: cloneOverrides(config.Overrides),
		allow:     slices.Clone(config.Allow),
		deny:      slices.Clone(config.Deny),
		validate:  config.Validate,
		lastGood:  make(map[modelCatalogKey][]pluginapi.ModelInfo),
		freshness: make(map[modelCatalogKey]time.Time),
		inflight:  make(map[modelCatalogKey]*modelFlight),
	}
}

// NewModelCatalogWithTTL records a per-plugin TTL for the freshness
// markers. Like NewCachedQuotaWithTTL it mirrors the old constructor, but
// it does not introduce a skip gate: ModelsForAuth fetches on every
// invocation regardless of the TTL (see ModelCatalogConfig.TTL).
func NewModelCatalogWithTTL(config ModelCatalogConfig, ttl time.Duration) *ModelCatalog {
	if ttl > 0 {
		config.TTL = ttl
	}
	return NewModelCatalog(config)
}

func (c *ModelCatalog) authKey(req pluginapi.AuthModelRequest, provider string) (modelCatalogKey, error) {
	raw, err := json.Marshal(struct {
		Storage    []byte
		Metadata   map[string]any
		Attributes map[string]string
	}{req.StorageJSON, req.Metadata, req.Attributes})
	if err != nil {
		return modelCatalogKey{}, rpcError("invalid_auth", "invalid model credential context", 400)
	}
	return modelCatalogKey{AuthID: req.AuthID, Provider: provider, Credentials: sha256.Sum256(raw)}, nil
}

func (c *ModelCatalog) fetchTimeout() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	return ModelsFetchTimeout
}

// pruneFreshnessLocked drops expired freshness markers. Last-good entries
// are never pruned here, so TTL expiry cannot erase the fallback.
func (c *ModelCatalog) pruneFreshnessLocked(now time.Time) {
	if c.ttl <= 0 {
		return
	}
	for k, at := range c.freshness {
		if !now.Before(at.Add(c.ttl)) {
			delete(c.freshness, k)
		}
	}
}

// StaticModels serves the static list through the uniform pipeline.
func (c *ModelCatalog) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pluginapi.ModelResponse{}, err
	}
	return pluginapi.ModelResponse{Provider: req.Plugin.Name, Models: c.pipeline(c.static)}, nil
}

// RegisterModels serves the static list through the uniform pipeline, for
// development-time registration.
func (c *ModelCatalog) RegisterModels(ctx context.Context, req pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pluginapi.ModelRegistrationResponse{}, err
	}
	return pluginapi.ModelRegistrationResponse{Provider: req.Plugin.Name, Models: c.pipeline(c.static)}, nil
}

// ModelsForAuth fetches fresh models for one auth and serves the uniform
// pipeline result. Concurrent calls for the same credential generation
// share one fetch; sequential calls each fetch.
func (c *ModelCatalog) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pluginapi.ModelResponse{}, err
	}
	provider := req.AuthProvider
	if provider == "" {
		provider = req.Plugin.Name
	}
	key, err := c.authKey(req, provider)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	for {
		c.mu.Lock()
		c.pruneFreshnessLocked(c.now())
		if e := c.inflight[key]; e != nil {
			hook := c.waitHook
			c.mu.Unlock()
			if hook != nil {
				hook()
			}
			select {
			case <-ctx.Done():
				return pluginapi.ModelResponse{}, ctx.Err()
			case <-e.ready:
			}
			// The shared leader failed on its own cancelled
			// context and there is nothing to fall back to: a
			// waiter with a live context retries instead of
			// inheriting the other caller's cancellation.
			if e.leaderCanceled && e.err != nil && ctx.Err() == nil {
				continue
			}
			return pluginapi.ModelResponse{Provider: provider, Models: cloneModelInfos(e.models)}, e.err
		}
		e := &modelFlight{ready: make(chan struct{})}
		c.inflight[key] = e
		c.mu.Unlock()
		models, cache, ferr := c.fetchAndServe(ctx, req, key)
		e.leaderCanceled = ctx.Err() != nil
		c.mu.Lock()
		if c.inflight[key] == e {
			delete(c.inflight, key)
			if c.ttl > 0 {
				c.freshness[key] = c.now()
			}
			if cache {
				c.cacheLastGoodLocked(key, models)
			}
		}
		e.models = cloneModelInfos(models)
		e.err = ferr
		close(e.ready)
		c.mu.Unlock()
		return pluginapi.ModelResponse{Provider: provider, Models: cloneModelInfos(models)}, ferr
	}
}

// fetchAndServe runs the fresh path or the fallback chain and reports
// whether the result should be cached as last-good (only a fresh success,
// including a legitimate filtered-empty, is cached).
func (c *ModelCatalog) fetchAndServe(ctx context.Context, req pluginapi.AuthModelRequest, key modelCatalogKey) (models []pluginapi.ModelInfo, cache bool, err error) {
	if c.fetch == nil {
		return c.pipeline(c.static), false, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, c.fetchTimeout())
	defer cancel()
	raw, ferr := c.callFetch(fetchCtx, req)
	if ferr == nil && len(raw) == 0 {
		ferr = rpcError("models_fetch_failed", "model discovery returned no models", 502)
	}
	var mid []pluginapi.ModelInfo
	if ferr == nil {
		mid = c.applyGlobOverrides(raw)
		if c.validate != nil {
			// Preserve the glob decisions already made for the
			// pipeline output: a ContributorID rewrite is allowed
			// because its source id was, so re-checking the
			// rewritten id against Allow would drop an approved
			// model. Validate-added ids still face the full
			// allow/deny policy.
			approved := make(map[string]bool, len(mid))
			for _, m := range mid {
				approved[m.ID] = true
			}
			mid, ferr = c.callValidate(fetchCtx, req, mid)
			if ferr == nil {
				mid = c.filterAfterValidate(mid, approved)
			}
		}
	}
	if ferr == nil {
		out := c.applyNamespace(mid)
		if len(mid) > 0 && len(out) == 0 {
			// Every surviving candidate was rejected by the
			// namespace filter: the upstream payload is
			// incompatible with this catalog, not an intentional
			// empty policy outcome. Treat the refresh as a
			// failed fetch so last-good survives.
			ferr = rpcError("models_fetch_failed", "model discovery returned no usable models", 502)
		} else {
			return out, true, nil
		}
	}
	return c.fallback(key, ferr)
}

func (c *ModelCatalog) callFetch(ctx context.Context, req pluginapi.AuthModelRequest) (models []pluginapi.ModelInfo, err error) {
	defer func() {
		if recover() != nil {
			models, err = nil, rpcError("models_fetch_failed", "model discovery failed", 502)
		}
	}()
	return c.fetch(ctx, req)
}

func (c *ModelCatalog) callValidate(ctx context.Context, req pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) (out []pluginapi.ModelInfo, err error) {
	defer func() {
		if recover() != nil {
			out, err = nil, rpcError("models_validate_failed", "model validation failed", 502)
		}
	}()
	out, err = c.validate(ctx, req, cloneModelInfos(models))
	if err != nil {
		return nil, err
	}
	if out == nil {
		return []pluginapi.ModelInfo{}, nil
	}
	return cloneModelInfos(out), nil
}

// cacheLastGoodLocked stores one successful generation, evicting the
// oldest generations first once the store is full. Callers hold mu.
func (c *ModelCatalog) cacheLastGoodLocked(key modelCatalogKey, models []pluginapi.ModelInfo) {
	if _, ok := c.lastGood[key]; !ok {
		c.lastGoodOrder = append(c.lastGoodOrder, key)
		for len(c.lastGoodOrder) > maxLastGoodGenerations {
			delete(c.lastGood, c.lastGoodOrder[0])
			c.lastGoodOrder = c.lastGoodOrder[1:]
		}
	}
	c.lastGood[key] = cloneModelInfos(models)
}

// fallback never drops a known model: last-good first (even a cached
// legitimate empty), then the static list through the pipeline. The fetch
// error surfaces only when nothing is available.
func (c *ModelCatalog) fallback(key modelCatalogKey, ferr error) ([]pluginapi.ModelInfo, bool, error) {
	c.mu.Lock()
	lg, ok := c.lastGood[key]
	c.mu.Unlock()
	if ok {
		return cloneModelInfos(lg), false, nil
	}
	if len(c.static) > 0 {
		return c.pipeline(c.static), false, nil
	}
	return nil, false, ferr
}

// Invalidate discards the last-good entry (and any in-flight fetch) for
// the given auth ID. A fetch completing after Invalidate returns its
// result to its own callback but never resurrects the invalidated
// generation: its last-good write checks the in-flight entry is still
// current under lock. It fires no host callback (see the package
// freshness notes: recovery waits for the next host event).
func (c *ModelCatalog) Invalidate(authID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.lastGood {
		if key.AuthID == authID {
			delete(c.lastGood, key)
		}
	}
	c.lastGoodOrder = slices.DeleteFunc(c.lastGoodOrder, func(key modelCatalogKey) bool {
		return key.AuthID == authID
	})
	for key := range c.freshness {
		if key.AuthID == authID {
			delete(c.freshness, key)
		}
	}
	for key := range c.inflight {
		if key.AuthID == authID {
			delete(c.inflight, key)
		}
	}
}

// pipeline applies the uniform treatment every source receives (fresh,
// last-good, static): glob filter → overrides → namespace prefix and
// validation. Fresh and static run it per call; last-good stores a prior
// pipeline result and is served from a copy, so it inherently carries the
// same treatment.
func (c *ModelCatalog) pipeline(raw []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return c.applyNamespace(c.applyGlobOverrides(raw))
}

// applyGlobOverrides filters, contributor-deduplicates and applies
// overrides against upstream (pre-namespace) ids. Contributor dedup: when
// both an id and its "-contributor" sibling appear, the contributor entry
// wins and the base entry is dropped, so the model is neither
// double-listed nor served under both ids. This needs no per-model
// override: the pairing is discovered from the source list itself. A
// model with no sibling keeps its base id. A final-id dedup after
// overrides (e.g. an explicit ContributorID rewrite colliding with a real
// sibling) keeps the first entry.
func (c *ModelCatalog) applyGlobOverrides(raw []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	filtered := c.filterByGlobs(cloneModelInfos(raw))
	present := make(map[string]bool, len(filtered))
	for _, m := range filtered {
		present[m.ID] = true
	}
	deduped := make([]pluginapi.ModelInfo, 0, len(filtered))
	for _, m := range filtered {
		if present[m.ID+"-contributor"] && !strings.HasSuffix(m.ID, "-contributor") {
			continue
		}
		deduped = append(deduped, m)
	}
	seen := make(map[string]bool, len(deduped))
	out := make([]pluginapi.ModelInfo, 0, len(deduped))
	for _, m := range deduped {
		if ov, ok := c.overrides[m.ID]; ok {
			if ov.DisplayName != "" {
				m.DisplayName = ov.DisplayName
			}
			if ov.ContextLength != 0 {
				m.ContextLength = ov.ContextLength
			}
			if ov.MaxCompletionTokens != 0 {
				m.MaxCompletionTokens = ov.MaxCompletionTokens
			}
			if ov.ContributorID != "" {
				m.ID = ov.ContributorID
			}
			if ov.Thinking != nil {
				m.Thinking = cloneThinking(ov.Thinking)
			}
		}
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

// applyNamespace prefixes upstream ids with the catalog namespace (ids
// already carrying it are left for the kit filter to judge, so a doubled
// namespace is still dropped) and enforces it with the kit filterModels
// rule.
func (c *ModelCatalog) applyNamespace(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if c.namespace == "" {
		return models
	}
	prefixed := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if !strings.HasPrefix(m.ID, c.namespace+"/") {
			m.ID = c.namespace + "/" + m.ID
		}
		prefixed = append(prefixed, m)
	}
	return filterModels(c.namespace, prefixed)
}

// filterByGlobs applies the allow/deny policy alone.
func (c *ModelCatalog) filterByGlobs(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	filtered := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if !globAllows(c.allow, m.ID) || globDenies(c.deny, m.ID) {
			continue
		}
		filtered = append(filtered, m)
	}
	return filtered
}

// filterAfterValidate applies the allow/deny policy to Validate output
// while preserving the upstream glob decisions. An id the pipeline
// already approved (a ContributorID rewrite whose source passed the
// first glob pass) is kept even when it fails Allow on its rewritten
// form; deny always wins, so a denied id never survives either way.
// Ids Validate invented still need the full policy check.
func (c *ModelCatalog) filterAfterValidate(models []pluginapi.ModelInfo, approved map[string]bool) []pluginapi.ModelInfo {
	filtered := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if globDenies(c.deny, m.ID) {
			continue
		}
		if !approved[m.ID] && !globAllows(c.allow, m.ID) {
			continue
		}
		filtered = append(filtered, m)
	}
	return filtered
}

func globAllows(allow []string, id string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, pattern := range allow {
		if match, _ := path.Match(pattern, id); match {
			return true
		}
	}
	return false
}

func globDenies(deny []string, id string) bool {
	for _, pattern := range deny {
		if match, _ := path.Match(pattern, id); match {
			return true
		}
	}
	return false
}

// cloneOverrides deep-copies the override map, including each Thinking
// value, so later mutation of the config cannot alter served models.
func cloneOverrides(in map[string]ModelOverride) map[string]ModelOverride {
	if in == nil {
		return nil
	}
	out := make(map[string]ModelOverride, len(in))
	for k, ov := range in {
		ov.Thinking = cloneThinking(ov.Thinking)
		out[k] = ov
	}
	return out
}

func cloneModelInfos(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if models == nil {
		return nil
	}
	out := make([]pluginapi.ModelInfo, len(models))
	for i, m := range models {
		out[i] = m
		out[i].Thinking = cloneThinking(m.Thinking)
		out[i].SupportedGenerationMethods = slices.Clone(m.SupportedGenerationMethods)
		out[i].SupportedParameters = slices.Clone(m.SupportedParameters)
		out[i].SupportedInputModalities = slices.Clone(m.SupportedInputModalities)
		out[i].SupportedOutputModalities = slices.Clone(m.SupportedOutputModalities)
	}
	return out
}

func cloneThinking(t *pluginapi.ThinkingSupport) *pluginapi.ThinkingSupport {
	if t == nil {
		return nil
	}
	out := *t
	out.Levels = slices.Clone(t.Levels)
	return &out
}

var _ pluginapi.ModelProvider = (*ModelCatalog)(nil)
var _ pluginapi.ModelRegistrar = (*ModelCatalog)(nil)
