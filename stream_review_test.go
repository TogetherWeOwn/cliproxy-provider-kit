package providerkit

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSSEReviewRegressions(t *testing.T) {
	t.Run("native fields", func(t *testing.T) {
		for _, wire := range []string{
			"id: 17\nretry: 2000\nevent: delta\ndata: {}\n\n",
			"id: 17\n\n", "retry: 2000\n\n", "id:\n\n",
			"id: first\nid: last\nretry: invalid\nretry: 123\ndata: {}\n\n",
			": heartbeat\n\n",
			"event: text\ndata: hello\ndata:  world\n\n",
			"data: {\ndata:  \"a\": 1}\n\n",
			"data\n\n", "event: empty\ndata\n\n",
			"event: text\ndata\ndata: hello\ndata\n\n",
		} {
			for split := 0; split <= len(wire); split++ {
				got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(wire[:split]), streamBytes(wire[split:])), streamFraming{native: true}, nil))
				if len(errs) != 0 || strings.Join(got, "") != wire {
					t.Errorf("wire=%q split=%d: got %q errs %v", wire, split, got, errs)
					break
				}
			}
		}
	})
	t.Run("native truncation", func(t *testing.T) {
		complete := "data: {}\n\n"
		for _, tail := range []string{"event: message_stop\ndata: {\"type\":\"message_stop\"}", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n", "id: 17\n", "data", "data\n"} {
			got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(complete+tail)), streamFraming{native: true}, nil))
			if len(errs) != 0 || !reflect.DeepEqual(got, []string{complete}) {
				t.Errorf("tail=%q: got %q errs %v", tail, got, errs)
			}
		}
	})
	t.Run("translator compatibility", func(t *testing.T) {
		for _, tc := range []struct {
			wire string
			want []string
		}{
			{"data\n\n", []string{"data: "}},
			{"event: empty\ndata\n\n", []string{"data: "}},
			{"data\ndata: {}\n\n", []string{"data: {}"}},
			{"data: {}\ndata\n\n", []string{"data: {}"}},
			{"data: {}", []string{"data: {}"}},
			{"data: {}\n", []string{"data: {}"}},
			{"id: 17\nretry: 123\n\ndata: {}\n\n", []string{"data: {}"}},
		} {
			got, errs := collectStream(t, frameStreamChunks(context.Background(), streamFeed(streamBytes(tc.wire)), false, nil))
			if len(errs) != 0 || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("wire=%q: got %q errs %v", tc.wire, got, errs)
			}
		}
	})
	t.Run("cumulative bounds", func(t *testing.T) {
		for _, native := range []bool{false, true} {
			for _, extra := range []int{0, 1} {
				// Both lines are independently below the limit, and the joined
				// value is valid JSON, so only cumulative accounting can reject it.
				first := "data: {" + strings.Repeat(" ", maxStreamEventBytes/2-2) + "\n"
				second := "data: " + strings.Repeat(" ", maxStreamEventBytes/2-2+extra) + "}\n\n"
				for _, split := range []int{0, len(first), len(first) + 8, len(first + second)} {
					got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes((first + second)[:split]), streamBytes((first + second)[split:])), streamFraming{native: native}, nil))
					if extra == 0 {
						if len(errs) != 0 || len(got) != 1 {
							t.Fatalf("native=%v split=%d: events %d errs %v", native, split, len(got), errs)
						}
					} else if len(got) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "exceeds 8 MiB") {
						t.Fatalf("native=%v split=%d: events %d errs %v", native, split, len(got), errs)
					}
				}
			}
		}
		for _, field := range []string{"id: ", "retry: ", "event: ", ": "} {
			// Metadata-only events must also be bounded cumulatively.
			wire := field + string(bytes.Repeat([]byte("1"), maxStreamEventBytes/2)) + "\n" + field + string(bytes.Repeat([]byte("2"), maxStreamEventBytes/2)) + "\n\n"
			got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(wire)), streamFraming{native: true}, nil))
			want := ""
			if field == ": " {
				// Completed standalone comments are delivered promptly, but still
				// count against the cumulative budget until a blank line.
				want = strings.SplitN(wire, "\n", 2)[0] + "\n"
			}
			if strings.Join(got, "") != want || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
				t.Fatalf("field=%q: events %d errs %v", field, len(got), errs)
			}
		}
		for _, native := range []bool{false, true} {
			wire := "data: " + strings.Repeat(" ", maxStreamEventBytes-1) + "\ndata\n\n"
			got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(wire)), streamFraming{native: native}, nil))
			if len(got) != 0 || len(errs) != 1 || streamRPC(t, errs[0]).Code != "invalid_stream" {
				t.Fatalf("colonless native=%v: events %d errs %v", native, len(got), errs)
			}
		}
	})
	t.Run("native cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		exited := make(chan struct{})
		out := frameSSEChunks(ctx, streamFeed(streamBytes("id: 17\n\n")), streamFraming{native: true}, func() { close(exited) })
		for range out {
		}
		select {
		case <-exited:
		default:
			t.Fatal("native cancellation did not release upstream")
		}
	})
	t.Run("native line endings", func(t *testing.T) {
		for _, sep := range []string{"\r", "\r\n"} {
			wire := "\xef\xbb\xbfid: 17" + sep + "data: hello" + sep + sep
			for split := 0; split <= len(wire); split++ {
				t.Run(fmt.Sprintf("%q/%d", sep, split), func(t *testing.T) {
					got, errs := collectStream(t, frameSSEChunks(context.Background(), streamFeed(streamBytes(wire[:split]), streamBytes(wire[split:])), streamFraming{native: true}, nil))
					if len(errs) != 0 || !reflect.DeepEqual(got, []string{"id: 17\ndata: hello\n\n"}) {
						t.Fatalf("got %q errs %v", got, errs)
					}
				})
			}
		}
	})
}
