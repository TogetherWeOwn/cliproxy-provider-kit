package providerkit

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestSSEMediaType(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", true},
		{"text/event-stream", true},
		{"Text/Event-Stream; charset=utf-8", true},
		{`application/json; note="text/event-stream"`, false},
		{"application/text/event-stream", false},
		{"text/event-stream-extra", false},
		{"text/event-stream; invalid", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if got := isSSEStream(http.Header{"Content-Type": {tc.value}}); got != tc.want {
				t.Fatalf("isSSEStream = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSSELeadingBOMAcrossReads(t *testing.T) {
	wire := "\xef\xbb\xbfdata: {\"a\":1}\n\ndata: {\"b\":2}\n\n"
	for split := 0; split <= len(wire); split++ {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes(wire[:split]), streamBytes(""), streamBytes(wire[split:])), true, nil))
			if len(errs) != 0 || !reflect.DeepEqual(got, []string{`{"a":1}`, `{"b":2}`}) {
				t.Fatalf("got %q errs %v", got, errs)
			}
		})
	}
	// Only the stream-leading BOM is special; a later BOM is data.
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: \"\xef\xbb\xbf\"\n\n")), true, nil))
	if len(errs) != 0 || !reflect.DeepEqual(got, []string{"\"\xef\xbb\xbf\""}) {
		t.Fatalf("got %q errs %v", got, errs)
	}
}

func TestSSESizeBoundaryIndependentOfReads(t *testing.T) {
	for _, extra := range []int{0, 1} {
		// The limit includes the data-line separator, not the SSE field prefix.
		value := "\"" + string(bytes.Repeat([]byte("x"), maxStreamEventBytes-3+extra)) + "\""
		wire := "data: " + value + "\n\n"
		for _, split := range []int{0, len(wire) - 2, len(wire) - 1, len(wire)} {
			t.Run(fmt.Sprintf("extra=%d/split=%d", extra, split), func(t *testing.T) {
				got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes(wire[:split]), streamBytes(wire[split:])), true, nil))
				if extra == 0 {
					if len(errs) != 0 || len(got) != 1 || got[0] != value {
						t.Fatalf("events %d errs %v", len(got), errs)
					}
				} else if len(got) != 0 || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
					t.Fatalf("events %d errs %v", len(got), errs)
				}
			})
		}
	}
}

func TestPassthroughHeaderlessJSONIsNotSilentlyDropped(t *testing.T) {
	for _, wire := range []string{`{"a":1}`, "{\"a\":1}\n{\"b\":2}\n", `[]`, `null`, `true`, `42`, `"hello"`} {
		h := &localHostHTTP{status: 200, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte(wire)}}}
		resp, err := echoExecutor().ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h})
		if err != nil {
			t.Fatal(err)
		}
		got, errs := collectStream(t, resp.Chunks)
		// Headerless streams retain the documented SSE contract. Unsupported
		// bare JSON must fail explicitly rather than look like an empty success.
		if len(got) != 0 || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
			t.Fatalf("events %d errs %v", len(got), errs)
		}
	}
}

func TestPassthroughFramesNonOpenAISSE(t *testing.T) {
	first := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`
	last := `{"type":"message_stop"}`
	wire := "event: content_block_delta\ndata: " + first + "\n\nevent: message_stop\ndata: " + last + "\n\n"
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, split := range []int{0, 5, len(wire) / 2, len(wire)} {
			t.Run(fmt.Sprintf("%s/%d", path, split), func(t *testing.T) {
				h := &localHostHTTP{status: 200, headers: http.Header{"Content-Type": {"text/event-stream"}}, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte(wire[:split])}, {Payload: []byte(wire[split:])}}}
				resp, err := echoExecutor().ExecuteStream(context.Background(), pluginapi.ExecutorRequest{Format: "claude", Metadata: map[string]any{"request_path": path}, HTTPClient: h})
				if err != nil {
					t.Fatal(err)
				}
				got, errs := collectStream(t, resp.Chunks)
				want := []string{"data: " + first, "data: " + last}
				if path == "/v1/messages" {
					want = []string{"event: content_block_delta\ndata: " + first + "\n\n", "event: message_stop\ndata: " + last + "\n\n"}
				}
				if len(errs) != 0 || !reflect.DeepEqual(got, want) {
					t.Fatalf("got %q errs %v", got, errs)
				}
			})
		}
	}
}

func TestPassthroughFramingErrorCancelsHostOperation(t *testing.T) {
	var hostCtx context.Context
	upstream := make(chan pluginapi.HTTPStreamChunk, 1)
	upstream <- pluginapi.HTTPStreamChunk{Payload: []byte("data: not\ndata: json\n\n")}
	h := funcHostHTTP{doStream: func(ctx context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
		hostCtx = ctx
		return pluginapi.HTTPStreamResponse{StatusCode: 200, Chunks: upstream}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := echoExecutor().ExecuteStream(ctx, pluginapi.ExecutorRequest{HTTPClient: h})
	if err != nil {
		t.Fatal(err)
	}
	_, errs := collectStream(t, resp.Chunks)
	if len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
		t.Fatalf("errs %v", errs)
	}
	select {
	case <-hostCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("framing error left upstream operation alive")
	}
}

func TestClientWantsNativeSSE(t *testing.T) {
	for _, tc := range []struct {
		format, path string
		want         bool
	}{
		{"openai", "/v1/chat/completions", false},
		{"openai", "/v1/messages", false},
		{"claude", "/v1/chat/completions", false},
		{"claude", "/v1/completions", false},
		{"claude", "/v1/messages/", true},
		{"claude", "/v1/responses", false},
		{"claude", "/v1beta/models/model:streamGenerateContent", false},
		{"claude", "", true},
		{"openai-response", "/v1/responses", true},
		{"openai-response", "/v1/messages", false},
		{"gemini", "/v1beta/models/model:streamGenerateContent", false},
		{"gemini", "/v1/messages", false},
		{"gemini", "", true},
	} {
		r := pluginapi.ExecutorRequest{Format: tc.format, Metadata: map[string]any{"request_path": tc.path}}
		if got := clientWantsNativeSSE(r); got != tc.want {
			t.Errorf("%s %s: got %v want %v", tc.format, tc.path, got, tc.want)
		}
	}
}

func benchmarkFragmentedSSE(b *testing.B, size int) {
	wire := []byte("data: \"" + string(bytes.Repeat([]byte("x"), size)) + "\"\n\n")
	var chunks []pluginapi.ExecutorStreamChunk
	for len(wire) > 0 {
		n := min(len(wire), 256)
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: wire[:n]})
		wire = wire[n:]
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for c := range frameStreamChunks(context.Background(), streamFeed(chunks...), true, nil) {
			if c.Err != nil {
				b.Fatal(c.Err)
			}
		}
	}
}

func BenchmarkFragmentedSSE(b *testing.B) {
	for _, size := range []int{16 << 10, 64 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) { benchmarkFragmentedSSE(b, size) })
	}
}

func TestSSEPartialLineAllocationBound(t *testing.T) {
	// This is an allocation, not timing, guard. Copying a retained prefix on
	// every read costs >16 MiB for this 64 KiB fixture; geometric buffering
	// and final framing fit comfortably inside 2 MiB.
	result := testing.Benchmark(func(b *testing.B) { benchmarkFragmentedSSE(b, 64<<10) })
	if got := result.AllocedBytesPerOp(); got > 2<<20 {
		t.Fatalf("partial-line buffering allocated %d bytes for 64 KiB", got)
	}
}
