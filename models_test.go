package providerkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeModelsHostClient struct {
	mu      sync.Mutex
	calls   int
	doErr   error
	status  int
	body    []byte
	block   bool
	sawDone bool
	// blocked, when non-nil, receives one signal when a Do call
	// enters its blocking path so tests can cancel deterministically
	// instead of sleeping. Buffered cap 1, sent non-blockingly.
	blocked chan struct{}
	lastReq pluginapi.HTTPRequest
	lastURL string
}

func (f *fakeModelsHostClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.calls++
	f.lastReq = req
	f.lastURL = req.URL
	doErr, status, body, block := f.doErr, f.status, f.body, f.block
	f.mu.Unlock()
	if block {
		if f.blocked != nil {
			select {
			case f.blocked <- struct{}{}:
			default:
			}
		}
		<-ctx.Done()
		f.mu.Lock()
		f.sawDone = true
		f.mu.Unlock()
		return pluginapi.HTTPResponse{}, ctx.Err()
	}
	if doErr != nil {
		return pluginapi.HTTPResponse{}, doErr
	}
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{}, Body: slices.Clone(body)}, nil
}

func (f *fakeModelsHostClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("streaming not supported in fixture")
}

func (f *fakeModelsHostClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type modelsFixture struct {
	mu     sync.Mutex
	models []pluginapi.ModelInfo
	err    error
	calls  int
}

func (f *modelsFixture) fetch(_ context.Context, _ pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

func (f *modelsFixture) set(models []pluginapi.ModelInfo, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = models
	f.err = err
}

func (f *modelsFixture) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func catalogAuthReq(authID string) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{
		AuthID:       authID,
		AuthProvider: "meta",
		StorageJSON:  []byte(`{"api_key":"test-key"}`),
		Plugin:       pluginapi.Metadata{Name: "meta"},
	}
}

func namedModels(ids ...string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", DisplayName: id})
	}
	return out
}

func TestModelCatalogOpenAIParseHeadersAndOverrides(t *testing.T) {
	host := &fakeModelsHostClient{status: 200, body: []byte(`{"data":[{"id":"m-one","object":"model","created":123,"owned_by":"test"},{"id":"m-two"}]}`)}
	fetcher := OpenAIModelsFetcher("https://api.example.test/v1/", "/models", func(_ pluginapi.AuthModelRequest) http.Header {
		return http.Header{"Authorization": {"Bearer k"}}
	})
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fetcher})
	req := catalogAuthReq("auth-1")
	req.HTTPClient = host
	resp, err := cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/m-one", "meta/m-two"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want %v", got, want)
	}
	host.mu.Lock()
	url := host.lastURL
	auth := host.lastReq.Headers.Get("Authorization")
	host.mu.Unlock()
	if url != "https://api.example.test/v1/models" {
		t.Fatalf("url %q, want joined baseURL+path", url)
	}
	if auth != "Bearer k" {
		t.Fatalf("authorization %q, want headers plumbed", auth)
	}
	if resp.Models[0].Created != 123 || resp.Models[0].OwnedBy != "test" {
		t.Fatalf("parsed fields %+v, want created/owned_by carried", resp.Models[0])
	}
}

