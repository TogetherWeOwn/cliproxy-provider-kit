package providerkit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestHostHTTPScopedRequests(t *testing.T) {
	h := CallerFunc(func(ctx context.Context, m string, raw []byte) ([]byte, error) {
		var b map[string]json.RawMessage
		json.Unmarshal(raw, &b)
		if m == "host.http.operation_open" {
			if string(b["host_callback_id"]) != `"scope-7"` {
				t.Errorf("missing scope: %s", raw)
			}
			return []byte(`{"ok":true,"result":{"operation_id":"op-1"}}`), nil
		}
		if m == "host.http.do" {
			if string(b["operation_id"]) != `"op-1"` || string(b["host_callback_id"]) != `"scope-7"` {
				t.Errorf("unscoped request: %s", raw)
			}
			return []byte(`{"ok":true,"result":{"StatusCode":200,"Body":"b2s="}}`), nil
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	c := NewHostHTTPClient(h, "scope-7")
	out, err := c.Do(context.Background(), pluginapi.HTTPRequest{URL: "https://example.com/echo"})
	if err != nil || out.StatusCode != 200 || string(out.Body) != "ok" {
		t.Fatalf("response %+v err %v", out, err)
	}
}
func TestHostHTTPContextCancelsOperation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	h := CallerFunc(func(ctx context.Context, m string, raw []byte) ([]byte, error) {
		switch m {
		case "host.http.operation_open":
			return []byte(`{"ok":true,"result":{"operation_id":"op"}}`), nil
		case "host.http.do":
			close(started)
			<-canceled
			return nil, errors.New("credential in transport error")
		case "host.http.cancel":
			once.Do(func() { close(canceled) })
			return []byte(`{"ok":true,"result":{}}`), nil
		}
		return nil, errors.New("unexpected method")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := NewHostHTTPClient(h, "scope").Do(ctx, pluginapi.HTTPRequest{}); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach host")
	}
}
func TestHostHTTPStreamReadsAndCloses(t *testing.T) {
	var mu sync.Mutex
	reads, closes := 0, 0
	h := CallerFunc(func(ctx context.Context, m string, raw []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch m {
		case "host.http.operation_open":
			return []byte(`{"ok":true,"result":{"operation_id":"op"}}`), nil
		case "host.http.do_stream":
			return []byte(`{"ok":true,"result":{"status_code":200,"stream_id":"s"}}`), nil
		case "host.http.stream_read":
			reads++
			if reads == 1 {
				return []byte(`{"ok":true,"result":{"payload":"YWJj","done":false}}`), nil
			}
			return []byte(`{"ok":true,"result":{"done":true}}`), nil
		case "host.http.stream_close":
			closes++
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	resp, err := NewHostHTTPClient(h, "scope").DoStream(context.Background(), pluginapi.HTTPRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for chunk := range resp.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		got += string(chunk.Payload)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != "abc" || resp.StatusCode != 200 || reads != 2 || closes != 1 {
		t.Fatalf("stream %q %+v reads %d closes %d", got, resp, reads, closes)
	}
}

func TestHostHTTPFailuresAreRedacted(t *testing.T) {
	if _, err := NewHostHTTPClient(nil, "scope").Do(context.Background(), pluginapi.HTTPRequest{}); MapError(err).Code != "host_unavailable" || MapError(err).HTTPStatus != 503 {
		t.Fatalf("nil host %v", err)
	}
	for name, reply := range map[string]func() ([]byte, error){
		"host error envelope": func() ([]byte, error) {
			return []byte(`{"ok":false,"error":{"code":"x","message":"Bearer example-key rejected","http_status":401}}`), nil
		},
		"callback error":  func() ([]byte, error) { return nil, errors.New("Bearer example-key in transport") },
		"not an envelope": func() ([]byte, error) { return []byte(`example-key`), nil },
		"invalid result": func() ([]byte, error) {
			return []byte(`{"ok":true,"result":{"StatusCode":"example-key"}}`), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := CallerFunc(func(_ context.Context, m string, _ []byte) ([]byte, error) {
				switch m {
				case "host.http.operation_open":
					return []byte(`{"ok":true,"result":{"operation_id":"op"}}`), nil
				case "host.http.do":
					return reply()
				}
				return []byte(`{"ok":true,"result":{}}`), nil
			})
			out, err := NewHostHTTPClient(h, "scope").Do(context.Background(), pluginapi.HTTPRequest{})
			e := MapError(err)
			if e.Code != "host_error" || e.HTTPStatus != 502 || strings.Contains(e.Message, "example-key") || out.StatusCode != 0 {
				t.Fatalf("mapped %+v response %+v", e, out)
			}
		})
	}
}
func TestHostHTTPOperationLifecycle(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	opID := "op-1"
	h := CallerFunc(func(_ context.Context, m string, raw []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, m+" "+string(raw))
		switch m {
		case "host.http.operation_open":
			return []byte(`{"ok":true,"result":{"operation_id":"` + opID + `"}}`), nil
		case "host.http.do":
			return []byte(`{"ok":true,"result":{"StatusCode":200}}`), nil
		case "host.http.do_stream":
			return []byte(`{"ok":true,"result":{"status_code":200}}`), nil
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	})
	reset := func() []string { mu.Lock(); defer mu.Unlock(); c := calls; calls = nil; return c }
	c := NewHostHTTPClient(h, "scope-7")
	if _, err := c.Do(context.Background(), pluginapi.HTTPRequest{}); err != nil {
		t.Fatal(err)
	}
	// Every operation is released exactly once, in its callback scope, after the request.
	got := reset()
	if len(got) != 3 || !strings.HasPrefix(got[2], `host.http.cancel `) || !strings.Contains(got[2], `"operation_id":"op-1"`) || !strings.Contains(got[2], `"host_callback_id":"scope-7"`) {
		t.Fatalf("calls %q", got)
	}
	if _, err := c.DoStream(context.Background(), pluginapi.HTTPRequest{}); MapError(err).Code != "host_error" {
		t.Fatalf("missing stream id %v", err)
	}
	if got = reset(); len(got) != 3 || !strings.HasPrefix(got[2], "host.http.cancel ") {
		t.Fatalf("stream setup failure leaked the operation: %q", got)
	}
	opID = ""
	if _, err := c.Do(context.Background(), pluginapi.HTTPRequest{}); MapError(err).Code != "host_error" {
		t.Fatalf("missing operation id %v", err)
	}
	if got = reset(); len(got) != 1 {
		t.Fatalf("request sent without an operation: %q", got)
	}
}
func TestHostHTTPStreamErrorsAreRedactedAndClosed(t *testing.T) {
	for name, read := range map[string][]byte{
		"upstream error": []byte(`{"ok":true,"result":{"payload":"YWJj","error":"read tcp: Bearer example-key"}}`),
		"host failure":   []byte(`{"ok":false,"error":{"code":"x","message":"example-key"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			var closes atomic.Int32
			h := CallerFunc(func(_ context.Context, m string, raw []byte) ([]byte, error) {
				switch m {
				case "host.http.operation_open":
					return []byte(`{"ok":true,"result":{"operation_id":"op"}}`), nil
				case "host.http.do_stream":
					return []byte(`{"ok":true,"result":{"status_code":200,"stream_id":"s"}}`), nil
				case "host.http.stream_read":
					return read, nil
				case "host.http.stream_close":
					if !strings.Contains(string(raw), `"stream_id":"s"`) {
						t.Errorf("close for wrong stream: %s", raw)
					}
					closes.Add(1)
				}
				return []byte(`{"ok":true,"result":{}}`), nil
			})
			resp, err := NewHostHTTPClient(h, "scope").DoStream(context.Background(), pluginapi.HTTPRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var last pluginapi.HTTPStreamChunk
			for chunk := range resp.Chunks {
				last = chunk
			}
			if last.Err == nil || strings.Contains(MapError(last.Err).Message, "example-key") || closes.Load() != 1 {
				t.Fatalf("terminal chunk %+v closes %d", last, closes.Load())
			}
		})
	}
}
