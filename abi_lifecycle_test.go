package main

import "testing"

// TestTerminalNativeShutdownGatesInitAndCallsButNotBufferRelease covers the
// resident-image lifecycle contract in one ordered sequence, because the
// terminal transition is package-global and irreversible within a process:
//
//  1. before terminal native shutdown, init and calls are accepted;
//  2. after terminal native shutdown, cliproxy_plugin_init must be rejected
//     (nonzero) rather than reinitialize terminal Go runtime state, and new
//     native calls are rejected;
//  3. buffer release remains available after terminal shutdown so response
//     buffers still held by the host are freed, not leaked.
func TestTerminalNativeShutdownGatesInitAndCallsButNotBufferRelease(t *testing.T) {
	if !nativeInitAllowed() {
		t.Fatal("cliproxy_plugin_init must be accepted before terminal native shutdown")
	}
	if !beginNativeCall() {
		t.Fatal("native calls must be accepted before terminal native shutdown")
	}
	nativeCallWG.Done()

	beginNativeShutdown()

	if nativeInitAllowed() {
		t.Fatal("cliproxy_plugin_init must return nonzero after terminal native shutdown")
	}
	if beginNativeCall() {
		t.Fatal("native calls must be rejected after terminal native shutdown")
	}

	// Ungated release path: freeing a live C allocation and a nil pointer
	// must both work after terminal shutdown.
	freeNativeBuffer(cBytes([]byte("host-held response buffer")))
	freeNativeBuffer(nil)
}
