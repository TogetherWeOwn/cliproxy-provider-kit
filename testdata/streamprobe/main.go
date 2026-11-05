// Streamprobe is an offline host-compatibility fixture, not a provider.
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
			Metadata: pluginapi.Metadata{Name: "streamprobe", Version: "0.1.0", Author: "Example Organization", GitHubRepository: "https://github.com/example/streamprobe"},
			Capabilities: pluginapi.Capabilities{
				Executor: fixtureExecutor{Passthrough: providerkit.Passthrough{Provider: "streamprobe", BuildRequest: func(context.Context, pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
					return pluginapi.HTTPRequest{}, nil
				}}},
				ExecutorInputFormats: []string{"claude"}, ExecutorOutputFormats: []string{"claude"}, ManagementAPI: fixtureManagement{},
			},
		}, host)
	}); err != nil {
		panic(err)
	}
}

type fixtureExecutor struct{ providerkit.Passthrough }

func (e fixtureExecutor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	// A deliberately fragmented in-memory HTTP feed keeps this fixture fully
	// offline while exercising Passthrough, RPC and the real host translators.
	r.HTTPClient = fixtureHTTP{}
	return e.Passthrough.ExecuteStream(ctx, r)
}

type fixtureHTTP struct{}

func (fixtureHTTP) Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	panic("stream-only fixture")
}
func (fixtureHTTP) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	wire := "id: 17\nretry: 2000\n\n"
	for _, payload := range []struct{ name, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"fixture","type":"message","role":"assistant","model":"fixture","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		wire += "event: " + payload.name + "\ndata: " + payload.data + "\n\n"
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, 3)
	chunks <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[:5])}
	chunks <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[5 : len(wire)/2])}
	chunks <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[len(wire)/2:])}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Chunks: chunks}, nil
}

type fixtureManagement struct{}

func (m fixtureManagement) RegisterManagement(context.Context, pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	return pluginapi.ManagementRegistrationResponse{
		Routes:    []pluginapi.ManagementRoute{{Method: "GET", Path: "streamprobe/status", Handler: m}},
		Resources: []pluginapi.ResourceRoute{{Path: "help", Menu: "Help", Handler: m}},
	}, nil
}
func (fixtureManagement) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return pluginapi.ManagementResponse{StatusCode: 200, Body: []byte(req.Path)}, nil
}
