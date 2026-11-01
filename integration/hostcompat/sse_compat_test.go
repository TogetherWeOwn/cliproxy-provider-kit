//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator" // the host's built-in translators, as in cmd/server
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	claudehandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// packedTokens are the deltas of packedEchoSSE, in order.
var packedTokens = []string{"Hello", ", ", "world", "!"}

func echoChunk(content string, finish *string) string {
	delta := map[string]any{"content": content}
	if content == packedTokens[0] {
		delta["role"] = "assistant"
	}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != nil {
		choice = map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": *finish}
	}
	raw, _ := json.Marshal(map[string]any{"id": "echo-1", "object": "chat.completion.chunk", "model": "echo-model", "choices": []any{choice}})
	return "data: " + string(raw) + "\n\n"
}

// packedEchoSSE is a hostile upstream stream: comments, several events in one
// write, one event split across writes, and a trailing [DONE].
var packedEchoSSE = func() []string {
	stop := "stop"
	return []string{
		": ECHO PROCESSING\n\n" + echoChunk(packedTokens[0], nil) + echoChunk(packedTokens[1], nil),
		": ECHO PROCESSING\n\n" + echoChunk(packedTokens[2], nil)[:40],
		echoChunk(packedTokens[2], nil)[40:] + echoChunk(packedTokens[3], nil),
		echoChunk("", &stop),
		"data: [DONE]\n\n",
	}
}()

// A packed upstream must never leak its framing to clients: the kit re-frames
// ExecuteStream output into one host chunk per data event, so the real OpenAI
// handler writes single "data:" lines with one terminator, and the Claude
// translator keeps every token.
func TestPackedUpstreamThroughRealHandlers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &input)
		if input["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, write := range packedEchoSSE {
				_, _ = w.Write([]byte(write))
				w.(http.Flusher).Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"echo","object":"chat.completion","model":"echo-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archDir := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(archDir, 0755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(archDir, "echo.so")
	if runtime.GOOS == "darwin" {
		lib = filepath.Join(archDir, "echo.dylib")
	}
	build := exec.Command("go", "build", "-buildmode=c-shared", "-o", lib, "./examples/echo")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("c-shared build: %v\n%s", out, err)
	}
	enabled := true
	var raw yaml.Node
	if err = yaml.Unmarshal([]byte("enabled: true\nupstream_url: "+upstream.URL+"\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"echo": {Enabled: &enabled, Raw: *raw.Content[0]}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	host.ApplyConfig(ctx, cfg)
	if !host.PluginLoaded("echo") || !host.PluginRegistered("echo") {
		t.Fatalf("real loader did not register plugin: %+v", host.RegisteredPlugins())
	}
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "echo", FileName: "example.json", RawJSON: []byte(`{"type":"echo","api_key":"example-key"}`)})
	if err != nil || !handled || auth == nil {
		t.Fatalf("parse %v %v", handled, err)
	}
	m := executors{}
	host.RegisterModels(ctx, models{})
	host.RegisterExecutors(m, models{})
	ex, ok := m["echo"]
	if !ok {
		t.Fatal("real executor adapter not registered")
	}

	gin.SetMode(gin.TestMode)
	mgr := coreauth.NewManager(nil, nil, nil)
	mgr.SetRetryConfig(0, 0, 1)
	mgr.RegisterExecutor(ex)
	a := auth.Clone()
	a.ID = "packed-auth"
	a.Status = coreauth.StatusActive
	a.Attributes = map[string]string{"type": "api_key", "priority": "0"}
	if _, err := mgr.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "echo/echo-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, mgr)
	router := gin.New()
	router.POST("/v1/chat/completions", openaihandlers.NewOpenAIAPIHandler(base).ChatCompletions)
	router.POST("/v1/messages", claudehandlers.NewClaudeCodeAPIHandler(base).ClaudeMessages)
	post := func(t *testing.T, path, body string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d %s", path, rec.Code, rec.Body.String())
		}
		out := rec.Body.String()
		if strings.Contains(out, "data: data:") || strings.Contains(out, "ECHO PROCESSING") {
			t.Fatalf("%s: raw upstream framing reached the client:\n%s", path, out)
		}
		return out
	}
	want := strings.Join(packedTokens, "")

	t.Run("OpenAI chat", func(t *testing.T) {
		out := post(t, "/v1/chat/completions", `{"model":"echo/echo-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if strings.Count(out, "[DONE]") != 1 || !strings.HasSuffix(out, "data: [DONE]\n\n") {
			t.Fatalf("[DONE] framing:\n%s", out)
		}
		var text strings.Builder
		for _, frame := range strings.Split(out, "\n\n") {
			if strings.TrimSpace(frame) == "" {
				continue
			}
			lines := strings.Split(frame, "\n")
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
				t.Fatalf("frame is not one data line %q\n%s", frame, out)
			}
			d := strings.TrimPrefix(lines[0], "data: ")
			if d == "[DONE]" {
				continue
			}
			var ev struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(d), &ev) != nil {
				t.Fatalf("event data is not JSON: %q", d)
			}
			for _, c := range ev.Choices {
				text.WriteString(c.Delta.Content)
			}
		}
		if text.String() != want {
			t.Fatalf("tokens %q, want %q\n%s", text.String(), want, out)
		}
	})

	t.Run("Claude messages", func(t *testing.T) {
		out := post(t, "/v1/messages", `{"model":"echo/echo-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		var text strings.Builder
		stops := 0
		for _, frame := range strings.Split(out, "\n\n") {
			if strings.TrimSpace(frame) == "" {
				continue
			}
			var data []string
			for _, line := range strings.Split(frame, "\n") {
				switch {
				case strings.HasPrefix(line, "data: "):
					data = append(data, strings.TrimPrefix(line, "data: "))
				case strings.HasPrefix(line, "event: "):
				default:
					t.Fatalf("malformed SSE line %q\n%s", line, out)
				}
			}
			if len(data) != 1 {
				t.Fatalf("frame without exactly one data line %q\n%s", frame, out)
			}
			d := data[0]
			var ev struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(d), &ev) != nil {
				t.Fatalf("event %q", d)
			}
			if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
				text.WriteString(ev.Delta.Text)
			}
			if ev.Type == "message_stop" {
				stops++
			}
		}
		if text.String() != want || stops != 1 {
			t.Fatalf("tokens %q (want %q), message_stop %d\n%s", text.String(), want, stops, out)
		}
	})
}