func TestModelCatalogOverridesApplyToFreshAndStatic(t *testing.T) {
	fix := &modelsFixture{models: namedModels("m-one")}
	cat := NewModelCatalog(ModelCatalogConfig{
		Namespace: "meta",
		Static:    namedModels("s-one"),
		Fetch:     fix.fetch,
		Overrides: map[string]ModelOverride{
			"m-one": {DisplayName: "One", ContextLength: 100, MaxCompletionTokens: 10, Thinking: &pluginapi.ThinkingSupport{Levels: []string{"low", "high"}, ZeroAllowed: true}},
			"s-one": {DisplayName: "Static One"},
		},
	})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("models %+v", resp.Models)
	}
	m := resp.Models[0]
	if m.ID != "meta/m-one" || m.DisplayName != "One" || m.ContextLength != 100 || m.MaxCompletionTokens != 10 {
		t.Fatalf("overridden %+v", m)
	}
	if m.Thinking == nil || !slices.Equal(m.Thinking.Levels, []string{"low", "high"}) || !m.Thinking.ZeroAllowed {
		t.Fatalf("thinking %+v, want levels preserved", m.Thinking)
	}
	// Uniform pipeline: static passes the same glob/override/namespace
	// treatment through StaticModels.
	sresp, err := cat.StaticModels(context.Background(), pluginapi.StaticModelRequest{Plugin: pluginapi.Metadata{Name: "meta"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(sresp.Models), []string{"meta/s-one"}; !slices.Equal(got, want) {
		t.Fatalf("static ids %v, want %v", got, want)
	}
	if sresp.Models[0].DisplayName != "Static One" {
		t.Fatalf("static display %q, want overridden", sresp.Models[0].DisplayName)
	}
	// RegisterModels serves the same static list through the pipeline.
	rresp, err := cat.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{Plugin: pluginapi.Metadata{Name: "meta"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(rresp.Models), []string{"meta/s-one"}; !slices.Equal(got, want) || rresp.Provider != "meta" {
		t.Fatalf("register %+v, want pipeline static for meta", rresp)
	}
}

func TestModelCatalogContributorDedupForUnknownIDs(t *testing.T) {
	// No per-model override for these previously unknown ids: the pairing
	// is discovered from the source list itself via the -contributor
	// sibling, so the contributor entry wins and the base entry is
	// dropped. A model with no sibling keeps its base id.
	fix := &modelsFixture{models: namedModels("unknown-a", "unknown-a-contributor", "unknown-solo")}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fix.fetch})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/unknown-a-contributor", "meta/unknown-solo"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want contributor win + kept base %v", got, want)
	}
	// Executor mapping: the executor strips exactly one leading namespace
	// before the upstream request, so the emitted contributor id reaches
	// the executor unchanged apart from that strip.
	for _, m := range resp.Models {
		rest, ok := strings.CutPrefix(m.ID, "meta/")
		if !ok || rest == "" || strings.HasPrefix(rest, "meta/") {
			t.Fatalf("emitted %q does not survive one-namespace strip", m.ID)
		}
	}
	if got := strings.TrimPrefix(resp.Models[0].ID, "meta/"); got != "unknown-a-contributor" {
		t.Fatalf("executor upstream id %q, want contributor variant", got)
	}
	if err := CheckModelNamespace("meta", resp.Models); err != nil {
		t.Fatalf("runtime namespace check: %v", err)
	}
}

func TestModelCatalogContributorHandOverride(t *testing.T) {
	fix := &modelsFixture{models: namedModels("known")}
	cat := NewModelCatalog(ModelCatalogConfig{
		Namespace: "meta",
		Fetch:     fix.fetch,
		Overrides: map[string]ModelOverride{"known": {ContributorID: "known-contributor"}},
	})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/known-contributor"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want contributor rewrite %v", got, want)
	}
}

func TestModelCatalogGlobsDenyWins(t *testing.T) {
	fix := &modelsFixture{models: namedModels("alpha-1", "alpha-2", "beta-1")}
	cat := NewModelCatalog(ModelCatalogConfig{
		Namespace: "meta",
		Fetch:     fix.fetch,
		Allow:     []string{"alpha-*"},
		Deny:      []string{"alpha-2"},
	})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/alpha-1"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want deny-wins glob result %v", got, want)
	}
}

func TestModelCatalogFilteredEmptyIsServedNotFailure(t *testing.T) {
	fix := &modelsFixture{models: namedModels("a", "b")}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("static-1"), Fetch: fix.fetch, Deny: []string{"*"}})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("filtered-empty served %v, want empty (legitimate policy), not static", modelIDs(resp.Models))
	}
	// The filtered-empty is cached as last-good: a later failing fetch
	// keeps serving empty rather than revealing static.
	fix.set(nil, errors.New("boom"))
	resp, err = cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("after failing fetch served %v, want cached filtered-empty", modelIDs(resp.Models))
	}
}

func TestModelCatalogEmptyUpstreamIsFailure(t *testing.T) {
	fix := &modelsFixture{models: namedModels("a")}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fix.fetch})
	if _, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1")); err != nil {
		t.Fatal(err)
	}
	fix.set(nil, nil)
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("empty upstream served %v, want last-good %v", got, want)
	}
	// With nothing available, an empty upstream surfaces an error.
	bare := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: (&modelsFixture{models: nil}).fetch})
	if _, err := bare.ModelsForAuth(context.Background(), catalogAuthReq("auth-1")); err == nil {
		t.Fatal("empty upstream with no fallback must error")
	}
}

