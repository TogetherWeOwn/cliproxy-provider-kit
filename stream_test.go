package providerkit

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func streamFeed(chunks ...pluginapi.ExecutorStreamChunk) <-chan pluginapi.ExecutorStreamChunk {
	in := make(chan pluginapi.ExecutorStreamChunk, len(chunks))
	for _, c := range chunks {
		in <- c
	}
	close(in)
	return in
}

func streamBytes(s string) pluginapi.ExecutorStreamChunk {
	return pluginapi.ExecutorStreamChunk{Payload: []byte(s)}
}

func collectStream(t *testing.T, out <-chan pluginapi.ExecutorStreamChunk) (payloads []string, errs []error) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case c, ok := <-out:
			if !ok {
				return payloads, errs
			}
			if c.Err != nil {
				errs = append(errs, c.Err)
				continue
			}
			payloads = append(payloads, string(c.Payload))
		case <-timeout:
			t.Fatal("stream did not close")
		}
	}
}

func streamRPC(t *testing.T, err error) *pluginabi.Error {
	t.Helper()
	var rpc *pluginabi.Error
	if !errors.As(err, &rpc) || rpc == nil {
		t.Fatalf("not an RPC error: %v", err)
	}
	return MapError(err)
}

// Several events and a comment in one write, a frame split across writes,
// CRLF line ends, a multi-line data field, [DONE], and a final frame with no
// blank line.
var packedUpstream = []string{
	": upstream comment\n\ndata: {\"a\":1}\n\ndata: {\"b\"",
	":2}\r\n\r\n: upstream comment\r\n\r\ndata: {\"c\":\n",
	"data: 3}\n\nid: 7\nevent: x\ndata:{\"d\":4}\n\ndata: [DONE]\n\ndata: {\"e\":5}",
}

func TestFrameStreamChunksBareForOpenAIChat(t *testing.T) {
	var chunks []pluginapi.ExecutorStreamChunk
	for _, s := range packedUpstream {
		chunks = append(chunks, streamBytes(s))
	}
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(chunks...), true, nil))
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`, `{"d":4}`, `{"e":5}`}
	if len(errs) != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q errs %v", got, errs)
	}
}

func TestFrameStreamChunksDataFramedForTranslatedRoutes(t *testing.T) {
	var chunks []pluginapi.ExecutorStreamChunk
	for _, s := range packedUpstream {
		chunks = append(chunks, streamBytes(s))
	}
	got, _ := collectStream(t, frameStreamChunks(context.Background(), streamFeed(chunks...), false, nil))
	want := []string{`data: {"a":1}`, `data: {"b":2}`, `data: {"c":3}`, `data: {"d":4}`, `data: {"e":5}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestFrameStreamChunksPassesErrorsThrough(t *testing.T) {
	boom := errors.New("boom")
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: {\"a\":1}\n\ndata: {\"trunc"), pluginapi.ExecutorStreamChunk{Payload: []byte("ated\":"), Err: boom}), false, nil))
	// Complete events before the error are delivered; a truncated frame is not.
	if !reflect.DeepEqual(got, []string{`data: {"a":1}`}) || len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Fatalf("got %q errs %v", got, errs)
	}
}

func TestFrameStreamChunksStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan pluginapi.ExecutorStreamChunk)
	out := frameStreamChunks(ctx, in, true, nil)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("chunk after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not closed after cancel")
	}
}

func TestClientWantsBareJSON(t *testing.T) {
	for _, tc := range []struct {
		format string
		path   string
		want   bool
	}{
		{"openai", "/v1/chat/completions", true},
		{"", "/v1/chat/completions", true},
		{"openai", "/v1/completions", true},
		{"openai", "/v1/messages", false},
		{"openai", "/v1/responses", false},
		{"openai", "/v1beta/models/*action", false},
		{"openai", "", false},
		// Only OpenAI output is ever re-framed: translators and native
		// host paths consume framed SSE with event names.
		{"claude", "/v1/chat/completions", false},
		{"claude", "/v1/messages", false},
		{"openai-response", "/v1/chat/completions", false},
	} {
		md := map[string]any{}
		if tc.path != "" {
			md["request_path"] = tc.path
		}
		r := pluginapi.ExecutorRequest{Format: tc.format, Metadata: md}
		if got := clientWantsBareJSON(r); got != tc.want {
			t.Fatalf("%q %q: %v", tc.format, tc.path, got)
		}
	}
}

func TestFrameStreamChunksCRLineEndings(t *testing.T) {
	// Standalone CR is a valid SSE line ending, including a CRLF split
	// across upstream reads: a trailing CR waits for the next byte.
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: {\"a\":1}\r\rdata: {\"b\":2}\r\r")), false, nil))
	if !reflect.DeepEqual(got, []string{`data: {"a":1}`, `data: {"b":2}`}) || len(errs) != 0 {
		t.Fatalf("CR-only: got %q errs %v", got, errs)
	}
	got, errs = collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: {\"a\":1}\r"), streamBytes("\n\r\ndata: {\"b\":2}\r\n\r\n")), false, nil))
	if !reflect.DeepEqual(got, []string{`data: {"a":1}`, `data: {"b":2}`}) || len(errs) != 0 {
		t.Fatalf("split CRLF: got %q errs %v", got, errs)
	}
}

