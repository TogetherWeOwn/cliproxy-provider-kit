package providerkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func quotaFixture(value float64) pluginapi.QuotaFetchResponse {
	return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "Credits", Buckets: []pluginapi.QuotaBucket{{Window: "weekly", RemainingFraction: value}}}}}
}
func TestQuotaCacheTTLPerAuthAndCopies(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var calls int
	q := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls++
		return quotaFixture(.75), nil
	})
	q.now = func() time.Time { return now }
	a := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo", StorageJSON: []byte(`{"api_key":"example-key"}`)}
	one, err := q.FetchQuota(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	one.Groups[0].Buckets[0].RemainingFraction = 0
	two, err := q.FetchQuota(context.Background(), a)
	if err != nil || two.Groups[0].Buckets[0].RemainingFraction != .75 || calls != 1 {
		t.Fatalf("cache %v calls %d err %v", two, calls, err)
	}
	b := a
	b.AuthID = "b"
	q.FetchQuota(context.Background(), b)
	if calls != 2 {
		t.Fatal("cross-account cache")
	}
	now = now.Add(59 * time.Second)
	q.FetchQuota(context.Background(), a)
	if calls != 2 {
		t.Fatal("early expiry")
	}
	now = now.Add(time.Second)
	q.FetchQuota(context.Background(), a)
	if calls != 3 {
		t.Fatal("cache not expired at 60 seconds")
	}
	a.Metadata = map[string]any{"api_key": "replacement-key"}
	q.FetchQuota(context.Background(), a)
	if calls != 4 {
		t.Fatal("credential change reused quota")
	}
	q.Invalidate("a")
	q.FetchQuota(context.Background(), a)
	if calls != 5 {
		t.Fatal("invalidation failed")
	}
	desc, _ := q.DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if desc.SupportsReset || len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != "echo" {
		t.Fatal(desc)
	}
	if _, err = q.ResetQuota(context.Background(), pluginapi.QuotaResetRequest{}); err == nil {
		t.Fatal("fake reset")
	}
}
func TestQuotaHeaderObservationMatchesEnrichedFetch(t *testing.T) {
	var calls int
	q := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls++
		return quotaFixture(.5), nil
	})
	// UsagePlugin only has auth identity, not persisted storage or host metadata.
	usage := pluginapi.UsageRecord{Provider: "echo", AuthID: "a", AuthIndex: "index-a"}
	observation := quotaFixture(.9)
	if err := q.Store(pluginapi.QuotaFetchRequest{AuthID: usage.AuthID, Provider: usage.Provider}, observation); err != nil {
		t.Fatal(err)
	}
	observation.Groups[0].Buckets[0].RemainingFraction = 0
	// The host reads physical file bytes and enriches FetchQuota independently.
	req := pluginapi.QuotaFetchRequest{
		AuthID: "a", AuthIndex: "index-a", Provider: "echo",
		StorageJSON: []byte("{\n  \"type\": \"echo\", \"api_key\": \"example-key\", \"label\": \"Example\"\n}\n"),
		Metadata:    map[string]any{"label": "Example"},
		Attributes:  map[string]string{"path": "example.json"},
	}
	for i := 0; i < 2; i++ {
		resp, err := q.FetchQuota(context.Background(), req)
		if err != nil || len(resp.Groups) != 1 || resp.Groups[0].Buckets[0].RemainingFraction != .9 || calls != 0 {
			t.Fatalf("header observation missed: response %+v, calls %d, error %v", resp, calls, err)
		}
		resp.Groups[0].Buckets[0].RemainingFraction = 0
	}
}

func TestQuotaHeaderObservationTTLAndInvalidation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var calls int
	q := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls++
		return quotaFixture(.5), nil
	})
	q.now = func() time.Time { return now }
	req := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo", StorageJSON: []byte(`{"api_key":"example-key"}`)}
	// A fresh header must supersede a successful, credential-keyed poll too.
	if _, err := q.FetchQuota(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	store := func() {
		t.Helper()
		if err := q.Store(pluginapi.QuotaFetchRequest{AuthID: "a"}, quotaFixture(.9)); err != nil {
			t.Fatal(err)
		}
	}
	store()
	check := func(r pluginapi.QuotaFetchRequest, want float64, wantCalls int) {
		t.Helper()
		resp, err := q.FetchQuota(context.Background(), r)
		if err != nil || len(resp.Groups) != 1 || resp.Groups[0].Buckets[0].RemainingFraction != want || calls != wantCalls {
			t.Fatalf("response %+v, calls %d (want %d), error %v", resp, calls, wantCalls, err)
		}
	}
	check(req, .9, 1)
	other := req
	other.AuthID = "b"
	check(other, .5, 2)
	now = now.Add(59 * time.Second)
	check(req, .9, 2)
	now = now.Add(time.Second)
	check(req, .5, 3)
	store()
	q.Invalidate("a")
	check(req, .5, 4)
}