func TestModelCatalogTimelineAFreshConvergesAndIgnoresTTL(t *testing.T) {
	fix := &modelsFixture{models: namedModels("a")}
	// A TTL larger than the interval between invocations must not skip
	// the second fetch: every ModelsForAuth fetches.
	cat := NewModelCatalogWithTTL(ModelCatalogConfig{Namespace: "meta", Fetch: fix.fetch}, time.Hour)
	if _, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1")); err != nil {
		t.Fatal(err)
	}
	fix.set(namedModels("a", "b"), nil)
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a", "meta/b"}; !slices.Equal(got, want) {
		t.Fatalf("timeline A served %v, want fresh convergence %v", got, want)
	}
	if n := fix.fetchCount(); n != 2 {
		t.Fatalf("fetches %d, want every invocation to fetch (2)", n)
	}
}

func TestModelCatalogTimelineBFailureThenNoGuaranteedRecovery(t *testing.T) {
	fix := &modelsFixture{models: namedModels("a")}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fix.fetch})
	if _, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1")); err != nil {
		t.Fatal(err)
	}
	// The fixture gains B but the one-shot callback's fetch fails
	// transiently: last-good A is served and B is not published. No
	// further invocation is guaranteed by the kit (recovery waits for
	// the next host event); this test asserts that absence — the fetch
	// count stays stable and no background work publishes B.
	fix.set(namedModels("a", "b"), errors.New("transient"))
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("timeline B served %v, want last-good %v (B unpublished)", got, want)
	}
	before := fix.fetchCount()
	time.Sleep(50 * time.Millisecond)
	if n := fix.fetchCount(); n != before {
		t.Fatalf("fetches %d, want %d: no background fetch without a host callback", n, before)
	}
}

func TestModelCatalogErrorFallbackStaticAndSurfacedError(t *testing.T) {
	fix := &modelsFixture{models: namedModels("a")}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("static-1"), Fetch: fix.fetch})
	if _, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1")); err != nil {
		t.Fatal(err)
	}
	fix.set(nil, errors.New("boom"))
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("error served %v, want last-good %v", got, want)
	}
	// No last-good yet: static serves on error. Nothing at all: error
	// surfaces (catalog availability never invents models).
	fresh := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("static-1"), Fetch: (&modelsFixture{err: errors.New("boom")}).fetch})
	resp, err = fresh.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/static-1"}; !slices.Equal(got, want) {
		t.Fatalf("static fallback served %v, want %v", got, want)
	}
	nothing := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: (&modelsFixture{err: errors.New("boom")}).fetch})
	if _, err := nothing.ModelsForAuth(context.Background(), catalogAuthReq("auth-1")); err == nil {
		t.Fatal("fetch error with no fallback must surface")
	}
}

func TestModelCatalogValidateHookFilterAndError(t *testing.T) {
	fix := &modelsFixture{models: namedModels("good", "bad")}
	cat := NewModelCatalog(ModelCatalogConfig{
		Namespace: "meta",
		Fetch:     fix.fetch,
		Validate: func(_ context.Context, _ pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
			out := models[:0:0]
			for _, m := range models {
				if m.ID == "good" {
					out = append(out, m)
				}
			}
			return out, nil
		},
	})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/good"}; !slices.Equal(got, want) {
		t.Fatalf("validate-filtered served %v, want %v (cached as last-good)", got, want)
	}
	// A failed Validate never replaces last-good.
	failing := NewModelCatalog(ModelCatalogConfig{
		Namespace: "meta",
		Static:    namedModels("static-1"),
		Fetch:     fix.fetch,
		Validate: func(context.Context, pluginapi.AuthModelRequest, []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
			return nil, errors.New("probe failed")
		},
	})
	resp, err = failing.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/static-1"}; !slices.Equal(got, want) {
		t.Fatalf("failed Validate served %v, want static %v", got, want)
	}
}

