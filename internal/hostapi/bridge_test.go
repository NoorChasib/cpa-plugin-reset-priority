package hostapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// scriptedCaller records calls and replies from a script.
type scriptedCaller struct {
	calls    []struct{ Method, Request string }
	response []byte
	err      error
}

func (c *scriptedCaller) Call(method string, request []byte) ([]byte, error) {
	c.calls = append(c.calls, struct{ Method, Request string }{method, string(request)})
	if c.err != nil {
		return nil, c.err
	}
	return c.response, nil
}

func envelopeWith(t *testing.T, result any) []byte {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(Envelope{OK: true, Result: raw})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestBridgeAuthListDecodes(t *testing.T) {
	caller := &scriptedCaller{response: envelopeWith(t, AuthListResponse{Files: []AuthEntry{
		{AuthIndex: "i1", Name: "a.json", Provider: "claude", Priority: 200},
	}})}
	entries, err := NewBridge(caller).AuthList(context.Background())
	if err != nil {
		t.Fatalf("AuthList: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.json" || entries[0].Priority != 200 {
		t.Errorf("entries = %+v", entries)
	}
	if caller.calls[0].Method != MethodHostAuthList {
		t.Errorf("method = %s", caller.calls[0].Method)
	}
}

func TestBridgeAuthGetRequestShape(t *testing.T) {
	caller := &scriptedCaller{response: envelopeWith(t, AuthGetResponse{
		AuthIndex: "i1", Name: "a.json", JSON: json.RawMessage(`{"k":1}`),
	})}
	resp, err := NewBridge(caller).AuthGet(context.Background(), "i1")
	if err != nil {
		t.Fatalf("AuthGet: %v", err)
	}
	if resp.Name != "a.json" || string(resp.JSON) != `{"k":1}` {
		t.Errorf("resp = %+v", resp)
	}
	if got := caller.calls[0].Request; got != `{"auth_index":"i1"}` {
		t.Errorf("request = %s", got)
	}
}

func TestBridgeAuthGetRuntimeWireShapes(t *testing.T) {
	// The audited host wraps the list-shaped runtime entry in an "auth" key.
	caller := &scriptedCaller{response: envelopeWith(t, AuthGetRuntimeResponse{Auth: AuthEntry{
		AuthIndex: "i1", Name: "a.json", Provider: "claude",
		Status: "error", StatusMessage: "unauthorized", Disabled: false,
	}})}
	entry, err := NewBridge(caller).AuthGetRuntime(context.Background(), "i1")
	if err != nil {
		t.Fatalf("AuthGetRuntime: %v", err)
	}
	if entry.Name != "a.json" || entry.Status != "error" || entry.StatusMessage != "unauthorized" {
		t.Errorf("entry = %+v", entry)
	}
	if caller.calls[0].Method != MethodHostAuthGetRuntime {
		t.Errorf("method = %s, want %s", caller.calls[0].Method, MethodHostAuthGetRuntime)
	}
	if got := caller.calls[0].Request; got != `{"auth_index":"i1"}` {
		t.Errorf("request = %s", got)
	}
}

func TestBridgeAuthGetRuntimeErrorEnvelope(t *testing.T) {
	// Unknown indexes and disabled-with-missing-file auths come back as RPC
	// errors at the audited host; the bridge must surface them as errors, not
	// as empty entries a caller could misread as healthy.
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: "not_found", Message: "auth not found"}})
	caller := &scriptedCaller{response: raw}
	if _, err := NewBridge(caller).AuthGetRuntime(context.Background(), "gone"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Errorf("err = %v, want not_found", err)
	}
}

func TestBridgeAuthSaveRequestShape(t *testing.T) {
	caller := &scriptedCaller{response: envelopeWith(t, AuthSaveResponse{Name: "a.json", Path: "/x/a.json"})}
	err := NewBridge(caller).AuthSave(context.Background(), "a.json", json.RawMessage(`{"priority":100}`))
	if err != nil {
		t.Fatalf("AuthSave: %v", err)
	}
	if got := caller.calls[0].Request; got != `{"name":"a.json","json":{"priority":100}}` {
		t.Errorf("request = %s", got)
	}
	if caller.calls[0].Method != MethodHostAuthSave {
		t.Errorf("method = %s", caller.calls[0].Method)
	}
}

