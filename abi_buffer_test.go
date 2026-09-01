package main

import (
	"errors"
	"testing"
)

func TestHostResponseRejectsOversizedLengthAndReleasesBufferExactlyOnce(t *testing.T) {
	ptr := cBytes([]byte{0})
	defer freeNativeBuffer(ptr)

	releaseCount := 0
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Errorf("oversized host response panicked instead of being rejected: %v", recovered)
		}
		if releaseCount != 1 {
			t.Errorf("host-owned buffer release count = %d, want 1", releaseCount)
		}
	}()

	raw, err := consumeHostResponse(ptr, maxCGoBytesLength+1, func() {
		releaseCount++
	})
	if !errors.Is(err, errNativeBufferTooLarge) {
		t.Fatalf("consumeHostResponse error = %v, want %v", err, errNativeBufferTooLarge)
	}
	if raw != nil {
		t.Fatalf("consumeHostResponse returned %d bytes for an oversized response", len(raw))
	}
}

func TestPluginRequestRejectsOversizedLengthWithoutReadingBuffer(t *testing.T) {
	ptr := cBytes([]byte{0})
	defer freeNativeBuffer(ptr)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Errorf("oversized plugin request panicked instead of being rejected: %v", recovered)
		}
	}()

	raw, err := readPluginRequest(ptr, maxCGoBytesLength+1)
	if !errors.Is(err, errNativeBufferTooLarge) {
		t.Fatalf("readPluginRequest error = %v, want %v", err, errNativeBufferTooLarge)
	}
	if raw != nil {
		t.Fatalf("readPluginRequest returned %d bytes for an oversized request", len(raw))
	}
}