func TestModelCatalogOversizedBodyIsFailedFetch(t *testing.T) {
	host := &fakeModelsHostClient{status: 200, body: make([]byte, MaxModelsBody+1)}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("static-1"), Fetch: OpenAIModelsFetcher("https://api.example.test/v1", "/models", nil)})
	req := catalogAuthReq("auth-1")
	req.HTTPClient = host
	resp, err := cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/static-1"}; !slices.Equal(got, want) {
		t.Fatalf("oversized served %v, want static fallback %v", got, want)
	}
	// The oversized body was never parsed or cached as last-good: a
	// following error still falls back to static (nothing better seen).
}

func TestModelCatalogFreshnessExpiryNeverErasesLastGood(t *testing.T) {
	fix := &modelsFixture{models: namedModels("a")}
	cat := NewModelCatalogWithTTL(ModelCatalogConfig{Namespace: "meta", Fetch: fix.fetch}, time.Minute)
	base := time.Now()
	now := base
	cat.now = func() time.Time { return now }
	if _, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1")); err != nil {
		t.Fatal(err)
	}
	now = base.Add(2 * time.Minute)
	fix.set(nil, errors.New("boom"))
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("past-TTL served %v, want durable last-good %v", got, want)
	}
}

func TestModelCatalogCredentialGenerationIsolation(t *testing.T) {
	fix := &modelsFixture{models: namedModels("a")}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("static-1"), Fetch: fix.fetch})
	reqA := catalogAuthReq("auth-1")
	if _, err := cat.ModelsForAuth(context.Background(), reqA); err != nil {
		t.Fatal(err)
	}
	// A rotated credential under the same auth identity must never
	// receive the old generation's list: its fetch fails, so it falls
	// back to static instead of generation A's last-good.
	reqB := catalogAuthReq("auth-1")
	reqB.StorageJSON = []byte(`{"api_key":"rotated-key"}`)
	fix.set(nil, errors.New("boom"))
	resp, err := cat.ModelsForAuth(context.Background(), reqB)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/static-1"}; !slices.Equal(got, want) {
		t.Fatalf("rotated credential served %v, want static %v, not stale last-good", got, want)
	}
	// The same generation still gets its own last-good.
	resp, err = cat.ModelsForAuth(context.Background(), reqA)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("original generation served %v, want last-good %v", got, want)
	}
}

func TestModelCatalogSingleFlightConcurrentOnly(t *testing.T) {
	var calls atomic.Int32
	fetch := func(ctx context.Context, _ pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
		calls.Add(1)
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return namedModels("a"), nil
	}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fetch})
	req := catalogAuthReq("auth-1")
	var wg sync.WaitGroup
	results := make([][]string, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := cat.ModelsForAuth(context.Background(), req)
			if err != nil {
				t.Errorf("concurrent call: %v", err)
				return
			}
			results[i] = modelIDs(resp.Models)
		}(i)
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("concurrent fetches %d, want single-flight coalesced to 1", n)
	}
	for i, got := range results {
		if want := []string{"meta/a"}; !slices.Equal(got, want) {
			t.Fatalf("waiter %d served %v, want %v", i, got, want)
		}
	}
	// Sequential invocations each fetch: single-flight never suppresses
	// a later callback.
	if _, err := cat.ModelsForAuth(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("fetches %d, want sequential call to fetch again (2)", n)
	}
}

func TestModelCatalogCopyIsolation(t *testing.T) {
	fix := &modelsFixture{models: namedModels("a")}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fix.fetch})
	req := catalogAuthReq("auth-1")
	resp, err := cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Models[0].ID = "tampered"
	fix.set(nil, errors.New("boom"))
	resp, err = cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("post-mutation served %v, want isolated last-good %v", got, want)
	}
}

func TestModelCatalogLateCompletionNoResurrect(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	fetch := func(ctx context.Context, _ pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
		if calls.Add(1) == 1 {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-gate:
				return namedModels("late-b"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("boom")
	}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("static-1"), Fetch: fetch})
	req := catalogAuthReq("auth-1")
	done := make(chan pluginapi.ModelResponse, 1)
	go func() {
		resp, _ := cat.ModelsForAuth(context.Background(), req)
		done <- resp
	}()
	<-started
	cat.Invalidate("auth-1")
	close(gate)
	select {
	case first := <-done:
		if got, want := modelIDs(first.Models), []string{"meta/late-b"}; !slices.Equal(got, want) {
			t.Fatalf("in-flight caller served %v, want its own fetch %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call did not return")
	}
	// The completion may serve its own caller, but it must not
	// resurrect the invalidated generation as last-good.
	resp, err := cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/static-1"}; !slices.Equal(got, want) {
		t.Fatalf("post-invalidate served %v, want static %v, not resurrected late-b", got, want)
	}
}

