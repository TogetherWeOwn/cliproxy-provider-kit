package providerkit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// QuotaCacheTTL is the maximum age of a successful per-auth quota observation.
const QuotaCacheTTL = 60 * time.Second

// FetchQuotaFunc fetches and normalizes one account. Provider-owned protocol
// parsing stays outside the kit. Errors are never cached; stale data is not
// silently returned. Honor context cancellation.
type FetchQuotaFunc func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error)

// CachedQuota implements a non-resettable SDK quota provider with per-account
// single-flight fetching, copy isolation, and a 60-second cache. Use Store to
// incorporate normalized inference response headers by auth identity without
// another HTTP poll or access to the host's credential context.
type CachedQuota struct {
	provider, display string
	fetch             FetchQuotaFunc
	now               func() time.Time
	mu                sync.Mutex
	entries           map[quotaKey]*quotaEntry
	observations      map[string]*quotaEntry
}
type quotaKey struct {
	AuthID, Provider string
	Credentials      [32]byte
}
type quotaEntry struct {
	ready   chan struct{}
	expires time.Time
	raw     []byte
	err     error
}

// NewCachedQuota creates an empty quota cache. The identity and callback are
// fixed for its lifetime. No background polls or provider requests are started.
func NewCachedQuota(provider, display string, fetch FetchQuotaFunc) *CachedQuota {
	return &CachedQuota{provider: provider, display: display, fetch: fetch, now: time.Now, entries: make(map[quotaKey]*quotaEntry), observations: make(map[string]*quotaEntry)}
}
func (q *CachedQuota) Identifier() string { return q.provider }
func (q *CachedQuota) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{q.provider}, DisplayName: q.display, SupportsReset: false}, nil
}
func (q *CachedQuota) authID(r pluginapi.QuotaFetchRequest) (string, error) {
	id := r.AuthID
	if id == "" {
		id = r.AuthIndex
	}
	if id == "" || q.provider == "" || r.Provider != "" && r.Provider != q.provider {
		return "", rpcError("invalid_auth", "quota requires a matching provider and auth identity", 400)
	}
	return id, nil
}
func (q *CachedQuota) key(r pluginapi.QuotaFetchRequest) (quotaKey, error) {
	id, err := q.authID(r)
	if err != nil {
		return quotaKey{}, err
	}
	raw, err := json.Marshal(struct {
		Storage    []byte
		Metadata   map[string]any
		Attributes map[string]string
	}{r.StorageJSON, r.Metadata, r.Attributes})
	if err != nil {
		return quotaKey{}, rpcError("invalid_auth", "invalid quota credential context", 400)
	}
	return quotaKey{id, q.provider, sha256.Sum256(raw)}, nil
}
func decodeQuota(e *quotaEntry) (pluginapi.QuotaFetchResponse, error) {
	if e.err != nil {
		return pluginapi.QuotaFetchResponse{}, e.err
	}
	var out pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(e.raw, &out); err != nil {
		return out, rpcError("invalid_quota", "invalid cached quota", 500)
	}
	return out, nil
}
func (q *CachedQuota) FetchQuota(ctx context.Context, r pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if err := ctx.Err(); err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	key, err := q.key(r)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	q.mu.Lock()
	now := q.now()
	// Evict expired completed observations, including obsolete credential versions.
	for k, e := range q.entries {
		if !e.expires.IsZero() && !now.Before(e.expires) {
			delete(q.entries, k)
		}
	}
	for id, e := range q.observations {
		if !now.Before(e.expires) {
			delete(q.observations, id)
		}
	}
	// Usage records carry auth identity but no credential context. Prefer their
	// fresh header observations even when the host enriches the fetch request.
	if e := q.observations[key.AuthID]; e != nil {
		q.mu.Unlock()
		return decodeQuota(e)
	}
	if e := q.entries[key]; e != nil {
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return pluginapi.QuotaFetchResponse{}, ctx.Err()
		case <-e.ready:
			return decodeQuota(e)
		}
	}
	e := &quotaEntry{ready: make(chan struct{})}
	q.entries[key] = e
	q.mu.Unlock()
	// Publish completion even if a provider callback panics, so waiters never hang.
	func() {
		defer func() {
			if recover() != nil {
				e.err = rpcError("plugin_panic", "quota operation panicked", 500)
			}
		}()
		if q.fetch == nil {
			e.err = rpcError("unsupported_method", "quota fetch is not configured", 501)
			return
		}
		var out pluginapi.QuotaFetchResponse
		// Route direct polls like host requests: auth proxy, then global proxy.
		out, e.err = q.fetch(WithProxyURL(ctx, AuthProxyURL(r.StorageJSON, r.Metadata, r.Host.ProxyURL)), r)
		if e.err == nil {
			e.raw, e.err = json.Marshal(out)
			if e.err != nil {
				e.err = rpcError("invalid_quota", "quota response encoding failed", 500)
			}
		}
	}()
	q.mu.Lock()
	e.expires = q.now().Add(QuotaCacheTTL)
	if e.err != nil && q.entries[key] == e {
		delete(q.entries, key)
	}
	close(e.ready)
	q.mu.Unlock()
	return decodeQuota(e)
}

// Store replaces an auth-identity observation (for example, from a provider
// quota header). Only AuthID (or AuthIndex when AuthID is absent) and an optional
// matching Provider are used; credential context is neither needed nor keyed.
// Fresh observations take precedence over polls, including older in-flight
// polls. Data is cloned on store and read. Invalidate when reusing an identity
// for a different credential, or recreate the cache on reconfiguration.
func (q *CachedQuota) Store(r pluginapi.QuotaFetchRequest, out pluginapi.QuotaFetchResponse) error {
	id, err := q.authID(r)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return rpcError("invalid_quota", "quota response encoding failed", 500)
	}
	e := &quotaEntry{ready: make(chan struct{}), raw: raw}
	close(e.ready)
	q.mu.Lock()
	e.expires = q.now().Add(QuotaCacheTTL)
	q.observations[id] = e
	q.mu.Unlock()
	return nil
}

// Invalidate discards header observations and all polled credential versions
// for the given auth ID (or AuthIndex when used instead of AuthID).
func (q *CachedQuota) Invalidate(authID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.observations, authID)
	for key := range q.entries {
		if key.AuthID == authID {
			delete(q.entries, key)
		}
	}
}
func (q *CachedQuota) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{}, rpcError("reset_unsupported", "this quota provider does not support resets", 400)
}

var _ pluginapi.QuotaProvider = (*CachedQuota)(nil)
