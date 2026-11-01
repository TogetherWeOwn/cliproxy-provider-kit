package providerkit

import (
	"context"
	"errors"
	"io"
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

func TestQuotaIdentityIsRequiredAndScoped(t *testing.T) {
	var calls int
	fetch := func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls++
		return quotaFixture(.5), nil
	}
	q := NewCachedQuota("echo", "Echo", fetch)
	for name, r := range map[string]pluginapi.QuotaFetchRequest{
		"no identity":    {Provider: "echo"},
		"other provider": {AuthID: "a", Provider: "other"},
	} {
		if _, err := q.FetchQuota(context.Background(), r); MapError(err).Code != "invalid_auth" {
			t.Fatalf("%s: fetch %v", name, err)
		}
		if err := q.Store(r, quotaFixture(.9)); MapError(err).Code != "invalid_auth" {
			t.Fatalf("%s: store %v", name, err)
		}
	}
	if _, err := NewCachedQuota("", "Echo", fetch).FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "a"}); err == nil {
		t.Fatal("unnamed quota provider served a fetch")
	}
	if calls != 0 {
		t.Fatalf("rejected requests reached the provider: %d", calls)
	}
	// A rejected cross-provider Store must not poison this provider's account.
	if resp, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo"}); err != nil || resp.Groups[0].Buckets[0].RemainingFraction != .5 || calls != 1 {
		t.Fatalf("cross-provider observation leaked: %+v calls %d %v", resp, calls, err)
	}
	// AuthIndex identifies the account when AuthID is absent, for both observations and invalidation.
	if err := q.Store(pluginapi.QuotaFetchRequest{AuthIndex: "index-b"}, quotaFixture(.9)); err != nil {
		t.Fatal(err)
	}
	byIndex := pluginapi.QuotaFetchRequest{AuthIndex: "index-b", Provider: "echo"}
	if resp, err := q.FetchQuota(context.Background(), byIndex); err != nil || resp.Groups[0].Buckets[0].RemainingFraction != .9 || calls != 1 {
		t.Fatalf("index observation missed: %+v calls %d %v", resp, calls, err)
	}
	q.Invalidate("index-b")
	if resp, err := q.FetchQuota(context.Background(), byIndex); err != nil || resp.Groups[0].Buckets[0].RemainingFraction != .5 || calls != 2 {
		t.Fatalf("index invalidation failed: %+v calls %d %v", resp, calls, err)
	}
}

func TestQuotaCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var attempts, calls atomic.Int32
	q := NewCachedQuota("echo", "Echo", func(ctx context.Context, _ pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		attempts.Add(1)
		if err := ctx.Err(); err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return quotaFixture(.5), nil
	})
	req := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo"}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.FetchQuota(canceled, req); !errors.Is(err, context.Canceled) || attempts.Load() != 0 {
		t.Fatalf("canceled fetch %v reached the provider %d times", err, attempts.Load())
	}
	leader := make(chan error, 1)
	go func() { _, err := q.FetchQuota(context.Background(), req); leader <- err }()
	<-started
	// A waiter on another caller's in-flight poll must still honor its own context.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	waiter := make(chan error, 1)
	go func() { _, err := q.FetchQuota(ctx, req); waiter <- err }()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ignored its context while another poll was in flight")
	}
	close(release)
	if err := <-leader; err != nil || calls.Load() != 1 {
		t.Fatalf("leader %v calls %d", err, calls.Load())
	}
}

func TestQuotaPanicAndMissingFetchAreNotCached(t *testing.T) {
	var calls int
	q := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls++
		if calls == 1 {
			panic("example-key")
		}
		return quotaFixture(.5), nil
	})
	req := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo"}
	_, err := q.FetchQuota(context.Background(), req)
	if e := MapError(err); e.Code != "plugin_panic" || strings.Contains(e.Message, "example-key") {
		t.Fatalf("panic %+v", e)
	}
	if resp, err := q.FetchQuota(context.Background(), req); err != nil || resp.Groups[0].Buckets[0].RemainingFraction != .5 || calls != 2 {
		t.Fatalf("panic result cached: %+v calls %d %v", resp, calls, err)
	}
	_, err = NewCachedQuota("echo", "Echo", nil).FetchQuota(context.Background(), req)
	if e := MapError(err); e.Code != "unsupported_method" || e.HTTPStatus != 501 {
		t.Fatalf("nil fetch %+v", e)
	}
}