func TestModelCatalogDerivedDeadlineFallsBack(t *testing.T) {
	host := &fakeModelsHostClient{status: 200, body: []byte(`{"data":[]}`), block: true}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("static-1"), Fetch: OpenAIModelsFetcher("https://api.example.test/v1", "/models", nil)})
	cat.timeout = 50 * time.Millisecond
	req := catalogAuthReq("auth-1")
	req.HTTPClient = host
	start := time.Now()
	resp, err := cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("blocked fetch returned after %s, want bounded by the derived deadline", elapsed)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/static-1"}; !slices.Equal(got, want) {
		t.Fatalf("deadline served %v, want static fallback %v", got, want)
	}
	host.mu.Lock()
	saw := host.sawDone
	host.mu.Unlock()
	if !saw {
		t.Fatal("host client never saw the derived deadline cancel its context")
	}
}

func TestModelCatalogParentCancellationPropagates(t *testing.T) {
	host := &fakeModelsHostClient{status: 200, body: []byte(`{"data":[]}`), block: true, blocked: make(chan struct{}, 1)}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("static-1"), Fetch: OpenAIModelsFetcher("https://api.example.test/v1", "/models", nil)})
	req := catalogAuthReq("auth-1")
	req.HTTPClient = host
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan pluginapi.ModelResponse, 1)
	go func() {
		resp, _ := cat.ModelsForAuth(ctx, req)
		done <- resp
	}()
	select {
	case <-host.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached the blocking host call")
	}
	cancel()
	select {
	case resp := <-done:
		if got, want := modelIDs(resp.Models), []string{"meta/static-1"}; !slices.Equal(got, want) {
			t.Fatalf("canceled served %v, want static fallback %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled call did not return")
	}
	host.mu.Lock()
	saw := host.sawDone
	host.mu.Unlock()
	if !saw {
		t.Fatal("host client never saw parent cancellation")
	}
}

func TestModelCatalogForegroundOnlyNoBackgroundFetch(t *testing.T) {
	host := &fakeModelsHostClient{status: 200, body: []byte(`{"data":[{"id":"a"}]}`)}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: OpenAIModelsFetcher("https://api.example.test/v1", "/models", nil)})
	if n := host.callCount(); n != 0 {
		t.Fatalf("constructor made %d host calls, want foreground-only (0)", n)
	}
	req := catalogAuthReq("auth-1")
	req.HTTPClient = host
	if _, err := cat.ModelsForAuth(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := host.callCount(); n != 1 {
		t.Fatalf("host calls %d, want exactly the foreground fetch (1)", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := host.callCount(); n != 1 {
		t.Fatalf("host calls %d, want no background fetch after the callback", n)
	}
}

func TestModelCatalogStaticOnlyAndPanicFallback(t *testing.T) {
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("s-one")})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/s-one"}; !slices.Equal(got, want) {
		t.Fatalf("static-only served %v, want %v", got, want)
	}
	panics := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Static: namedModels("s-one"), Fetch: func(context.Context, pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
		panic("fetch panicked")
	}})
	resp, err = panics.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/s-one"}; !slices.Equal(got, want) {
		t.Fatalf("panicking fetch served %v, want static fallback %v", got, want)
	}
}

func TestModelCatalogNamespaceRejectedRefreshKeepsLastGood(t *testing.T) {
	fx := &modelsFixture{}
	fx.set(namedModels("a"), nil)
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fx.fetch})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("first fetch served %v, want %v", got, want)
	}
	// A refresh whose only candidate is namespace-incompatible
	// (already doubled prefix) is a failed fetch, not a legitimate
	// empty: last-good survives it.
	fx.set(namedModels("meta/meta/b"), nil)
	resp, err = cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatalf("namespace-rejected refresh err = %v, want last-good fallback", err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("namespace-rejected refresh served %v, want last-good %v", got, want)
	}
	// And a later transport failure still falls back to the
	// untouched last-good entry.
	fx.set(nil, errors.New("boom"))
	resp, err = cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/a"}; !slices.Equal(got, want) {
		t.Fatalf("later failure served %v, want last-good %v", got, want)
	}
}

