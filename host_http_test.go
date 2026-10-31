package providerkit

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
