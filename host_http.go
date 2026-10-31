package providerkit

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// NewHostHTTPClient creates a callback-scoped SDK client. Runtime injects this
// automatically into each request. Never retain it past that request/stream.
// Cancellation uses the host's operation IDs, not abandoned blocking goroutines.
func NewHostHTTPClient(h Caller, callbackID string) pluginapi.HostHTTPClient {
	return &hostHTTP{h, callbackID}
}

type hostHTTP struct {
	host       Caller
	callbackID string
}
type httpOperation struct {
	CallbackID  string `json:"host_callback_id,omitempty"`
	OperationID string `json:"operation_id"`
}

func hostCall(ctx context.Context, h Caller, m string, q any, out any) error {
	if h == nil {
		return rpcError("host_unavailable", "host callbacks are unavailable", 503)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(q)
	if err != nil {
		return rpcError("invalid_request", "host request encoding failed", 400)
	}
	resp, err := h.Call(ctx, m, raw)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return rpcError("host_error", "host callback failed", 502)
	}
	var env pluginabi.Envelope
	if json.Unmarshal(resp, &env) != nil || !env.OK {
		return rpcError("host_error", "host callback failed", 502)
	}
	if out != nil && json.Unmarshal(env.Result, out) != nil {
		return rpcError("host_error", "invalid host response", 502)
	}
	return nil
}
func (c *hostHTTP) open(ctx context.Context) (httpOperation, func(), error) {
	var out struct {
		ID string `json:"operation_id"`
	}
	if err := hostCall(ctx, c.host, pluginabi.MethodHostHTTPOperationOpen, map[string]string{"host_callback_id": c.callbackID}, &out); err != nil {
		return httpOperation{}, nil, err
	}
	op := httpOperation{c.callbackID, out.ID}
	if op.OperationID == "" {
		return op, nil, rpcError("host_error", "host returned no operation ID", 502)
	}
	var once sync.Once
	cancel := func() {
		once.Do(func() { _ = hostCall(context.Background(), c.host, pluginabi.MethodHostHTTPCancel, op, nil) })
	}
	stop := context.AfterFunc(ctx, cancel)
	return op, func() { stop(); cancel() }, nil
}
func (c *hostHTTP) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	op, finish, err := c.open(ctx)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer finish()
	var out pluginapi.HTTPResponse
	err = hostCall(ctx, c.host, pluginabi.MethodHostHTTPDo, struct {
		Request pluginapi.HTTPRequest `json:"request"`
		httpOperation
	}{req, op}, &out)
	return out, err
}
func (c *hostHTTP) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	op, finish, err := c.open(ctx)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	var out struct {
		StatusCode int                 `json:"status_code"`
		Headers    map[string][]string `json:"headers"`
		ID         string              `json:"stream_id"`
	}
	if err = hostCall(ctx, c.host, pluginabi.MethodHostHTTPDoStream, struct {
		Request pluginapi.HTTPRequest `json:"request"`
		httpOperation
	}{req, op}, &out); err != nil {
		finish()
		return pluginapi.HTTPStreamResponse{}, err
	}
	if out.ID == "" {
		finish()
		return pluginapi.HTTPStreamResponse{}, rpcError("host_error", "host returned no stream ID", 502)
	}
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		defer finish()
		defer func() {
			_ = hostCall(context.Background(), c.host, pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": out.ID, "host_callback_id": c.callbackID}, nil)
		}()
		send := func(ch pluginapi.HTTPStreamChunk) bool {
			select {
			case chunks <- ch:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			var read struct {
				Payload []byte `json:"payload"`
				Done    bool   `json:"done"`
				Error   string `json:"error"`
			}
			err := hostCall(ctx, c.host, pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": out.ID, "host_callback_id": c.callbackID}, &read)
			if err != nil {
				send(pluginapi.HTTPStreamChunk{Err: err})
				return
			}
			if len(read.Payload) > 0 && !send(pluginapi.HTTPStreamChunk{Payload: read.Payload}) {
				return
			}
			if read.Error != "" {
				send(pluginapi.HTTPStreamChunk{Err: rpcError("upstream_stream_error", "upstream stream failed", 502)})
				return
			}
			if read.Done {
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: out.StatusCode, Headers: out.Headers, Chunks: chunks}, nil
}
