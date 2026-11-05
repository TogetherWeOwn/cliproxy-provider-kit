package providerkit

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPassthroughSSEDone(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, ending := range []string{"\n", "\r", "\r\n"} {
			wire := "data: {\"a\":1}" + ending + ending + "data: [DONE]" + ending + ending
			for _, split := range []int{0, len(wire) - 4, len(wire) - 1, len(wire)} {
				t.Run(fmt.Sprintf("%s/%q/%d", path, ending, split), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					// The transport remains open after its terminal event. No later
					// read or EOF should be needed to release it and finish the host stream.
					upstream := make(chan pluginapi.HTTPStreamChunk, 2)
					upstream <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[:split])}
					upstream <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[split:])}
					var hostCtx context.Context
					h := funcHostHTTP{doStream: func(ctx context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
						hostCtx = ctx
						return pluginapi.HTTPStreamResponse{StatusCode: 200, Chunks: upstream}, nil
					}}
					resp, err := echoExecutor().ExecuteStream(ctx, pluginapi.ExecutorRequest{Format: "openai", HTTPClient: h, Metadata: map[string]any{"request_path": path}})
					if err != nil {
						t.Fatal(err)
					}
					want := `{"a":1}`
					if path == "/v1/messages" {
						want = "data: " + want
					}
					select {
					case chunk := <-resp.Chunks:
						if chunk.Err != nil || string(chunk.Payload) != want {
							t.Fatalf("first chunk = %q, err %v", chunk.Payload, chunk.Err)
						}
					case <-time.After(time.Second):
						t.Fatal("missing data before terminator")
					}
					select {
					case chunk, ok := <-resp.Chunks:
						if ok {
							t.Fatalf("chunk after terminator: %+v", chunk)
						}
					case <-time.After(time.Second):
						t.Fatal("terminal event did not close the stream while the upstream body stayed open")
					}
					select {
					case <-hostCtx.Done():
					case <-time.After(time.Second):
						t.Fatal("terminal event did not cancel the upstream operation")
					}
				})
			}
		}
	}

	t.Run("ignore trailing bytes", func(t *testing.T) {
		got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes("data: [DONE]\n\ndata: {\"late\":true}\n\n")), true, nil))
		if len(got) != 0 || len(errs) != 0 {
			t.Fatalf("got %q errs %v", got, errs)
		}
	})
	t.Run("non-OpenAI sentinel is data", func(t *testing.T) {
		got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes("data: [DONE]\n\ndata: {}\n\n")), streamFraming{}, nil))
		if len(errs) != 0 || !reflect.DeepEqual(got, []string{"data: [DONE]", "data: {}"}) {
			t.Fatalf("got %q errs %v", got, errs)
		}
	})
}
