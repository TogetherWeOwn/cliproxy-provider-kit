//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestNativeSSEAndManagementThroughPinnedHost(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archDir := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(archDir, "streamprobe.so")
	if runtime.GOOS == "darwin" {
		lib = filepath.Join(archDir, "streamprobe.dylib")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-buildmode=c-shared", "-o", lib, "./testdata/streamprobe")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, out)
	}
	host := pluginhost.New()
	defer host.ShutdownAll()
	enabled := true
	host.ApplyConfig(ctx, &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"streamprobe": {Enabled: &enabled}}}})
	if !host.PluginLoaded("streamprobe") || !host.PluginRegistered("streamprobe") {
		t.Fatalf("fixture did not register: %+v", host.RegisteredPlugins())
	}

	// No live management service is contacted: these requests are dispatched
	// directly into the pinned host using in-memory response recorders.
	host.RegisterManagementRoutes(ctx, nil)
	for _, resource := range []bool{false, true} {
		path := "/v0/management/streamprobe/status"
		if resource {
			path = "/v0/resource/plugins/streamprobe/help"
		}
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		out := httptest.NewRecorder()
		handled := false
		if resource {
			handled = host.ServeResourceHTTP(out, req)
		} else {
			handled = host.ServeManagementHTTP(out, req)
		}
		if !handled || out.Code != 200 || out.Body.String() != path {
			t.Fatalf("route %s: handled=%v code=%d body=%s", path, handled, out.Code, out.Body.String())
		}
	}

	m := executors{}
	host.RegisterExecutors(m, models{})
	executor := m["streamprobe"]
	if executor == nil {
		t.Fatal("fixture executor missing")
	}
	for _, tc := range []struct {
		format sdktranslator.Format
		path   string
	}{
		{sdktranslator.FormatOpenAI, "/v1/chat/completions"},
		{sdktranslator.FormatClaude, "/v1/messages"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			payload := []byte(`{"model":"fixture","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
			resp, err := executor.ExecuteStream(ctx, &coreauth.Auth{ID: "fixture", Provider: "streamprobe"}, coreexecutor.Request{Model: "fixture", Format: tc.format, Payload: payload}, coreexecutor.Options{
				SourceFormat: tc.format, ResponseFormat: tc.format, Stream: true, Metadata: map[string]any{"request_path": tc.path},
			})
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			var wire strings.Builder
			stopped := false
			for {
				select {
				case <-ctx.Done():
					t.Fatal("host stream did not finish")
				case chunk, ok := <-resp.Chunks:
					if !ok {
						if text.String() != "Hello world" || !stopped {
							t.Fatalf("text=%q stopped=%v wire=%s", text.String(), stopped, wire.String())
						}
						if tc.format == sdktranslator.FormatClaude {
							if !strings.Contains(wire.String(), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
								t.Fatal("native route lost complete named SSE events")
							}
							if !strings.Contains(wire.String(), "id: 17\nretry: 2000\n\n") {
								t.Fatal("native route lost metadata-only SSE events")
							}
						}
						return
					}
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					wire.Write(chunk.Payload)
					for _, line := range strings.Split(string(chunk.Payload), "\n") {
						if tc.format == sdktranslator.FormatClaude && !strings.HasPrefix(line, "data:") {
							continue
						}
						line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
						if line == "" || line == "[DONE]" {
							continue
						}
						var event struct {
							Type  string `json:"type"`
							Delta struct {
								Text string `json:"text"`
							} `json:"delta"`
							Choices []struct {
								Delta struct {
									Content string `json:"content"`
								} `json:"delta"`
								Finish string `json:"finish_reason"`
							} `json:"choices"`
						}
						if err := json.Unmarshal([]byte(line), &event); err != nil {
							t.Fatalf("host chunk is not one JSON value: %s: %v", line, err)
						}
						text.WriteString(event.Delta.Text)
						stopped = stopped || event.Type == "message_stop"
						for _, choice := range event.Choices {
							text.WriteString(choice.Delta.Content)
							stopped = stopped || choice.Finish == "stop"
						}
					}
				}
			}
		})
	}
}
