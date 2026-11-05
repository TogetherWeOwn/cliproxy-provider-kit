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

// maxStreamEventBytes bounds the data lines of one buffered SSE event,
// including one separator per line but excluding the SSE field prefixes.
const maxStreamEventBytes = 8 << 20

type streamFraming struct {
	bare     bool // OpenAI chat handlers add their own data: prefix
	native   bool // native handlers write complete, named SSE events verbatim
	dropDone bool // the OpenAI host appends its own terminator
}

// frameStreamChunks implements the OpenAI-output chunk contract: bare JSON for
// chat/completions clients, one data:-framed event per translator call otherwise.
// Comments and [DONE] are dropped. onExit releases the upstream on completion,
// cancellation or framing error, including when the forwarder is blocked sending.
func frameStreamChunks(ctx context.Context, in <-chan pluginapi.ExecutorStreamChunk, bare bool, onExit func()) <-chan pluginapi.ExecutorStreamChunk {
	return frameSSEChunks(ctx, in, streamFraming{bare: bare, dropDone: true}, onExit)
}

// frameSSEChunks splits raw HTTP reads into complete SSE events. Translators
// consume one data: line per call (v8.0.49 adapters_executors.go); native handlers
// write full events, including event names and blank-line delimiters. CR, LF and
// CRLF are accepted, even across reads. Only a stream-leading UTF-8 BOM is ignored.
// A partial event cut short by an upstream error is never delivered.
func frameSSEChunks(ctx context.Context, in <-chan pluginapi.ExecutorStreamChunk, framing streamFraming, onExit func()) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(out)
		if onExit != nil {
			defer onExit()
		}
		var line []byte
		var event []byte // each data line is terminated by '\n'
		var name string
		firstLine := true
		send := func(c pluginapi.ExecutorStreamChunk) error {
			select {
			case out <- c:
				return nil
			case <-ctx.Done():
				return errStreamConsumerGone
			}
		}
		dispatch := func() error {
			eventName := name
			name = ""
			if len(event) == 0 {
				return nil
			}
			payload := string(event[:len(event)-1])
			event = event[:0]
			if framing.dropDone && strings.TrimSpace(payload) == "[DONE]" {
				return nil
			}
			// Translators read one JSON value per call, not an SSE document.
			if strings.Contains(payload, "\n") {
				var b bytes.Buffer
				if json.Compact(&b, []byte(payload)) != nil {
					return invalidStreamChunk("invalid_stream: multi-line upstream event is not JSON")
				}
				payload = b.String()
			}
			if !framing.bare {
				payload = "data: " + payload
			}
			if framing.native {
				if eventName != "" {
					payload = "event: " + eventName + "\n" + payload
				}
				payload += "\n\n"
			}
			return send(pluginapi.ExecutorStreamChunk{Payload: []byte(payload)})
		}
		withoutBOM := func(l []byte) []byte {
			if firstLine {
				return bytes.TrimPrefix(l, []byte("\xef\xbb\xbf"))
			}
			return l
		}
		dataValue := func(l []byte) []byte {
			v := l[len("data:"):]
			if len(v) > 0 && v[0] == ' ' {
				v = v[1:]
			}
			return v
		}
		checkLine := func(l []byte) error {
			l = withoutBOM(l)
			if bytes.HasPrefix(l, []byte("data:")) {
				if len(event)+len(dataValue(l))+1 > maxStreamEventBytes {
					return errStreamEventTooLarge
				}
			} else if len(l) > maxStreamEventBytes {
				return errStreamEventTooLarge
			}
			return nil
		}
		field := func(l []byte) error {
			l = withoutBOM(l)
			firstLine = false
			switch {
			case len(l) == 0:
				return dispatch()
			case l[0] == ':':
				// comment
			case bytes.HasPrefix(l, []byte("data:")):
				event = append(event, dataValue(l)...)
				event = append(event, '\n')
			case bytes.HasPrefix(l, []byte("event:")):
				name = string(bytes.TrimPrefix(l[len("event:"):], []byte(" ")))
			default:
				// Missing Content-Type retains the SSE assumption. Do not turn
				// unsupported bare JSON/NDJSON into an empty successful stream.
				v := bytes.TrimSpace(l)
				if len(v) > 0 && (v[0] == '{' || v[0] == '[' || json.Valid(v)) {
					return invalidStreamChunk("invalid_stream: expected SSE fields, received bare JSON")
				}
			}
			return nil
		}
		run := func() error {
			var skipLF bool
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
					// Scan only new bytes. Retained partial lines grow geometrically
					// rather than being copied and rescanned on every HTTP read.
					buf := c.Payload
					for len(buf) > 0 {
						if skipLF {
							skipLF = false
							if buf[0] == '\n' {
								buf = buf[1:]
								continue
							}
						}
						i := bytes.IndexAny(buf, "\r\n")
						n := i
						if n < 0 {
							n = len(buf)
						}
						// Bound before copying, allowing only field/BOM overhead.
						if n > maxStreamEventBytes+9-len(line) {
							return errStreamEventTooLarge
						}
						line = append(line, buf[:n]...)
						if err := checkLine(line); err != nil {
							return err
						}
						if i < 0 {
							break
						}
						if err := field(line); err != nil {
							return err
						}
						line = line[:0]
						skipLF = buf[i] == '\r'
						buf = buf[i+1:]
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

func openAIOutputFormat(r pluginapi.ExecutorRequest) bool {
	return r.Format == "" || r.Format == "openai"
}

// The host records the inbound client route in metadata. Format and SourceFormat
// both describe the plugin's own protocol, not the client's (v8.0.49 host adapter).
func streamRequestPath(r pluginapi.ExecutorRequest) string {
	path, _ := r.Metadata["request_path"].(string)
	return strings.TrimRight(strings.TrimSpace(path), "/")
}

func clientWantsBareJSON(r pluginapi.ExecutorRequest) bool {
	if !openAIOutputFormat(r) {
		return false
	}
	path := streamRequestPath(r)
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/v1/completions")
}

// Native handlers write SSE verbatim; translators require only one data: line.
// Without a known client route keep complete events for direct SDK callers.
func clientWantsNativeSSE(r pluginapi.ExecutorRequest) bool {
	if openAIOutputFormat(r) {
		return false
	}
	path := streamRequestPath(r)
	switch {
	case strings.HasSuffix(path, "/chat/completions"), strings.HasSuffix(path, "/v1/completions"):
		return false
	case strings.HasSuffix(path, "/messages"):
		return r.Format == "claude"
	case strings.HasSuffix(path, "/responses"):
		return r.Format == "openai-response"
	case strings.Contains(path, "/models/"):
		return r.Format == "gemini"
	default:
		return true
	}
}
