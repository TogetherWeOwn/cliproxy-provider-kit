//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
)

func TestRealHostCredentialOwnership(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archDir := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(archDir, 0755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(archDir, "ownership.so")
	if runtime.GOOS == "darwin" {
		lib = filepath.Join(archDir, "ownership.dylib")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-buildmode=c-shared", "-o", lib, "./integration/testdata/ownership")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("c-shared ownership build: %v\n%s", err, out)
	}
	host := pluginhost.New()
	defer host.ShutdownAll()
	enabled := true
	host.ApplyConfig(ctx, &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{
		Enabled: true, Dir: dir,
		Configs: map[string]config.PluginInstanceConfig{"ownership": {Enabled: &enabled}},
	}})
	if !host.PluginLoaded("ownership") || !host.PluginRegistered("ownership") {
		t.Fatal("real loader did not register ownership fixture")
	}
	// Before parsing any credentials or registering models/executors, only the
	// auth.identifier extension can establish this auth-only fixture's claim.
	if !host.HasAuthProvider("ownership-portal") || !host.HasAuthProvider("legacy-echo") {
		t.Fatal("declared credential provider was not matched by the real host")
	}
	for _, pair := range [][2]string{
		{"ownership-portal", "legacy-echo"},
		{"legacy-echo", "ownership-portal"},
		{" OWNERSHIP-PORTAL ", " LEGACY-ECHO "},
	} {
		if !host.SameAuthProvider(pair[0], pair[1]) {
			t.Fatalf("host did not match declared ownership: %q", pair)
		}
	}
	for _, unrelated := range []string{"unrelated", "legacy", "legacy-echo-other", ""} {
		if host.HasAuthProvider(unrelated) || host.SameAuthProvider("ownership-portal", unrelated) {
			t.Fatalf("host matched an undeclared credential provider %q", unrelated)
		}
	}
	host.ShutdownAll()
	if host.HasAuthProvider("legacy-echo") || host.SameAuthProvider("ownership-portal", "legacy-echo") {
		t.Fatal("credential ownership survived plugin shutdown")
	}
}
