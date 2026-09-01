package hostapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// rawEnvelope is a verbatim host payload. Fixtures are written as literal JSON
// (never marshalled from our own structs) so a wrong struct tag cannot make the
// test agree with itself.
type rawEnvelope string

func (r rawEnvelope) caller() *scriptedCaller { return &scriptedCaller{response: []byte(r)} }

// TestAuthEntryDecodesHostAuthListWireKeys pins the host.auth.list entry keys
// against a literal host payload. The timestamp keys matter beyond bookkeeping:
// the engine's post-sentinel recovery gate accepts recovery only when
// last_refresh or the physical file modification time is newer than the
// sentinel write, so a key that fails to bind silently disables half of that
// evidence rather than producing a visible decode error.
func TestAuthEntryDecodesHostAuthListWireKeys(t *testing.T) {
	// Verbatim shape of an audited host.auth.list file entry: the modification
	// timestamp key is "modtime", not "mod_time".
	fixture := rawEnvelope(`{
	  "ok": true,
	  "result": {
	    "files": [
	      {
	        "id": "claude-a",
	        "auth_index": "i1",
	        "name": "a.json",
	        "type": "claude",
	        "provider": "claude",
	        "status": "active",
	        "source": "file",
	        "path": "/auths/a.json",
	        "size": 2048,
	        "modtime": "2026-03-04T05:06:07Z",
	        "last_refresh": "2026-03-04T04:00:00Z",
	        "email": "a@example.com",
	        "priority": 200
	      }
	    ]
	  }
	}`)

	entries, err := NewBridge(fixture.caller()).AuthList(context.Background())
	if err != nil {
		t.Fatalf("AuthList: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	entry := entries[0]

	wantModTime := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if !entry.ModTime.Equal(wantModTime) {
		t.Errorf("ModTime = %v, want %v (host key is \"modtime\")", entry.ModTime, wantModTime)
	}
	wantLastRefresh := time.Date(2026, 3, 4, 4, 0, 0, 0, time.UTC)
	if !entry.LastRefresh.Equal(wantLastRefresh) {
		t.Errorf("LastRefresh = %v, want %v", entry.LastRefresh, wantLastRefresh)
	}
	if entry.AuthIndex != "i1" || entry.Name != "a.json" || entry.Provider != "claude" {
		t.Errorf("identity fields = %+v", entry)
	}
	if entry.Status != "active" || entry.Source != "file" || entry.Path != "/auths/a.json" {
		t.Errorf("metadata fields = %+v", entry)
	}
	if entry.Email != "a@example.com" || entry.Priority != 200 {
		t.Errorf("email/priority = %q/%d", entry.Email, entry.Priority)
	}
}

// TestAuthEntryDecodesHostAuthGetRuntimeModTime covers the runtime probe path
// specifically: the health poll reads ModTime off host.auth.get_runtime entries
// to decide whether an "active" record is genuine recovery or the host's own
// save-side rebuild.
func TestAuthEntryDecodesHostAuthGetRuntimeModTime(t *testing.T) {
	fixture := rawEnvelope(`{
	  "ok": true,
	  "result": {
	    "auth": {
	      "auth_index": "i1",
	      "name": "a.json",
	      "status": "active",
	      "modtime": "2026-03-04T05:06:07Z"
	    }
	  }
	}`)

	entry, err := NewBridge(fixture.caller()).AuthGetRuntime(context.Background(), "i1")
	if err != nil {
		t.Fatalf("AuthGetRuntime: %v", err)
	}
	want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if !entry.ModTime.Equal(want) {
		t.Errorf("ModTime = %v, want %v (host key is \"modtime\")", entry.ModTime, want)
	}
}

// TestAuthEntryModTimeBindsOnlyTheHostKey guards against reintroducing the
// snake_case spelling as an accepted alias. "mod_time" is not a key the audited
// host emits, so an entry carrying only that key must leave ModTime zero.
func TestAuthEntryModTimeBindsOnlyTheHostKey(t *testing.T) {
	var entry AuthEntry
	if err := json.Unmarshal([]byte(`{"name":"a.json","mod_time":"2026-03-04T05:06:07Z"}`), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !entry.ModTime.IsZero() {
		t.Errorf("ModTime = %v, want zero: \"mod_time\" is not a host key", entry.ModTime)
	}

	raw, err := json.Marshal(AuthEntry{Name: "a.json", ModTime: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"modtime":`) {
		t.Errorf("marshalled entry missing \"modtime\" key: %s", raw)
	}
	if strings.Contains(string(raw), `"mod_time":`) {
		t.Errorf("marshalled entry emits non-host key \"mod_time\": %s", raw)
	}
}
