package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrBridgeShuttingDown is returned when shutdown has closed the host-callback
// gate. Native callbacks cannot be cancelled once entered, so shutdown rejects
// new callbacks and drains callbacks that already acquired the gate.
var ErrBridgeShuttingDown = errors.New("host bridge is shutting down")

// Caller invokes one raw host callback. The production implementation lives
// in the CGO ABI layer; tests provide fakes.
type Caller interface {
	// Call sends request JSON to the named host callback and returns the raw
	// envelope bytes produced by the host.
	Call(method string, request []byte) ([]byte, error)
}

// Bridge provides typed host callbacks over a raw Caller.
type Bridge struct {
	mu        sync.Mutex
	drained   *sync.Cond
	caller    Caller
	accepting bool
	inFlight  int
}

// NewBridge wraps a raw Caller.
func NewBridge(caller Caller) *Bridge {
	b := &Bridge{caller: caller, accepting: true}
	b.drained = sync.NewCond(&b.mu)
	return b
}

// Quiesce closes the callback gate. Calls that already acquired the gate are
// allowed to finish and remain visible to Drain.
func (b *Bridge) Quiesce() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.accepting = false
	b.mu.Unlock()
}

// Drain closes the callback gate and waits until all callbacks that already
// acquired it have fully returned. In particular, this includes an HTTPDo
// worker whose caller-facing context has already timed out.
//
// The wait is intentionally unbounded. ABI v1 host callbacks carry no
// cancellation, so a worker goroutine still inside the host's call function
// pointer cannot be revoked or abandoned: returning from terminal native
// shutdown while it runs would let the host release the host API table and
// unload the library underneath live frames (use-after-free). A bounded or
// timed drain is therefore unsafe at this ABI. Audited-host consequence: if
// a host callback (typically host.http.do with no host-side deadline) never
// returns, terminal native shutdown blocks inside the host's unload path
// until CPA itself exits. See docs/troubleshooting.md.
func (b *Bridge) Drain() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.accepting = false
	for b.inFlight != 0 {
		b.drained.Wait()
	}
	b.mu.Unlock()
}

func (b *Bridge) acquireCaller() (Caller, error) {
	if b == nil {
		return nil, fmt.Errorf("host bridge is unavailable")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.accepting {
		return nil, ErrBridgeShuttingDown
	}
	if b.caller == nil {
		return nil, fmt.Errorf("host bridge is unavailable")
	}
	b.inFlight++
	return b.caller, nil
}

func (b *Bridge) releaseCaller() {
	b.mu.Lock()
	b.inFlight--
	if b.inFlight == 0 {
		b.drained.Broadcast()
	}
	b.mu.Unlock()
}

func marshalRequest(method string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", method, err)
	}
	return raw, nil
}

func invoke(caller Caller, method string, raw []byte, out any) error {
	respRaw, errCall := caller.Call(method, raw)
	if errCall != nil {
		return fmt.Errorf("host callback %s: %w", method, errCall)
	}
	var env Envelope
	if errUnmarshal := json.Unmarshal(respRaw, &env); errUnmarshal != nil {
		return fmt.Errorf("decode %s envelope: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("host callback %s failed: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("host callback %s failed", method)
	}
	if out == nil {
		return nil
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		return fmt.Errorf("decode %s result: %w", method, errUnmarshal)
	}
	return nil
}

// call marshals payload, invokes the callback, and decodes the envelope.
func (b *Bridge) call(method string, payload any, out any) error {
	raw, errMarshal := marshalRequest(method, payload)
	if errMarshal != nil {
		return errMarshal
	}
	caller, errAcquire := b.acquireCaller()
	if errAcquire != nil {
		return errAcquire
	}
	defer b.releaseCaller()
	return invoke(caller, method, raw, out)
}

// AuthList lists all credentials exposed by the host.
func (b *Bridge) AuthList(ctx context.Context) ([]AuthEntry, error) {
	_ = ctx // the native host callback carries no context
	var resp AuthListResponse
	if err := b.call(MethodHostAuthList, struct{}{}, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// AuthGet fetches the complete physical credential JSON by auth index.
func (b *Bridge) AuthGet(ctx context.Context, authIndex string) (AuthGetResponse, error) {
	_ = ctx
	var resp AuthGetResponse
	if err := b.call(MethodHostAuthGet, AuthGetRequest{AuthIndex: authIndex}, &resp); err != nil {
		return AuthGetResponse{}, err
	}
	return resp, nil
}

// AuthGetRuntime fetches the current runtime health entry for one auth index
// without touching credential JSON.
//
// Audited host behavior (CLIProxyAPI 81e1b5374f99c212f196f34956eeed964a46b8fa,
// internal/pluginhost/auth_callbacks.go): the callback resolves the index by
// iterating the core auth manager's list (brief internal manager locking, the
// host mutex is only taken to read the manager pointer) and performs at most
// one os.Stat; no lock is held across blocking I/O and the callback never
// re-enters plugin code. It is therefore a fast local call like AuthGet and
// runs synchronously on the caller's goroutine: it participates in the
// in-flight gate and is drained during native shutdown. The host returns an
// RPC error (not an empty entry) for unknown indexes and for disabled auths
// whose physical file has disappeared, so callers must treat an error as
// "no usable runtime observation", never as a health transition.
func (b *Bridge) AuthGetRuntime(ctx context.Context, authIndex string) (AuthEntry, error) {
	_ = ctx
	var resp AuthGetRuntimeResponse
	if err := b.call(MethodHostAuthGetRuntime, AuthGetRequest{AuthIndex: authIndex}, &resp); err != nil {
		return AuthEntry{}, err
	}
	return resp.Auth, nil
}

// AuthSave persists a complete credential JSON document to the named auth
// file. Callers must have re-read the document immediately before mutating
// it, because host.auth.save is whole-document replacement.
func (b *Bridge) AuthSave(ctx context.Context, name string, doc json.RawMessage) error {
	_ = ctx
	var resp AuthSaveResponse
	return b.call(MethodHostAuthSave, AuthSaveRequest{Name: name, JSON: doc}, &resp)
}

// HTTPDo performs an upstream HTTP request through the host's proxy-aware
// HTTP client.
//
// The native host callback carries no context, so the call runs in a worker
// goroutine and this method returns as soon as ctx is cancelled or its
// deadline passes (making request-timeout effective for the engine). The
// underlying native callback cannot be cancelled. It continues in the
// background, remains counted as in-flight, and is drained during native
// plugin shutdown before the host function table may be released.
func (b *Bridge) HTTPDo(ctx context.Context, req HTTPRequest) (HTTPResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return HTTPResponse{}, err
	}

	raw, errMarshal := marshalRequest(MethodHostHTTPDo, req)
	if errMarshal != nil {
		return HTTPResponse{}, errMarshal
	}
	caller, errAcquire := b.acquireCaller()
	if errAcquire != nil {
		return HTTPResponse{}, errAcquire
	}

	type httpResult struct {
		resp HTTPResponse
		err  error
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		defer b.releaseCaller()
		var resp HTTPResponse
		err := invoke(caller, MethodHostHTTPDo, raw, &resp)
		resultCh <- httpResult{resp: resp, err: err}
	}()
	select {
	case <-ctx.Done():
		return HTTPResponse{}, ctx.Err()
	case result := <-resultCh:
		return result.resp, result.err
	}
}

// Log emits a sanitized log line through the host logger. Failures are
// swallowed: logging must never break reconciliation.
func (b *Bridge) Log(level, message string) {
	_ = b.call(MethodHostLog, LogRequest{Level: level, Message: message}, nil)
}
