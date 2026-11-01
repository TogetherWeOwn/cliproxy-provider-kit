package providerkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// maxStreamEventBytes bounds one buffered SSE event. Upstream data events are
// small; a stream without line breaks must not grow memory without limit.
const maxStreamEventBytes = 8 << 20

// The host consumes one SSE event per stream chunk, but an upstream's raw
// bytes can carry several events, comments and [DONE] in one read.
// frameStreamChunks re-frames an OpenAI-protocol SSE stream into exactly one
// chunk per data event (line endings CR, LF or CRLF, including a CRLF split
// across upstream reads):
//
//   - When the client speaks OpenAI chat, the host skips translation for this
//     same-format plugin stream and the OpenAI handler writes each chunk as
//     "data: %s\n\n" (v8.0.49 internal/pluginhost/adapters_executors.go,
//     sdk/api/handlers/openai/openai_handlers.go), so the chunk is the bare
//     JSON payload.
//   - Otherwise a host translator reads the chunk and needs one "data:"-framed
//     event per call (internal/translator/openai/claude/
//     openai_claude_response.go), so the chunk is "data: <json>".
//
// Comment lines (": …") and [DONE] are dropped: the host and the handlers
// append their own terminator (adapters_executors.go). Without this framing
// the handlers wrap each raw read in another "data:" prefix, which produced
// "data: data:" frames and duplicate "[DONE]" on /v1/chat/completions and
// lost tokens on /v1/messages.
// Errors pass through; a frame cut short by an error is not delivered. An
// event over 8 MiB, or a multi-line event that is not one JSON value, ends the
// stream with an invalid_stream error.
// onExit, when non-nil, runs after the parser stops (end of stream, caller
// cancellation or framing error) so the caller can release the upstream feed:
// without it a framing error leaves the forwarder blocked on an unread
// channel while the upstream connection stays open.
func frameStreamChunks(ctx context.Context, in <-chan pluginapi.ExecutorStreamChunk, bare bool, onExit func()) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(out)
		if onExit != nil {
			defer onExit()
		}
		var line []byte  // the current, incomplete line
		var event []byte // data lines of the current event, each terminated by '\n'
		send := func(c pluginapi.ExecutorStreamChunk) error {
			select {
			case out <- c:
				return nil
			case <-ctx.Done():
				return errStreamConsumerGone
			}
		}
		dispatch := func() error {
			if len(event) == 0 {
				return nil
			}
			payload := string(event[:len(event)-1])
			event = event[:0]
			if strings.TrimSpace(payload) == "[DONE]" {
				return nil
			}
			// One chunk is one line downstream: an event split over several
			// data lines must be a single JSON value, sent compacted.
			if strings.Contains(payload, "\n") {
				var b bytes.Buffer
				if json.Compact(&b, []byte(payload)) != nil {
					return invalidStreamChunk("invalid_stream: multi-line upstream event is not JSON")
				}
				payload = b.String()
			}
			if !bare {
				payload = "data: " + payload
			}
			return send(pluginapi.ExecutorStreamChunk{Payload: []byte(payload)})
		}
		// field handles one complete line (SSE: CRLF or LF terminated).
		field := func(l []byte) error {
			l = bytes.TrimSuffix(l, []byte("\r"))
			switch {
			case len(l) == 0:
				return dispatch()
			case l[0] == ':':
				// comment
			case bytes.HasPrefix(l, []byte("data:")):
				v := l[len("data:"):]
				if len(v) > 0 && v[0] == ' ' {
					v = v[1:]
				}
				// Count the separator too: every data line advances the
				// bound, so an event of many empty lines still trips it.
				if len(event)+len(v)+1 > maxStreamEventBytes {
					return errStreamEventTooLarge
				}
				event = append(event, v...)
				event = append(event, '\n')
			}
			// Other fields (event, id, retry) carry nothing the host reads.
			return nil
		}
		// run consumes the input; it returns nil at the end of the stream.
		run := func() error {
			for {
				select {
				case <-ctx.Done():
					return errStreamConsumerGone
				case c, ok := <-in:
					if !ok {
						if len(line) > 0 {
							if err := field(line); err != nil {
								return err
							}
						}
						return dispatch()
					}
					buf := append(line, c.Payload...)
					for len(buf) > 0 {
						i := bytes.IndexAny(buf, "\r\n")
						if i < 0 {
							break
						}
						next := i + 1
						if buf[i] == '\r' {
							if i+1 >= len(buf) {
								// A trailing CR may be a CRLF split
								// across reads; hold it for more
								// input or EOF.
								break
							}
							if buf[i+1] == '\n' {
								next = i + 2
							}
						}
						if err := field(buf[:i]); err != nil {
							return err
						}
						buf = buf[next:]
					}
					line = append(line[:0:0], buf...)
					if len(line)+len(event) > maxStreamEventBytes {
						return errStreamEventTooLarge
					}
					if c.Err != nil {
						return c.Err
					}
				}
			}
		}
		if err := run(); err != nil && err != errStreamConsumerGone {
			_ = send(pluginapi.ExecutorStreamChunk{Err: err})
		}
	}()
	return out
}

var (
	errStreamConsumerGone  = errors.New("stream consumer gone")
	errStreamEventTooLarge = invalidStreamChunk("invalid_stream: upstream event exceeds 8 MiB")
)

func invalidStreamChunk(msg string) *pluginabi.Error {
	return rpcError("invalid_stream", msg, http.StatusBadGateway)
}

// openAIOutputFormat reports whether the plugin speaks the OpenAI protocol
// for this call. The host sets Format to the plugin's own declared output
// format ("openai", see sdk/translator FormatOpenAI); an empty format keeps
// the historical OpenAI assumption for direct callers. Translators and
// native host paths consume framed SSE, so only OpenAI output is re-framed.
//
// clientWantsBareJSON reports a client on an OpenAI chat or completions
// route served by an OpenAI-output plugin, the only case in which the host
// passes this plugin's stream through untranslated and writes each chunk
// itself as "data: %s". The plugin cannot read the client format from the request:
// the host sets SourceFormat and Format to the plugin's own declared format
// whatever the client speaks, so the inbound route the host records in the
// request metadata is the reliable signal. Without it the chunk stays
// "data:"-framed, the host's documented chunk form.
func openAIOutputFormat(r pluginapi.ExecutorRequest) bool {
	return r.Format == "" || r.Format == "openai"
}

func clientWantsBareJSON(r pluginapi.ExecutorRequest) bool {
	if !openAIOutputFormat(r) {
		return false
	}
	path, _ := r.Metadata["request_path"].(string)
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/v1/completions")
}