// A CR-delimited event flushed with the feed still open must be delivered
// without waiting for another read or EOF: a trailing CR ends the line at
// once, and only a following LF is swallowed.
func TestFrameStreamChunksCRDelimitedFlushesWithoutMoreInput(t *testing.T) {
	in := make(chan pluginapi.ExecutorStreamChunk, 1)
	in <- streamBytes("data: {\"a\":1}\r\r")
	out := frameStreamChunks(context.Background(), in, false, nil)
	select {
	case c := <-out:
		if c.Err != nil {
			t.Fatalf("err %v", c.Err)
		}
		if got := string(c.Payload); got != `data: {"a":1}` {
			t.Fatalf("got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CR-delimited event stalled with the feed still open")
	}
	close(in)
	for range out {
	}
}

// An empty chunk between CR and LF must not clear the pending LF swallow:
// the three chunks below are one two-line event, not two events.
func TestFrameStreamChunksEmptyChunkBetweenCRAndLF(t *testing.T) {
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(
		streamBytes("data: {\"a\":\r"),
		streamBytes(""),
		streamBytes("\ndata: 1}\n\n"),
	), true, nil))
	if len(errs) != 0 || !reflect.DeepEqual(got, []string{`{"a":1}`}) {
		t.Fatalf("got %q errs %v", got, errs)
	}
}

func TestFrameStreamChunksBoundsEmptyLines(t *testing.T) {
	// Every data line advances the event bound, even empty ones: an event
	// of blank data lines with no terminator must still trip it.
	lines := bytes.Repeat([]byte("data:\n"), (8<<20)+2)
	_, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes(string(lines))), false, nil))
	if len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
		t.Fatalf("errs %v", errs)
	}
}

func TestFrameStreamChunksBoundsAnEvent(t *testing.T) {
	big := make([]byte, maxStreamEventBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: {\"a\":1}\n\ndata: "), pluginapi.ExecutorStreamChunk{Payload: big}), true, nil))
	if !reflect.DeepEqual(got, []string{`{"a":1}`}) || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
		t.Fatalf("got %d events, errs %v", len(got), errs)
	}
}

// An oversized event completed inside one read is refused, not dispatched.
func TestFrameStreamChunksBoundsACompletedEvent(t *testing.T) {
	big := bytes.Repeat([]byte("x"), maxStreamEventBytes)
	payload := append(append([]byte("data: {\"a\":1}\n\ndata: "), big...), []byte("\ndata: y\n\n")...)
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(pluginapi.ExecutorStreamChunk{Payload: payload}), true, nil))
	if !reflect.DeepEqual(got, []string{`{"a":1}`}) || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
		t.Fatalf("got %d events, errs %v", len(got), errs)
	}
}

func TestFrameStreamChunksRefusesMultiLineNonJSON(t *testing.T) {
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: {\"a\":1}\n\ndata: not\ndata: json\n\ndata: {\"b\":2}\n\n")), false, nil))
	if !reflect.DeepEqual(got, []string{`data: {"a":1}`}) || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
		t.Fatalf("got %q errs %v", got, errs)
	}
}

// ExecuteStream re-frames raw upstream bytes: one chunk per data event,
// comments and [DONE] dropped, data-framed without a request_path signal.
func TestPassthroughExecuteStreamFramesUpstreamSSE(t *testing.T) {
	h := &localHostHTTP{status: 200, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte(": comment\n\ndata: {\"a\":1}\n\ndata: {\"b\":2}\n\n")}, {Payload: []byte("data: [DONE]\n\n")}}}
	p := echoExecutor()
	resp, err := p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for c := range resp.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		got = append(got, string(c.Payload))
	}
	want := []string{`data: {"a":1}`, `data: {"b":2}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

// With the chat-completions route in the request metadata the chunks are the
// bare JSON payloads the OpenAI handler writes as "data: %s".
func TestPassthroughExecuteStreamBareForChatRoute(t *testing.T) {
	h := &localHostHTTP{status: 200, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte("data: {\"a\":1}\n\n")}}}
	p := echoExecutor()
	resp, err := p.ExecuteStream(context.Background(), pluginapi.ExecutorRequest{HTTPClient: h, Metadata: map[string]any{"request_path": "/v1/chat/completions"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for c := range resp.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		got = append(got, string(c.Payload))
	}
	if !reflect.DeepEqual(got, []string{`{"a":1}`}) {
		t.Fatalf("got %q", got)
	}
}

func TestFrameStreamChunksCallsOnExit(t *testing.T) {
	// On a framing error the parser must run onExit so the caller can release
	// the upstream feed instead of stranding it.
	called := false
	_, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: not\ndata: json\n\n")), false, func() { called = true }))
	if len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
		t.Fatalf("errs %v", errs)
	}
	if !called {
		t.Fatal("onExit not called after framing error")
	}
	// Normal completion runs it too.
	called = false
	got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: {\"a\":1}\n\n")), false, func() { called = true }))
	if len(errs) != 0 || !reflect.DeepEqual(got, []string{`data: {"a":1}`}) || !called {
		t.Fatalf("got %q errs %v called %v", got, errs, called)
	}
}