func TestBridgeHTTPDoWireShapes(t *testing.T) {
	// The host returns the untagged pluginapi.HTTPResponse: capitalized
	// keys and a base64 body.
	body := base64.StdEncoding.EncodeToString([]byte(`{"seven_day":{}}`))
	raw := fmt.Sprintf(`{"ok":true,"result":{"StatusCode":200,"Headers":{"Content-Type":["application/json"]},"Body":%q}}`, body)
	caller := &scriptedCaller{response: []byte(raw)}

	ctx := WithHostCallbackID(context.Background(), "request-context-123")
	resp, err := NewBridge(caller).HTTPDo(ctx, HTTPRequest{
		Method:  "GET",
		URL:     "https://example.com/usage",
		Headers: map[string][]string{"Authorization": {"Bearer x"}},
	})
	if err != nil {
		t.Fatalf("HTTPDo: %v", err)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"seven_day":{}}` {
		t.Errorf("resp = %+v", resp)
	}

	// The request uses snake_case keys and carries the management callback ID
	// so the host can resolve the originating request's cancellation context.
	req := caller.calls[0].Request
	for _, want := range []string{
		`"host_callback_id":"request-context-123"`,
		`"method":"GET"`,
		`"url":"https://example.com/usage"`,
		`"headers":{"Authorization":["Bearer x"]}`,
	} {
		if !strings.Contains(req, want) {
			t.Errorf("request missing %s: %s", want, req)
		}
	}
}

// gatedCaller blocks Call until released, recording lifecycle events.
type gatedCaller struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	response []byte
}

func (c *gatedCaller) Call(method string, request []byte) ([]byte, error) {
	close(c.started)
	<-c.release
	close(c.finished)
	return c.response, nil
}

// TestBridgeHTTPDoHonorsContextCancellation: HTTPDo must return as soon as
// the context ends, while the non-cancellable native callback continues in
// the background and remains eligible for shutdown draining.
func TestBridgeHTTPDoHonorsContextCancellation(t *testing.T) {
	caller := &gatedCaller{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
		response: envelopeWith(t, HTTPResponse{StatusCode: 200}),
	}
	bridge := NewBridge(caller)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := bridge.HTTPDo(ctx, HTTPRequest{Method: "GET", URL: "https://example.com"})
		errCh <- err
	}()

	<-caller.started // the native call is in flight and blocked
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("HTTPDo did not return after context cancellation")
	}

	// The abandoned native call still completes. Native shutdown is
	// responsible for draining it before the host callback table is released.
	close(caller.release)
	select {
	case <-caller.finished:
	case <-time.After(5 * time.Second):
		t.Fatalf("native callback did not complete after release")
	}
}

// TestBridgeHTTPDoHonorsDeadline: an expired deadline surfaces as
// DeadlineExceeded even though the native call never returns in time.
func TestBridgeHTTPDoHonorsDeadline(t *testing.T) {
	caller := &gatedCaller{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
		response: envelopeWith(t, HTTPResponse{StatusCode: 200}),
	}
	t.Cleanup(func() {
		close(caller.release)
		select {
		case <-caller.finished:
		case <-time.After(5 * time.Second):
			t.Errorf("native callback did not complete during cleanup")
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewBridge(caller).HTTPDo(ctx, HTTPRequest{Method: "GET", URL: "https://example.com"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestBridgeQuiesceRejectsNewCallbacksAndDrainWaits(t *testing.T) {
	caller := &gatedCaller{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
		response: envelopeWith(t, HTTPResponse{StatusCode: 200}),
	}
	bridge := NewBridge(caller)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := bridge.HTTPDo(ctx, HTTPRequest{Method: "GET", URL: "https://example.com"})
		errCh <- err
	}()
	released := false
	defer func() {
		if !released {
			close(caller.release)
		}
	}()
	<-caller.started
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("HTTPDo err = %v, want context.Canceled", err)
	}

	bridge.Quiesce()
	if _, err := bridge.AuthList(context.Background()); !errors.Is(err, ErrBridgeShuttingDown) {
		t.Fatalf("new callback err = %v, want ErrBridgeShuttingDown", err)
	}

	drained := make(chan struct{})
	go func() {
		bridge.Drain()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatalf("Drain returned while native callback was still in flight")
	case <-time.After(20 * time.Millisecond):
	}

	close(caller.release)
	released = true
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatalf("Drain did not return after native callback completed")
	}
}

func TestBridgeErrorEnvelope(t *testing.T) {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: "not_found", Message: "no auth"}})
	caller := &scriptedCaller{response: raw}
	_, err := NewBridge(caller).AuthGet(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Errorf("err = %v, want not_found", err)
	}
}

func TestBridgeCallerError(t *testing.T) {
	caller := &scriptedCaller{err: errors.New("dlopen sadness")}
	if _, err := NewBridge(caller).AuthList(context.Background()); err == nil {
		t.Errorf("want error when caller fails")
	}
}

func TestBridgeMalformedEnvelope(t *testing.T) {
	caller := &scriptedCaller{response: []byte("not json")}
	if _, err := NewBridge(caller).AuthList(context.Background()); err == nil {
		t.Errorf("want error on malformed envelope")
	}
}

func TestLifecycleRequestConfigYAMLIsBase64(t *testing.T) {
	// []byte fields travel as base64 through encoding/json — the wire form
	// the host produces for config_yaml.
	yaml := "enabled: true\n"
	raw, err := json.Marshal(LifecycleRequest{ConfigYAML: []byte(yaml), SchemaVersion: 4})
	if err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString([]byte(yaml))
	if !strings.Contains(string(raw), fmt.Sprintf("%q", want)) {
		t.Errorf("config_yaml not base64 on the wire: %s", raw)
	}
	var decoded LifecycleRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.ConfigYAML) != yaml {
		t.Errorf("round trip = %q, want %q", decoded.ConfigYAML, yaml)
	}
}
