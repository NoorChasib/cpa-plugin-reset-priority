package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
)

// TestWriteAndRosterErrorLogsEmitOutsideEngineMu guards the cross-ABI logging
// rule: the production Logger crosses the native host.log boundary and may
// block or re-enter plugin code that takes Engine.mu (for example a status
// dispatch), so no engine log may be emitted while Engine.mu is held.
//
// Both audited regression paths are exercised: recordWriteError (save
// failure) and the deferred-roster warning. The probe runs on the same
// goroutine as the emit and no other engine goroutine is live at emit time
// (RunAsync is captured, roster failure schedules no fetches, and writeback
// runs after fetchAll has joined), so a failed TryLock proves the emitting
// goroutine itself holds Engine.mu.
func TestWriteAndRosterErrorLogsEmitOutsideEngineMu(t *testing.T) {
	clk := clock.NewFake(baseTime)
	host := newFakeHost()
	claude := newFakeProvider("claude", clk.Now)
	async := &asyncQueue{}

	var eng *Engine
	var logMu sync.Mutex
	var lines []string
	var heldDuringLog []string
	log := func(level, message string) {
		logMu.Lock()
		defer logMu.Unlock()
		lines = append(lines, message)
		if eng == nil {
			return
		}
		if eng.mu.TryLock() {
			eng.mu.Unlock()
		} else {
			heldDuringLog = append(heldDuringLog, message)
		}
	}
	eng = New(defaultConfig(), Deps{
		Clock:     clk,
		Host:      host,
		Providers: map[string]providers.Provider{"claude": claude},
		Log:       log,
		RunAsync:  async.run,
	})

	host.setEntry(hostapi.AuthEntry{
		AuthIndex: "idx-a",
		ID:        "id-a",
		Name:      "a.json",
		Provider:  "claude",
		Type:      "claude",
		Status:    "active",
		Source:    "file",
		Path:      "/auth/a.json",
	}, map[string]any{
		"type":          "claude",
		"access_token":  token("a"),
		"refresh_token": "refresh-" + token("a"),
	})
	claude.setReset(token("a"), day(1))

	host.mu.Lock()
	host.saveErr["a.json"] = errFake("save backend unavailable")
	host.mu.Unlock()
	eng.Reconcile(context.Background())

	host.mu.Lock()
	host.listErr = errFake("host list unavailable")
	host.mu.Unlock()
	eng.Reconcile(context.Background())

	logMu.Lock()
	defer logMu.Unlock()
	var sawSaveFailure, sawRosterDeferred bool
	for _, line := range lines {
		if strings.Contains(line, "save failed:") {
			sawSaveFailure = true
		}
		if strings.Contains(line, "auth roster reconciliation deferred:") {
			sawRosterDeferred = true
		}
	}
	if !sawSaveFailure {
		t.Fatalf("save failure was not logged; lines: %q", lines)
	}
	if !sawRosterDeferred {
		t.Fatalf("deferred roster error was not logged; lines: %q", lines)
	}
	if len(heldDuringLog) != 0 {
		t.Fatalf("log lines emitted while Engine.mu was held: %q", heldDuringLog)
	}
}