func TestModelCatalogValidateCannotBypassDeny(t *testing.T) {
	fx := &modelsFixture{}
	fx.set(namedModels("approved"), nil)
	cat := NewModelCatalog(ModelCatalogConfig{
		Namespace: "meta",
		Fetch:     fx.fetch,
		Deny:      []string{"retired"},
		Validate: func(_ context.Context, _ pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
			return append(models, namedModels("retired")...), nil
		},
	})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/approved"}; !slices.Equal(got, want) {
		t.Fatalf("Validate-renamed denied id served %v, want %v", got, want)
	}
	// The denied id must not be cached either: a later fetch
	// failure serves the last-good without it.
	fx.set(nil, errors.New("boom"))
	resp, err = cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/approved"}; !slices.Equal(got, want) {
		t.Fatalf("last-good after denied Validate served %v, want %v", got, want)
	}
}

func TestModelCatalogFreshnessNotRecordedWithoutTTL(t *testing.T) {
	fx := &modelsFixture{}
	fx.set(nil, errors.New("boom"))
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fx.fetch})
	for i := 0; i < 5; i++ {
		_, _ = cat.ModelsForAuth(context.Background(), catalogAuthReq(fmt.Sprintf("auth-%d", i)))
	}
	if n := len(cat.freshness); n != 0 {
		t.Fatalf("freshness entries = %d, want 0 (TTL unset records nothing)", n)
	}
	withTTL := NewModelCatalogWithTTL(ModelCatalogConfig{Namespace: "meta", Fetch: fx.fetch}, time.Minute)
	_, _ = withTTL.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if n := len(withTTL.freshness); n == 0 {
		t.Fatal("freshness entries = 0 with explicit TTL, want the diagnostic marker")
	}
}

func TestModelCatalogOverridesThinkingCopiedAtConstruction(t *testing.T) {
	levels := []string{"low"}
	thinking := &pluginapi.ThinkingSupport{Min: 100, Levels: levels}
	config := ModelCatalogConfig{
		Namespace: "meta",
		Static:    namedModels("m"),
		Overrides: map[string]ModelOverride{"m": {Thinking: thinking}},
	}
	cat := NewModelCatalog(config)
	levels[0] = "high"
	thinking.Min = 999
	resp, err := cat.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 1 || resp.Models[0].Thinking == nil {
		t.Fatalf("static models = %+v, want one model with thinking", resp.Models)
	}
	if got := resp.Models[0].Thinking.Levels; !slices.Equal(got, []string{"low"}) {
		t.Fatalf("thinking levels = %v, want [low] (post-construction mutation must not leak)", got)
	}
	if got := resp.Models[0].Thinking.Min; got != 100 {
		t.Fatalf("thinking min = %d, want 100", got)
	}
}

