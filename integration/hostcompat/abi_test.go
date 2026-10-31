//go:build cgo && linux

package hostcompat

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Run native argument/ownership checks in a disposable process: a Go c-shared
// image must never be dlclosed in a long-lived test process.
func TestNativeABIContract(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "echo.so")
	build := exec.Command("go", "build", "-buildmode=c-shared", "-o", lib, "./examples/echo")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}
	binary := filepath.Join(dir, "abi-check")
	compile := exec.Command("cc", "-std=c11", "-Wall", "-Wextra", "-Werror", "-I", filepath.Join(root, "cabi"), "testdata/abi_check.c", "-ldl", "-o", binary)
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("build C ABI checker: %v\n%s", err, out)
	}
	if out, err := exec.Command(binary, lib).CombinedOutput(); err != nil {
		t.Fatalf("native ABI contract: %v\n%s", err, out)
	}
}
