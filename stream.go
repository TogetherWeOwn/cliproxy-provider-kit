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

// maxStreamEventBytes bounds buffered SSE data values plus one separator per
// line, excluding data: prefixes. Native events also charge other fields verbatim.
const maxStreamEventBytes = 8 << 20

type streamFraming struct {
	bare           bool // OpenAI chat and Gemini handlers add their own data: prefix
	native         bool // Claude/Responses handlers write native SSE bytes verbatim
	dropDone       bool // stop at [DONE]; the OpenAI host appends its own terminator
	rejectBareJSON bool // missing Content-Type leaves JSON versus SSE ambiguous
}

// frameStreamChunks implements the OpenAI-output chunk contract: bare JSON for
// chat/completions clients, one data:-framed event per translator call otherwise.
// Comments are dropped; [DONE] ends the stream. onExit releases the upstream on completion,
// cancellation or framing error, including when the forwarder is blocked sending.
func frameStreamChunks(ctx context.Context, in <-chan pluginapi.ExecutorStreamChunk, bare bool, onExit func()) <-chan pluginapi.ExecutorStreamChunk {
	return frameSSEChunks(ctx, in, streamFraming{bare: bare, dropDone: true}, onExit)
}

// frameSSEChunks splits raw HTTP reads into complete SSE events. Translators
// consume one data: line per call (v8.0.49 adapters_executors.go); native SSE
// handlers write full events and standalone comment lines. CR, LF and CRLF are
// accepted, even across reads. Only a stream-leading UTF-8 BOM is ignored.
// Unfinished data/metadata events are never delivered on a native route.
func frameSSEChunks(ctx context.Context, in <-chan pluginapi.ExecutorStreamChunk, framing streamFraming, onExit func()) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(out)
		if onExit != nil {
			defer onExit()
		}
		var line []byte
		var event []byte       // each data line is terminated by '\n'
		var nativeEvent []byte // original fields with normalized LF separators
		var nativeBytes int    // same data budget, plus metadata/comment bytes
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
			if framing.native {
				if nativeBytes == 0 {
					return nil
				}
				payload := append(nativeEvent, '\n')
				// The receiver owns this buffer; never reuse it for another event.
				nativeEvent = nil
				nativeBytes = 0
				return send(pluginapi.ExecutorStreamChunk{Payload: payload})
			}
			if len(event) == 0 {
				return nil
			}
			payload := string(event[:len(event)-1])
			event = event[:0]
			if framing.dropDone && strings.TrimSpace(payload) == "[DONE]" {
				return errStreamComplete
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
			return send(pluginapi.ExecutorStreamChunk{Payload: []byte(payload)})
		}
		withoutBOM := func(l []byte) []byte {
			if firstLine {
				return bytes.TrimPrefix(l, []byte("\xef\xbb\xbf"))
			}
			return l
		}
		dataValue := func(l []byte) ([]byte, bool) {
			if bytes.Equal(l, []byte("data")) {
				return nil, true // a colonless field has an empty value
			}
			if !bytes.HasPrefix(l, []byte("data:")) {
				return nil, false
			}
			v := l[len("data:"):]
			if len(v) > 0 && v[0] == ' ' {
				v = v[1:]
			}
			return v, true
		}
		checkLine := func(l []byte, complete bool) error {
			l = withoutBOM(l)
			v, data := dataValue(l)
			if data && (complete || bytes.HasPrefix(l, []byte("data:"))) {
				buffered := len(event)
				if framing.native {
					buffered = nativeBytes
				}
				if buffered+len(v)+1 > maxStreamEventBytes {
					return errStreamEventTooLarge
				}
			} else if len(l) > maxStreamEventBytes {
				return errStreamEventTooLarge
			} else if framing.native && complete && len(l) > 0 && nativeBytes+len(l)+1 > maxStreamEventBytes {
				return errStreamEventTooLarge
			}
			return nil
		}
		field := func(l []byte, terminated bool) error {
			if err := checkLine(l, true); err != nil {
				return err
			}
			l = withoutBOM(l)
			firstLine = false
			if len(l) == 0 {
				return dispatch()
			}
			v, data := dataValue(l)
			if !data && framing.rejectBareJSON {
				// Only a missing Content-Type is ambiguous. Explicit SSE permits
				// JSON-looking unknown fields, including colonless fields.
				v := bytes.TrimSpace(l)
				if len(v) > 0 && (v[0] == '{' || v[0] == '[' || json.Valid(v)) {
					return invalidStreamChunk("invalid_stream: expected SSE fields, received bare JSON")
				}
			}
			if framing.native {
				if terminated && l[0] == ':' && len(nativeEvent) == 0 {
					// Standalone comments keep native connections live without a
					// blank line. Charge them until the real event delimiter, and
					// never let a comment overtake buffered data/metadata fields.
					nativeBytes += len(l) + 1
					return send(pluginapi.ExecutorStreamChunk{Payload: append(bytes.Clone(l), '\n')})
				}
				// Retain fields in source order, including metadata-only events,
				// comments and unknown fields. The downstream SSE client applies
				// id/retry validation; do not invent or persist parser state here.
				nativeEvent = append(nativeEvent, l...)
				nativeEvent = append(nativeEvent, '\n')
				if data {
					nativeBytes += len(v) + 1
				} else {
					nativeBytes += len(l) + 1
				}
			} else if data {
				event = append(event, v...)
				event = append(event, '\n')
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
							if err := field(line, false); err != nil {
								return err
							}
						}
						if framing.native {
							// EOF is not an SSE event delimiter. Translators keep
							// their historical flush, but native clients must not
							// receive a fabricated completion event.
							return nil
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
						if err := checkLine(line, false); err != nil {
							return err
						}
						if i < 0 {
							break
						}
						if err := field(line, true); err != nil {
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
		if err := run(); err != nil && err != errStreamConsumerGone && err != errStreamComplete {
			_ = send(pluginapi.ExecutorStreamChunk{Err: err})
		}
	}()
	return out
}

var (
	errStreamComplete      = errors.New("stream complete")
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
	path := streamRequestPath(r)
	if openAIOutputFormat(r) {
		return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/v1/completions")
	}
	// Same-format Gemini bypasses translation, but its HTTP writer adds the
	// SSE envelope (default/alt=sse) or writes raw data (other alt modes).
	return r.Format == "gemini" && strings.Contains(path, "/models/")
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
		return false // Gemini's writer expects payloads, not native SSE
	default:
		return true
	}
}
