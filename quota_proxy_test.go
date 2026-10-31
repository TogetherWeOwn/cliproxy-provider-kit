package providerkit

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// proxyRecorder is a plain HTTP forward proxy stand-in: it answers every
// request itself and records the absolute target URL it was asked to fetch.
type proxyRecorder struct {
	*httptest.Server
	mu      sync.Mutex
	targets []string
}

func newProxyRecorder(t *testing.T) *proxyRecorder {
	p := &proxyRecorder{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.targets = append(p.targets, r.URL.String())
		p.mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *proxyRecorder) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func TestAuthProxyURLPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		storage  string
		metadata map[string]any
		host     string
		want     string
	}{
		{"metadata wins", `{"proxy_url":"http://storage:1"}`, map[string]any{"proxy_url": " http://meta:1 "}, "http://host:1", "http://meta:1"},
		{"storage before host", `{"proxy_url":"http://storage:1"}`, nil, "http://host:1", "http://storage:1"},
		{"host fallback", `{}`, map[string]any{"proxy_url": " "}, " http://host:1 ", "http://host:1"},
		{"cleared metadata hides storage", `{"proxy_url":"http://storage:1"}`, map[string]any{"proxy_url": ""}, "http://host:1", "http://host:1"},
		{"explicit direct", `{}`, map[string]any{"proxy_url": "direct"}, "http://host:1", "direct"},
		{"unreadable storage", `not json`, nil, "http://host:1", "http://host:1"},
		{"inherit", `{}`, nil, "", ""},
	} {
		if got := AuthProxyURL([]byte(tc.storage), tc.metadata, tc.host); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestQuotaProxySettings(t *testing.T) {
	request := func(proxy string) *http.Request {
		r, _ := http.NewRequestWithContext(WithProxyURL(context.Background(), proxy), http.MethodGet, "https://quota.invalid/usage", nil)
		return r
	}
	for _, raw := range []string{"direct", "NONE"} {
		if u, err := QuotaProxy(request(raw)); u != nil || err != nil {
			t.Fatalf("%s: %v %v", raw, u, err)
		}
	}
	for _, raw := range []string{"http://p:1", "https://p:1", "socks5://user:pass@p:1", "socks5h://p:1"} {
		if u, err := QuotaProxy(request(raw)); err != nil || u == nil || u.String() != raw {
			t.Fatalf("%s: %v %v", raw, u, err)
		}
	}
	for _, raw := range []string{"ftp://p:1", "p:1", "http://", "://bad"} {
		if _, err := QuotaProxy(request(raw)); err == nil {
			t.Fatalf("%s accepted", raw)
		}
	}
}

func TestCachedQuotaRoutesPollThroughAuthOrHostProxy(t *testing.T) {
	authProxy, hostProxy := newProxyRecorder(t), newProxyRecorder(t)
	client := NewQuotaHTTPClient(nil)
	defer client.CloseIdleConnections()
	q := NewCachedQuota("echo", "Echo", func(ctx context.Context, r pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		if _, err := client.Do(ctx, pluginapi.HTTPRequest{URL: "http://quota.invalid/" + r.AuthID}); err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		return quotaFixture(.5), nil
	})
	host := pluginapi.HostConfigSummary{ProxyURL: hostProxy.URL}
	// proxy_url arrives in metadata exactly as APIKeyAuth.ParseAuth stores it.
	if _, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "with-proxy", Provider: "echo", StorageJSON: []byte(`{"api_key":"one"}`), Metadata: map[string]any{"proxy_url": authProxy.URL}, Host: host}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "global", Provider: "echo", StorageJSON: []byte(`{"api_key":"two"}`), Host: host}); err != nil {
		t.Fatal(err)
	}
	if got := authProxy.seen(); len(got) != 1 || got[0] != "http://quota.invalid/with-proxy" {
		t.Fatalf("auth proxy saw %v", got)
	}
	if got := hostProxy.seen(); len(got) != 1 || got[0] != "http://quota.invalid/global" {
		t.Fatalf("host proxy saw %v", got)
	}
}

func TestQuotaHTTPInvalidProxyFailsClosed(t *testing.T) {
	var direct int
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { direct++ }))
	defer upstream.Close()
	c := NewQuotaHTTPClient(nil)
	defer c.CloseIdleConnections()
	_, err := c.Do(WithProxyURL(context.Background(), "ftp://user:secret-pass@proxy.invalid"), pluginapi.HTTPRequest{URL: upstream.URL})
	var pe *pluginabi.Error
	if err == nil || !errors.As(err, &pe) || pe.Code != "invalid_proxy" || strings.Contains(err.Error(), "secret-pass") {
		t.Fatalf("error %v", err)
	}
	if direct != 0 {
		t.Fatal("invalid proxy fell back to a direct connection")
	}
}

// connectProxy is a minimal HTTPS (CONNECT) forward proxy that counts tunnels.
func connectProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var tunnels atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		tunnels.Add(1)
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { io.Copy(upstream, client); upstream.Close() }()
		go func() { io.Copy(client, upstream); client.Close() }()
	}))
	t.Cleanup(s.Close)
	return s, &tunnels
}

func TestQuotaHTTPConnectionsDoNotCrossProxies(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	proxyA, tunnelsA := connectProxy(t)
	proxyB, tunnelsB := connectProxy(t)
	transport := NewQuotaTransport()
	transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	c := NewQuotaHTTPClient(transport)
	defer c.CloseIdleConnections()
	for _, proxy := range []string{proxyA.URL, proxyB.URL} {
		if _, err := c.Do(WithProxyURL(context.Background(), proxy), pluginapi.HTTPRequest{URL: upstream.URL}); err != nil {
			t.Fatal(err)
		}
	}
	if tunnelsA.Load() != 1 || tunnelsB.Load() != 1 {
		t.Fatalf("tunnels A=%d B=%d; the second poll reused the first proxy's connection", tunnelsA.Load(), tunnelsB.Load())
	}
}
