// ABI entrypoint for the CLIProxyAPI native plugin host (ABI version 1).
//
// The host dlopens the shared library, resolves cliproxy_plugin_init, and
// exchanges JSON RPC messages through the function tables declared below.
// Buffer ownership: response buffers returned by the plugin are allocated
// with malloc and released when the host calls the plugin's free_buffer;
// buffers returned by host callbacks are released through the host's
// free_buffer.
//
// This file contains only the C boundary; all behavior lives in
// internal/plugin and is covered by pure-Go tests.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int resetPriorityPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void resetPriorityPluginFree(void*, size_t);
extern void resetPriorityPluginShutdown(void);

static const cliproxy_host_api* reset_priority_host;

static void reset_priority_store_host(const cliproxy_host_api* host) {
	reset_priority_host = host;
}

static void reset_priority_clear_host(void) {
	reset_priority_host = NULL;
}

static int reset_priority_call_host(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (reset_priority_host == NULL || reset_priority_host->call == NULL) {
		return 1;
	}
	return reset_priority_host->call(reset_priority_host->host_ctx, method, request, request_len, response);
}

static void reset_priority_free_host_buffer(void* ptr, size_t len) {
	if (reset_priority_host != NULL && reset_priority_host->free_buffer != NULL && ptr != NULL) {
		reset_priority_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/plugin"
)

var (
	runtimeOnce sync.Once
	pluginRT    *plugin.Runtime

	nativeCallMu       sync.Mutex
	nativeCallWG       sync.WaitGroup
	nativeShuttingDown bool
)

func pluginRuntime() *plugin.Runtime {
	runtimeOnce.Do(func() {
		pluginRT = plugin.NewRuntime(cgoHostCaller{})
	})
	return pluginRT
}

func beginNativeCall() bool {
	nativeCallMu.Lock()
	defer nativeCallMu.Unlock()
	if nativeShuttingDown {
		return false
	}
	nativeCallWG.Add(1)
	return true
}

func beginNativeShutdown() {
	nativeCallMu.Lock()
	nativeShuttingDown = true
	nativeCallMu.Unlock()
}

// nativeInitAllowed reports whether cliproxy_plugin_init may accept a new
// initialization. Once this resident image has entered terminal native
// shutdown, its Go-side state (runtimeOnce, the terminally shut down plugin
// runtime, and the shutdown gates) is permanently terminal and must never be
// reinitialized: dlclose does not reset a Go runtime, so a same-path native
// unload/reinstall cannot yield a fresh plugin in this process and requires a
// CPA restart instead.
func nativeInitAllowed() bool {
	nativeCallMu.Lock()
	defer nativeCallMu.Unlock()
	return !nativeShuttingDown
}

// cgoHostCaller invokes host callbacks through the stored host API.
type cgoHostCaller struct{}

// Call implements hostapi.Caller.
func (cgoHostCaller) Call(method string, request []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var requestPtr *C.uint8_t
	if len(request) > 0 {
		cRequest := C.CBytes(request)
		defer C.free(cRequest)
		requestPtr = (*C.uint8_t)(cRequest)
	}

	var response C.cliproxy_buffer
	code := C.reset_priority_call_host(cMethod, requestPtr, C.size_t(len(request)), &response)

	var raw []byte
	if response.ptr != nil && response.len > 0 {
		raw = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.reset_priority_free_host_buffer(response.ptr, response.len)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response (code %d)", method, int(code))
	}
	// A nonzero code with an envelope payload is surfaced through the
	// envelope's error by the bridge; a nonzero code without one is an
	// error here.
	if code != 0 {
		var env hostapi.Envelope
		if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || env.OK {
			return nil, fmt.Errorf("host callback %s failed with code %d", method, int(code))
		}
	}
	return raw, nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, pluginAPI *C.cliproxy_plugin_api) C.int {
	if pluginAPI == nil {
		return 1
	}
	if host != nil && host.abi_version != C.uint32_t(hostapi.ABIVersion) {
		return 1
	}
	if !nativeInitAllowed() {
		// Terminal native shutdown has begun (or completed) in this resident
		// image. Refuse initialization instead of handing out function
		// pointers into terminal Go runtime state; the host must restart CPA
		// to load this library path again.
		return 1
	}
	C.reset_priority_store_host(host)
	pluginAPI.abi_version = C.uint32_t(hostapi.ABIVersion)
	pluginAPI.call = C.cliproxy_plugin_call_fn(C.resetPriorityPluginCall)
	pluginAPI.free_buffer = C.cliproxy_plugin_free_fn(C.resetPriorityPluginFree)
	pluginAPI.shutdown = C.cliproxy_plugin_shutdown_fn(C.resetPriorityPluginShutdown)
	return 0
}

//export resetPriorityPluginCall
func resetPriorityPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if !beginNativeCall() {
		if response != nil {
			response.ptr = nil
			response.len = 0
		}
		return 1
	}
	defer nativeCallWG.Done()

	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	envelope := pluginRuntime().Dispatch(C.GoString(method), requestBytes)
	writePluginResponse(response, envelope)

	var env hostapi.Envelope
	if errUnmarshal := json.Unmarshal(envelope, &env); errUnmarshal != nil || !env.OK {
		// Nonzero returns are accepted by the host when the payload decodes
		// as the RPC error envelope, which Dispatch guarantees.
		return 1
	}
	return 0
}

//export resetPriorityPluginFree
func resetPriorityPluginFree(ptr unsafe.Pointer, length C.size_t) {
	// Buffer release is intentionally NOT gated on native shutdown: the host
	// may legally release a response buffer it still holds after shutdown has
	// begun, and refusing the free would leak the C allocation. This path
	// touches only the C heap, never Go plugin runtime state, so it needs no
	// lifecycle bookkeeping.
	_ = length
	freeNativeBuffer(ptr)
}

// freeNativeBuffer releases a C-heap allocation produced by cBytes. A nil
// pointer is a no-op.
func freeNativeBuffer(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}

// cBytes copies payload onto the C heap; ownership passes to the caller and
// is released through freeNativeBuffer (the plugin's free_buffer).
func cBytes(payload []byte) unsafe.Pointer {
	return C.CBytes(payload)
}

//export resetPriorityPluginShutdown
func resetPriorityPluginShutdown() {
	// Reject new native calls first. Runtime shutdown then closes lifecycle and
	// host-callback gates and drains plugin work plus synchronous callbacks that
	// outlived their Go contexts. Finally wait for entered ABI calls to leave
	// their response-copying code before releasing the C host table.
	beginNativeShutdown()
	pluginRuntime().Shutdown()
	nativeCallWG.Wait()
	C.reset_priority_clear_host()
}

func writePluginResponse(response *C.cliproxy_buffer, payload []byte) {
	if response == nil || len(payload) == 0 {
		return
	}
	ptr := cBytes(payload)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(payload))
}