func TestQuotaHTTPMethodAndStatusClassification(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/headers":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer example-key" {
				w.WriteHeader(400)
			}
		case "/limited":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		case "/down":
			w.WriteHeader(503)
		case "/forbidden":
			w.WriteHeader(403)
		case "/redirect":
			w.Header().Set("Retry-After", "9")
			http.Redirect(w, r, "/target", 307)
		case "/exact":
			w.Write([]byte(strings.Repeat("x", 1<<20)))
		}
	}))
	defer s.Close()
	c := NewQuotaHTTPClient(nil)
	defer c.CloseIdleConnections()
	// The helper is read-only so it can never become an inference or mutation path.
	for _, method := range []string{http.MethodPost, http.MethodDelete, "get"} {
		_, err := c.Do(context.Background(), pluginapi.HTTPRequest{Method: method, URL: s.URL + "/headers", Body: []byte(`{}`)})
		if e := MapError(err); e.Code != "invalid_request" || e.HTTPStatus != 400 {
			t.Fatalf("%s accepted: %+v", method, e)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("rejected method reached the endpoint %d times", hits.Load())
	}
	if resp, err := c.Do(context.Background(), pluginapi.HTTPRequest{Method: http.MethodGet, URL: s.URL + "/headers", Headers: http.Header{"Authorization": {"Bearer example-key"}}}); err != nil || resp.StatusCode != 200 {
		t.Fatalf("headers %+v %v", resp, err)
	}
	for _, tc := range []struct {
		path      string
		code      string
		status    int
		retryable bool
		after     int
	}{
		{"/limited", "quota_http_error", 429, true, 7},
		{"/down", "quota_http_error", 503, true, 0},
		{"/forbidden", "quota_http_error", 403, false, 0},
		{"/redirect", "quota_redirect", 502, false, 9},
	} {
		_, err := c.Do(context.Background(), pluginapi.HTTPRequest{URL: s.URL + tc.path})
		e := MapError(err)
		if e.Code != tc.code || e.HTTPStatus != tc.status || e.Retryable != tc.retryable || e.RetryAfterSeconds != tc.after {
			t.Fatalf("%s mapped %+v", tc.path, e)
		}
	}
	if resp, err := c.Do(context.Background(), pluginapi.HTTPRequest{URL: s.URL + "/exact"}); err != nil || len(resp.Body) != 1<<20 {
		t.Fatalf("1 MiB body rejected: %d %v", len(resp.Body), err)
	}
	if _, err := c.Do(context.Background(), pluginapi.HTTPRequest{URL: "://example-key"}); err == nil || strings.Contains(err.Error(), "example-key") {
		t.Fatalf("invalid URL %v", err)
	}
}

// A server that offers HTTP/2 over TLS (as api.cloudflare.com does) must still
// be read over HTTP/1.1, not misread as a malformed response.
func TestQuotaTransportSpeaksHTTP1ToHTTP2Servers(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := NewQuotaTransport()
	transport.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, server.URL, nil)
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.ProtoMajor != 1 || string(body) != "HTTP/1.1" {
			t.Fatalf("%s: proto %s, server saw %s", method, resp.Proto, body)
		}
	}
}

// TestQuotaWindowLabel pins the canonical panel labels: short window-style
// text the panel shows as the row label.
func TestQuotaWindowLabel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"5h", "5-hour limit"},
		{"five-hour", "5-hour limit"},
		{"weekly", "Weekly limit"},
		{"week", "Weekly limit"},
		{"monthly", "Monthly limit"},
		{"daily", "Daily limit"},
		{"per-minute", "per-minute"},
	} {
		if got := QuotaWindowLabel(tc.in); got != tc.want {
			t.Errorf("QuotaWindowLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNewQuotaBucket proves card rows carry the canonical label and never a
// Description: the panel would show a Description as the row label.
func TestNewQuotaBucket(t *testing.T) {
	b := NewQuotaBucket("weekly", 0.5, "2026-11-02T00:00:00Z")
	if b.Window != "Weekly limit" || b.RemainingFraction != 0.5 || b.ResetTime != "2026-11-02T00:00:00Z" {
		t.Fatalf("wrong bucket: %+v", b)
	}
	if b.Description != "" {
		t.Fatalf("bucket must not set Description: %+v", b)
	}
}

// TestQuotaWindowLabelCanonical pins the canonical spellings plus the
// verbatim-passthrough rule: unlisted input (including typos and the empty
// string) renders trimmed, never remapped.
func TestQuotaWindowLabelCanonical(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"5H", "5-hour limit"},
		{"Weekly", "Weekly limit"},
		{"MONTHLY", "Monthly limit"},
		{" per-minute ", "per-minute"},
		{"weekly limit", "Weekly limit"},
		{"weakly", "weakly"},
		{"", ""},
	} {
		if got := QuotaWindowLabel(tc.in); got != tc.want {
			t.Errorf("QuotaWindowLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
