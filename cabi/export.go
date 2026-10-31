// Package cabi exports the ABI 1 function table for Go c-shared provider plugins.
// Import it from a main package and call Export in init; see examples/echo.
package cabi

/*
#include "abi.h"
*/
import "C"

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"unsafe"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Factory constructs a fresh runtime for each successful native initialization.
// A resident image can be logically shut down and initialized again by the host.
type Factory func(providerkit.Caller) (*providerkit.Runtime, error)

var state struct {
	sync.Mutex
	factory Factory
	runtime *providerkit.Runtime
}

// Export installs the factory before the host calls cliproxy_plugin_init.
// Call once from main.init, never from main.main (c-shared does not run main).
func Export(factory Factory) error {
	if factory == nil {
		return errors.New("plugin factory is required")
	}
	state.Lock()
	defer state.Unlock()
	if state.factory != nil {
		return errors.New("plugin factory is already installed")
	}
	state.factory = factory
	return nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) (rc C.int) {
	rc = 1
	defer func() {
		if recover() != nil {
			rc = 1
		}
	}()
	if host == nil || plugin == nil || uint32(host.abi_version) != pluginabi.ABIVersion || host.call == nil || host.free_buffer == nil {
		return 1
	}
	state.Lock()
	defer state.Unlock()
	if state.factory == nil || state.runtime != nil {
		return 1
	}
	// Copy the C table, not the host's table pointer. host_ctx is retained by the
	// host for the lifetime of the resident image (the pinned loader never dlcloses).
	callbacks := &hostCaller{api: *host}
	runtime, err := state.factory(callbacks)
	if err != nil || runtime == nil {
		return 1
	}
	state.runtime = runtime
	C.kit_set_plugin(plugin)
	return 0
}

//export cliproxy_plugin_call
func cliproxy_plugin_call(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	rc = 1
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			_ = write(response, []byte(`{"ok":false,"error":{"code":"plugin_panic","message":"native plugin call panicked","http_status":500}}`))
			rc = 1
		}
	}()
	if method == nil || uint64(length) > math.MaxInt32 || length > 0 && request == nil {
		write(response, []byte(`{"ok":false,"error":{"code":"invalid_request","message":"invalid native call arguments","http_status":400}}`))
		return 1
	}
	var raw []byte
	if length > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(length))
	}
	state.Lock()
	r := state.runtime
	state.Unlock()
	if r == nil {
		write(response, []byte(`{"ok":false,"error":{"code":"not_ready","message":"plugin is not initialized","http_status":503}}`))
		return 1
	}
	out := r.Call(context.Background(), C.GoString(method), raw)
	if !write(response, out) {
		return 1
	}
	return 0
}
func write(response *C.cliproxy_buffer, raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return false
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
	return true
}

//export cliproxy_plugin_free_buffer
func cliproxy_plugin_free_buffer(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxy_plugin_shutdown
func cliproxy_plugin_shutdown() {
	defer func() { _ = recover() }()
	state.Lock()
	r := state.runtime
	state.runtime = nil
	state.Unlock()
	if r != nil {
		r.Shutdown(context.Background())
	}
}

type hostCaller struct{ api C.cliproxy_host_api }

func (h *hostCaller) Call(ctx context.Context, method string, request []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.IndexByte(method, 0) >= 0 {
		return nil, errors.New("invalid host callback method")
	}
	cmethod := C.CString(method)
	defer C.free(unsafe.Pointer(cmethod))
	var data unsafe.Pointer
	if len(request) > 0 {
		data = C.CBytes(request)
		defer C.free(data)
	}
	var out C.cliproxy_buffer
	rc := C.kit_host_call(&h.api, cmethod, (*C.uint8_t)(data), C.size_t(len(request)), &out)
	// Free even error responses, with the host's allocator, exactly once.
	if out.ptr != nil {
		defer C.kit_host_free(&h.api, out.ptr, out.len)
	}
	if uint64(out.len) > math.MaxInt32 || out.ptr == nil && out.len > 0 {
		return nil, errors.New("invalid host callback buffer")
	}
	if rc != 0 {
		return nil, errors.New("host callback failed")
	}
	if out.len == 0 {
		return nil, errors.New("empty host callback response")
	}
	return C.GoBytes(out.ptr, C.int(out.len)), nil
}
