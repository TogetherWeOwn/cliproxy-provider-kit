package providerkit

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestSSEFinalReviewRegressions(t *testing.T) {
	t.Run("explicit SSE unknown fields", func(t *testing.T) {
		for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
			for _, unknown := range []string{"true", "42", "null", `"hello"`, "[]", `{"a":1}`} {
				wire := unknown + "\ndata: {}\n\n"
				for split := 0; split <= len(wire); split++ {
					t.Run(fmt.Sprintf("%s/%s/%d", strings.ReplaceAll(strings.Trim(path, "/"), "/", "-"), unknown, split), func(t *testing.T) {
						h := &localHostHTTP{status: 200, headers: http.Header{"Content-Type": {"Text/Event-Stream; charset=utf-8"}}, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte(wire[:split])}, {Payload: []byte(wire[split:])}}}
						resp, err := echoExecutor().ExecuteStream(context.Background(), pluginapi.ExecutorRequest{Format: "claude", HTTPClient: h, Metadata: map[string]any{"request_path": path}})
						if err != nil {
							t.Fatal(err)
						}
						got, errs := collectStream(t, resp.Chunks)
						want := []string{"data: {}"}
						if path == "/v1/messages" {
							want = []string{wire}
						}
						if len(errs) != 0 || !reflect.DeepEqual(got, want) {
							t.Fatalf("got %q errs %v, want %q", got, errs, want)
						}
					})
				}
			}
		}
	})

	t.Run("native line-only heartbeat", func(t *testing.T) {
		for _, sep := range []string{"\n", "\r", "\r\n"} {
			t.Run(fmt.Sprintf("%q", sep), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				upstream := make(chan pluginapi.HTTPStreamChunk)
				var hostCtx context.Context
				h := funcHostHTTP{doStream: func(ctx context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
					hostCtx = ctx
					return pluginapi.HTTPStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Chunks: upstream}, nil
				}}
				resp, err := echoExecutor().ExecuteStream(ctx, pluginapi.ExecutorRequest{Format: "claude", HTTPClient: h, Metadata: map[string]any{"request_path": "/v1/messages"}})
				if err != nil {
					t.Fatal(err)
				}
				write := func(s string) {
					t.Helper()
					select {
					case upstream <- pluginapi.HTTPStreamChunk{Payload: []byte(s)}:
					case <-time.After(time.Second):
						t.Fatal("upstream write blocked")
					}
				}
				read := func(want string) {
					t.Helper()
					select {
					case c, ok := <-resp.Chunks:
						if !ok || c.Err != nil || string(c.Payload) != want {
							t.Fatalf("got %q err %v open=%v, want %q", c.Payload, c.Err, ok, want)
						}
					case <-time.After(time.Second):
						t.Fatalf("no timely %q while upstream body stayed open", want)
					}
				}
				// Split both the comment and CRLF. No blank line or EOF is needed.
				write(": heart")
				write("beat" + sep[:1])
				read(": heartbeat\n")
				write(sep[1:] + ": next" + sep)
				read(": next\n")
				write("data: {}" + sep + sep)
				read("data: {}\n\n")
				// A comment inside an unfinished data event cannot overtake it.
				write("data: {\"partial\":true}" + sep + ": pending" + sep)
				close(upstream)
				got, errs := collectStream(t, resp.Chunks)
				if len(got) != 0 || len(errs) != 0 {
					t.Fatalf("truncated native event delivered: %q errs %v", got, errs)
				}
				select {
				case <-hostCtx.Done():
				case <-time.After(time.Second):
					t.Fatal("EOF did not cancel upstream")
				}
			})
		}
	})

	t.Run("headerless JSON still rejected", func(t *testing.T) {
		for _, format := range []string{"openai", "claude", "gemini", "openai-response"} {
			for _, wire := range []string{"true", "42\n", "{}\n", "[]", "null", `"hello"`} {
				h := &localHostHTTP{status: 200, chunks: []pluginapi.HTTPStreamChunk{{Payload: []byte(wire)}}}
				resp, err := echoExecutor().ExecuteStream(context.Background(), pluginapi.ExecutorRequest{Format: format, HTTPClient: h})
				if err != nil {
					t.Fatal(err)
				}
				got, errs := collectStream(t, resp.Chunks)
				if len(got) != 0 || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
					t.Fatalf("format=%s wire=%q: got %q errs %v", format, wire, got, errs)
				}
			}
		}
	})

	t.Run("native heartbeat bounds", func(t *testing.T) {
		first := ":" + strings.Repeat("x", maxStreamEventBytes/2-2) + "\n"
		for _, extra := range []int{0, 1} {
			// Already-forwarded comments still consume the current event's
			// budget. Each individual line fits; only the sum can overflow.
			second := "data: " + strings.Repeat("x", maxStreamEventBytes/2-1+extra) + "\n\n"
			got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(first+second)), streamFraming{native: true}, nil))
			if extra == 0 {
				if len(errs) != 0 || strings.Join(got, "") != first+second {
					t.Fatalf("boundary: events=%d errs %v", len(got), errs)
				}
			} else if strings.Join(got, "") != first || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
				t.Fatalf("overflow: events=%d errs %v", len(got), errs)
			}
		}
		oversized := ":" + strings.Repeat("x", maxStreamEventBytes) + "\n"
		got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(oversized)), streamFraming{native: true}, nil))
		if len(got) != 0 || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
			t.Fatalf("oversized comment: events=%d errs %v", len(got), errs)
		}
	})

	t.Run("native heartbeat cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in := make(chan pluginapi.ExecutorStreamChunk)
		exited := make(chan struct{})
		out := frameSSEChunks(ctx, in, streamFraming{native: true}, func() { close(exited) })
		select {
		case in <- streamBytes(": heartbeat\n"):
		case <-time.After(time.Second):
			t.Fatal("parser did not read heartbeat")
		}
		// No consumer reads the comment. Cancellation must release a blocked send.
		cancel()
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Fatal("cancellation left native heartbeat send blocked")
		}
		for range out {
		}
	})

	t.Run("native comment ordering and EOF", func(t *testing.T) {
		for _, wire := range []string{
			": first\n: second\n",
			": first\ndata: {}\n: inside\nid: 2\n\n: last\n",
			"id: 1\n: inside\ndata: {}\n\n: last\n",
		} {
			for split := 0; split <= len(wire); split++ {
				got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(wire[:split]), streamBytes(wire[split:])), streamFraming{native: true}, nil))
				if len(errs) != 0 || strings.Join(got, "") != wire {
					t.Fatalf("wire=%q split=%d: got %q errs %v", wire, split, got, errs)
				}
			}
		}
		for _, tail := range []string{": unfinished", "data: {}\n", "id: 2\n"} {
			got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(": first\n"+tail)), streamFraming{native: true}, nil))
			if len(errs) != 0 || strings.Join(got, "") != ": first\n" {
				t.Fatalf("tail=%q: got %q errs %v", tail, got, errs)
			}
		}
	})
}