func TestQuotaHeaderObservationSurvivesOlderPoll(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	q := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls.Add(1)
		close(started)
		<-release
		return quotaFixture(.5), nil
	})
	req := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo", StorageJSON: []byte(`{"api_key":"example-key"}`), Metadata: map[string]any{"label": "Example"}}
	done := make(chan error, 1)
	go func() {
		_, err := q.FetchQuota(context.Background(), req)
		done <- err
	}()
	<-started
	err := q.Store(pluginapi.QuotaFetchRequest{AuthID: "a"}, quotaFixture(.9))
	close(release)
	if pollErr := <-done; pollErr != nil {
		t.Fatal(pollErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	resp, err := q.FetchQuota(context.Background(), req)
	if err != nil || len(resp.Groups) != 1 || resp.Groups[0].Buckets[0].RemainingFraction != .9 || calls.Load() != 1 {
		t.Fatalf("older poll replaced header: response %+v, calls %d, error %v", resp, calls.Load(), err)
	}
}

func TestQuotaConcurrentFetchAndHeaderUpdate(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	q := NewCachedQuota("echo", "Echo", func(ctx context.Context, r pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return quotaFixture(.5), nil
		case <-ctx.Done():
			return pluginapi.QuotaFetchResponse{}, ctx.Err()
		}
	})
	req := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo"}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.FetchQuota(context.Background(), req); err != nil {
				t.Error(err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate fetches %d", calls.Load())
	}
	if err := q.Store(req, quotaFixture(.9)); err != nil {
		t.Fatal(err)
	}
	resp, err := q.FetchQuota(context.Background(), req)
	if err != nil || resp.Groups[0].Buckets[0].RemainingFraction != .9 || calls.Load() != 1 {
		t.Fatal(resp, err)
	}
}
func TestQuotaErrorsAreNotCached(t *testing.T) {
	var calls int
	q := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls++
		return pluginapi.QuotaFetchResponse{}, errors.New("poll failed")
	})
	req := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo"}
	q.FetchQuota(context.Background(), req)
	q.FetchQuota(context.Background(), req)
	if calls != 2 {
		t.Fatal("cached error")
	}
}
func TestQuotaHTTPBoundedNoRedirectAndRedacted(t *testing.T) {
	var redirected atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"remaining":0.5}`))
		case "/redirect":
			http.Redirect(w, r, "/target", 302)
		case "/target":
			redirected.Add(1)
		case "/large":
			w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
		case "/unauthorized":
			w.WriteHeader(401)
			w.Write([]byte("private-body-example-key"))
		}
	}))
	defer s.Close()
	c := NewQuotaHTTPClient(nil)
	resp, err := c.Do(context.Background(), pluginapi.HTTPRequest{URL: s.URL + "/ok", Headers: http.Header{"Authorization": {"Bearer example-key"}}})
	if err != nil || string(resp.Body) != `{"remaining":0.5}` {
		t.Fatal(resp, err)
	}
	for _, path := range []string{"/redirect", "/large", "/unauthorized"} {
		resp, err = c.Do(context.Background(), pluginapi.HTTPRequest{URL: s.URL + path})
		if err == nil || strings.Contains(err.Error(), "example-key") || strings.Contains(err.Error(), "private-body") || len(resp.Body) != 0 {
			t.Fatalf("unsafe %s response %v error %v", path, resp, err)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("followed redirect")
	}
	if c.client.Timeout != 15*time.Second {
		t.Fatalf("timeout %v", c.client.Timeout)
	}
}
func TestQuotaHTTPTransportErrorRedactionAndCancellation(t *testing.T) {
	c := NewQuotaHTTPClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("example-key leaked in transport")
	}))
	_, err := c.Do(context.Background(), pluginapi.HTTPRequest{URL: "https://example.com/quota?credential=example-key"})
	if err == nil || strings.Contains(err.Error(), "example-key") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Do(ctx, pluginapi.HTTPRequest{URL: "https://example.com/quota"})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
