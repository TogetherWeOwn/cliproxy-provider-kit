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

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestGeminiStreamThroughRealHandler(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archDir := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(archDir, 0755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(archDir, "geminiprobe.so")
	if runtime.GOOS == "darwin" {
		lib = filepath.Join(archDir, "geminiprobe.dylib")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-buildmode=c-shared", "-o", lib, "./testdata/geminiprobe")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, out)
	}
	host := pluginhost.New()
	defer host.ShutdownAll()
	enabled := true
	host.ApplyConfig(ctx, &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"geminiprobe": {Enabled: &enabled}}}})
	if !host.PluginLoaded("geminiprobe") || !host.PluginRegistered("geminiprobe") {
		t.Fatalf("fixture did not register: %+v", host.RegisteredPlugins())
	}
	m := executors{}
	host.RegisterExecutors(m, models{})
	if m["geminiprobe"] == nil {
		t.Fatal("fixture executor missing")
	}
	mgr := coreauth.NewManager(nil, nil, nil)
	mgr.SetRetryConfig(0, 0, 1)
	mgr.RegisterExecutor(m["geminiprobe"])
	a := &coreauth.Auth{ID: "gemini-stream-auth", Provider: "geminiprobe", Status: coreauth.StatusActive, Attributes: map[string]string{"type": "api_key"}}
	if _, err := mgr.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "geminiprobe/fixture"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	gin.SetMode(gin.TestMode)
	router := gin.New()
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, mgr)
	router.POST("/v1beta/models/*action", geminihandlers.NewGeminiAPIHandler(base).GeminiHandler)
	for _, query := range []string{"", "?alt=sse", "?$alt=sse", "?alt=json", "?$alt=json"} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/geminiprobe/fixture:streamGenerateContent"+query, strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			wire := rec.Body.String()
			var events []json.RawMessage
			if strings.HasSuffix(query, "=json") {
				if err := json.Unmarshal([]byte(wire), &events); err != nil {
					t.Fatalf("raw alt=json stream is not a JSON array: %s: %v", wire, err)
				}
			} else {
				if rec.Header().Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(wire, "\n\n") {
					t.Fatalf("missing SSE headers/delimiter: %v %q", rec.Header(), wire)
				}
				for _, frame := range strings.Split(strings.TrimSuffix(wire, "\n\n"), "\n\n") {
					if !strings.HasPrefix(frame, "data: ") || !json.Valid([]byte(strings.TrimPrefix(frame, "data: "))) {
						t.Fatalf("Gemini event is not one JSON data value: %q\n%s", frame, wire)
					}
					events = append(events, json.RawMessage(strings.TrimPrefix(frame, "data: ")))
				}
			}
			if len(events) != 2 {
				t.Fatalf("events=%d, want 2: %s", len(events), wire)
			}
			var text strings.Builder
			stopped := false
			for _, event := range events {
				var response struct {
					Candidates []struct {
						Content struct {
							Parts []struct{ Text string } `json:"parts"`
						} `json:"content"`
						FinishReason string `json:"finishReason"`
					} `json:"candidates"`
				}
				if err := json.Unmarshal(event, &response); err != nil {
					t.Fatal(err)
				}
				for _, candidate := range response.Candidates {
					for _, part := range candidate.Content.Parts {
						text.WriteString(part.Text)
					}
					stopped = stopped || candidate.FinishReason == "STOP"
				}
			}
			if text.String() != "Hello world" || !stopped {
				t.Fatalf("text=%q stopped=%v: %s", text.String(), stopped, wire)
			}
		})
	}
}
