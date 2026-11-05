// Geminiprobe is an offline host-compatibility fixture, not a provider.
package main

import (
	"context"
	"net/http"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func main() {}
func init() {
	if err := cabi.Export(func(host providerkit.Caller) (*providerkit.Runtime, error) {
		return providerkit.New(providerkit.Definition{
			Metadata: pluginapi.Metadata{Name: "geminiprobe", Version: "0.1.0", Author: "Example Organization", GitHubRepository: "https://github.com/example/geminiprobe"},
			Capabilities: pluginapi.Capabilities{
				Executor: fixtureExecutor{Passthrough: providerkit.Passthrough{Provider: "geminiprobe", BuildRequest: func(_ context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
					return pluginapi.HTTPRequest{URL: "https://example.com/stream?alt=" + r.Alt}, nil
				}}},
				ExecutorInputFormats: []string{"gemini"}, ExecutorOutputFormats: []string{"gemini"},
			},
		}, host)
	}); err != nil {
		panic(err)
	}
}

type fixtureExecutor struct{ providerkit.Passthrough }

func (e fixtureExecutor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	r.HTTPClient = fixtureHTTP{}
	return e.Passthrough.ExecuteStream(ctx, r)
}

type fixtureHTTP struct{}

func (fixtureHTTP) Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	panic("stream-only fixture")
}
func (fixtureHTTP) DoStream(_ context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	first := `{"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"},"index":0}]}`
	last := `{"candidates":[{"content":{"parts":[{"text":" world"}],"role":"model"},"index":0,"finishReason":"STOP"}]}`
	wire := ": heartbeat\n\ndata: " + first + "\n\ndata: {\ndata: " + last[1:] + "\n\n"
	contentType := "text/event-stream"
	if r.URL == "https://example.com/stream?alt=json" {
		wire = "[" + first + "," + last + "]"
		contentType = "application/json"
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, 3)
	chunks <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[:5])}
	chunks <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[5 : len(wire)/2])}
	chunks <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[len(wire)/2:])}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {contentType}}, Chunks: chunks}, nil
}