func TestModelCatalogLeaderCancelWaiterRetries(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	var once sync.Once
	fetch := func(ctx context.Context, _ pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
		if calls.Add(1) == 1 {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return namedModels("fresh"), nil
	}
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fetch})
	waiterAttached := make(chan struct{}, 1)
	cat.waitHook = func() {
		select {
		case waiterAttached <- struct{}{}:
		default:
		}
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := cat.ModelsForAuth(leaderCtx, catalogAuthReq("auth-1"))
		leaderDone <- err
	}()
	<-started
	type result struct {
		resp pluginapi.ModelResponse
		err  error
	}
	waiterDone := make(chan result, 1)
	go func() {
		resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
		waiterDone <- result{resp, err}
	}()
	// Wait until the waiter has actually attached to the in-flight
	// fetch, then cancel the leader's context: the waiter must
	// retry, not inherit it.
	select {
	case <-waiterAttached:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never attached to the in-flight fetch")
	}
	cancelLeader()
	select {
	case err := <-leaderDone:
		if err == nil {
			t.Fatal("leader err = nil, want the cancellation error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not return")
	}
	select {
	case r := <-waiterDone:
		if r.err != nil {
			t.Fatalf("waiter err = %v, want fresh models after re-election", r.err)
		}
		if got, want := modelIDs(r.resp.Models), []string{"meta/fresh"}; !slices.Equal(got, want) {
			t.Fatalf("waiter served %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("fetch calls = %d, want 2 (cancelled leader + waiter retry)", n)
	}
}

func TestModelCatalogContributorOverrideSurvivesValidate(t *testing.T) {
	fx := &modelsFixture{}
	fx.set(namedModels("base"), nil)
	cat := NewModelCatalog(ModelCatalogConfig{
		Namespace: "meta",
		Fetch:     fx.fetch,
		Allow:     []string{"base"},
		Overrides: map[string]ModelOverride{"base": {ContributorID: "base-contributor"}},
		Validate: func(_ context.Context, _ pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
			return models, nil
		},
	})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/base-contributor"}; !slices.Equal(got, want) {
		t.Fatalf("contributor override served %v, want %v (the approved rewrite must survive the post-Validate glob check)", got, want)
	}
	// The rewritten id is cached as last-good: a later fetch
	// failure serves it rather than an empty list.
	fx.set(nil, errors.New("boom"))
	resp, err = cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/base-contributor"}; !slices.Equal(got, want) {
		t.Fatalf("last-good after contributor fetch served %v, want %v", got, want)
	}
}

func TestModelCatalogValidateAddedModelFacesGlobs(t *testing.T) {
	fx := &modelsFixture{}
	fx.set(namedModels("base"), nil)
	cat := NewModelCatalog(ModelCatalogConfig{
		Namespace: "meta",
		Fetch:     fx.fetch,
		Allow:     []string{"base"},
		Overrides: map[string]ModelOverride{"base": {ContributorID: "base-contributor"}},
		Validate: func(_ context.Context, _ pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
			return append(models, namedModels("invented")...), nil
		},
	})
	resp, err := cat.ModelsForAuth(context.Background(), catalogAuthReq("auth-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"meta/base-contributor"}; !slices.Equal(got, want) {
		t.Fatalf("Validate-invented non-allowed id served %v, want only %v", got, want)
	}
}

func TestModelCatalogLastGoodBounded(t *testing.T) {
	fx := &modelsFixture{}
	fx.set(namedModels("a"), nil)
	cat := NewModelCatalog(ModelCatalogConfig{Namespace: "meta", Fetch: fx.fetch})
	for i := 0; i < 2*maxLastGoodGenerations; i++ {
		if _, err := cat.ModelsForAuth(context.Background(), catalogAuthReq(fmt.Sprintf("auth-%d", i))); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	missing, _ := cat.authKey(catalogAuthReq("auth-0"), "meta")
	present, _ := cat.authKey(catalogAuthReq(fmt.Sprintf("auth-%d", 2*maxLastGoodGenerations-1)), "meta")
	cat.mu.Lock()
	if n := len(cat.lastGood); n != maxLastGoodGenerations {
		cat.mu.Unlock()
		t.Fatalf("last-good entries = %d, want the bound %d", n, maxLastGoodGenerations)
	}
	if n := len(cat.lastGoodOrder); n != len(cat.lastGood) {
		cat.mu.Unlock()
		t.Fatalf("last-good order entries = %d, want %d (order must track the store)", n, len(cat.lastGood))
	}
	if _, ok := cat.lastGood[missing]; ok {
		cat.mu.Unlock()
		t.Fatal("oldest generation auth-0 still retained, want FIFO eviction")
	}
	if _, ok := cat.lastGood[present]; !ok {
		cat.mu.Unlock()
		t.Fatal("newest generation evicted, want only the oldest evicted")
	}
	before := len(cat.lastGood)
	cat.mu.Unlock()
	// Invalidate keeps the order slice tracking the store.
	cat.Invalidate(fmt.Sprintf("auth-%d", 2*maxLastGoodGenerations-1))
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if n := len(cat.lastGood); n != before-1 {
		t.Fatalf("last-good entries after Invalidate = %d, want %d", n, before-1)
	}
	if n := len(cat.lastGoodOrder); n != len(cat.lastGood) {
		t.Fatalf("order entries after Invalidate = %d, want %d", n, len(cat.lastGood))
	}
}
