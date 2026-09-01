package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/config"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
)

// baseTime is the deterministic test epoch.
var baseTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

type saveRecord struct {
	Name string
	Doc  map[string]json.RawMessage
}

// fakeHost is an in-memory HostAuth double backed by physical-JSON docs.
type fakeHost struct {
	mu         sync.Mutex
	now        func() time.Time
	entries    []hostapi.AuthEntry
	docs       map[string]json.RawMessage // by authIndex
	listErr    error
	getErr     map[string]error // by authIndex
	saveErr    map[string]error // by name
	runtimeErr map[string]error // by authIndex

	saves        []saveRecord
	getCalls     []string
	runtimeCalls []string
	listCalls    int
	// beforeGet mutates state just before AuthGet returns, simulating
	// concurrent credential refreshes.
	beforeGet func(authIndex string)
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		now:        time.Now,
		docs:       make(map[string]json.RawMessage),
		getErr:     make(map[string]error),
		saveErr:    make(map[string]error),
		runtimeErr: make(map[string]error),
	}
}

func (h *fakeHost) AuthList(ctx context.Context) ([]hostapi.AuthEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listCalls++
	if h.listErr != nil {
		return nil, h.listErr
	}
	out := make([]hostapi.AuthEntry, len(h.entries))
	copy(out, h.entries)
	return out, nil
}

func (h *fakeHost) AuthGet(ctx context.Context, authIndex string) (hostapi.AuthGetResponse, error) {
	h.mu.Lock()
	hook := h.beforeGet
	h.mu.Unlock()
	if hook != nil {
		hook(authIndex)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.getCalls = append(h.getCalls, authIndex)
	if err := h.getErr[authIndex]; err != nil {
		return hostapi.AuthGetResponse{}, err
	}
	doc, ok := h.docs[authIndex]
	if !ok {
		return hostapi.AuthGetResponse{}, fmt.Errorf("auth %s not found", authIndex)
	}
	name := ""
	path := ""
	for _, e := range h.entries {
		if e.AuthIndex == authIndex {
			name = e.Name
			path = e.Path
		}
	}
	return hostapi.AuthGetResponse{
		AuthIndex: authIndex,
		Name:      name,
		Path:      path,
		JSON:      append(json.RawMessage(nil), doc...),
	}, nil
}

// AuthGetRuntime mirrors the audited host callback: it answers from the
// runtime roster entry only (health fields, no credential JSON) and returns
// an error for unknown indexes.
func (h *fakeHost) AuthGetRuntime(ctx context.Context, authIndex string) (hostapi.AuthEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runtimeCalls = append(h.runtimeCalls, authIndex)
	if err := h.runtimeErr[authIndex]; err != nil {
		return hostapi.AuthEntry{}, err
	}
	for _, e := range h.entries {
		if e.AuthIndex == authIndex {
			return e, nil
		}
	}
	return hostapi.AuthEntry{}, fmt.Errorf("auth not found for auth_index %s", authIndex)
}

func (h *fakeHost) runtimeCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.runtimeCalls)
}

func (h *fakeHost) AuthSave(ctx context.Context, name string, doc json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.saveErr[name]; err != nil {
		return err
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(doc, &decoded); err != nil {
		return fmt.Errorf("save payload is not a JSON object: %w", err)
	}
	h.saves = append(h.saves, saveRecord{Name: name, Doc: decoded})
	for i := range h.entries {
		if h.entries[i].Name == name {
			h.docs[h.entries[i].AuthIndex] = append(json.RawMessage(nil), doc...)
			// Mirror the audited host.auth.save upsert: it re-synthesizes priority
			// but also rebuilds the runtime record as active/enabled. ModTime marks
			// the physical write so the engine can distinguish this side effect
			// from a later external refresh or operator reauthentication.
			if p, ok := parsePriorityRaw(decoded["priority"]); ok {
				h.entries[i].Priority = p
			}
			h.entries[i].Status = "active"
			h.entries[i].StatusMessage = ""
			h.entries[i].Disabled = false
			h.entries[i].Unavailable = false
			h.entries[i].ModTime = h.now()
			return nil
		}
	}
	return fmt.Errorf("no auth file named %s", name)
}

func (h *fakeHost) savesFor(name string) []saveRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []saveRecord
	for _, s := range h.saves {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func (h *fakeHost) saveCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.saves)
}

func (h *fakeHost) listCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listCalls
}

func (h *fakeHost) docPriority(t *testing.T, authIndex string) (int, bool) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(h.docs[authIndex], &decoded); err != nil {
		t.Fatalf("doc %s: %v", authIndex, err)
	}
	return parsePriorityRaw(decoded["priority"])
}

func (h *fakeHost) setEntry(entry hostapi.AuthEntry, doc map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	for i := range h.entries {
		if h.entries[i].AuthIndex == entry.AuthIndex {
			h.entries[i] = entry
			h.docs[entry.AuthIndex] = raw
			return
		}
	}
	h.entries = append(h.entries, entry)
	h.docs[entry.AuthIndex] = raw
}

func (h *fakeHost) removeEntry(authIndex string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.entries[:0]
	for _, e := range h.entries {
		if e.AuthIndex != authIndex {
			out = append(out, e)
		}
	}
	h.entries = out
	delete(h.docs, authIndex)
}

func (h *fakeHost) updateEntry(authIndex string, mutate func(*hostapi.AuthEntry)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.entries {
		if h.entries[i].AuthIndex == authIndex {
			mutate(&h.entries[i])
			return
		}
	}
	panic("no entry " + authIndex)
}

// fakeProvider returns canned observations keyed by access token.
type fakeProvider struct {
	id  string
	now func() time.Time

	mu      sync.Mutex
	resets  map[string]time.Time // token -> weekly reset
	noReset map[string]bool      // token -> respond without a weekly window
	errs    map[string]error     // token -> fetch error
	calls   map[string]int       // token -> call count
}

func newFakeProvider(id string, now func() time.Time) *fakeProvider {
	return &fakeProvider{
		id:      id,
		now:     now,
		resets:  make(map[string]time.Time),
		noReset: make(map[string]bool),
		errs:    make(map[string]error),
		calls:   make(map[string]int),
	}
}

func (p *fakeProvider) ID() string { return p.id }

func (p *fakeProvider) FetchWeeklyReset(ctx context.Context, creds providers.Credentials) (providers.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[creds.AccessToken]++
	if err := p.errs[creds.AccessToken]; err != nil {
		return providers.Observation{}, err
	}
	if p.noReset[creds.AccessToken] {
		return providers.Observation{ObservedAt: p.now()}, nil
	}
	reset, ok := p.resets[creds.AccessToken]
	if !ok {
		return providers.Observation{}, fmt.Errorf("no canned result for token")
	}
	return providers.Observation{HasWeekly: true, ResetAt: reset, ObservedAt: p.now()}, nil
}

func (p *fakeProvider) setReset(token string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resets[token] = at
	delete(p.errs, token)
	delete(p.noReset, token)
}

func (p *fakeProvider) setErr(token string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs[token] = err
}

func (p *fakeProvider) setNoWeekly(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.noReset[token] = true
	delete(p.errs, token)
	delete(p.resets, token)
}

func (p *fakeProvider) callCount(token string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[token]
}

// asyncQueue captures RunAsync work for step-by-step execution.
type asyncQueue struct {
	mu    sync.Mutex
	funcs []func()
}

func (q *asyncQueue) run(f func()) {
	q.mu.Lock()
	q.funcs = append(q.funcs, f)
	q.mu.Unlock()
}

func (q *asyncQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.funcs) == 0 {
			q.mu.Unlock()
			return
		}
		f := q.funcs[0]
		q.funcs = q.funcs[1:]
		q.mu.Unlock()
		f()
	}
}

func (q *asyncQueue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.funcs)
}

// testEnv bundles the deterministic engine test fixture.
type testEnv struct {
	t      *testing.T
	clk    *clock.Fake
	host   *fakeHost
	claude *fakeProvider
	codex  *fakeProvider
	async  *asyncQueue
	eng    *Engine
}

func defaultConfig() config.Config {
	return config.Config{
		Enabled:            true,
		ReconcileInterval:  time.Hour,
		RequestTimeout:     10 * time.Second,
		PriorityFloor:      100,
		PriorityStep:       100,
		QuarantinePriority: 0,
		ManageClaude:       true,
		ManageCodex:        true,
	}
}

func newTestEnv(t *testing.T, cfg config.Config) *testEnv {
	t.Helper()
	clk := clock.NewFake(baseTime)
	host := newFakeHost()
	host.now = clk.Now
	claude := newFakeProvider("claude", clk.Now)
	codex := newFakeProvider("codex", clk.Now)
	async := &asyncQueue{}
	eng := New(cfg, Deps{
		Clock: clk,
		Host:  host,
		Providers: map[string]providers.Provider{
			"claude": claude,
			"codex":  codex,
		},
		RunAsync: async.run,
	})
	return &testEnv{t: t, clk: clk, host: host, claude: claude, codex: codex, async: async, eng: eng}
}

func token(name string) string { return "tok-" + name }

// addAccount registers a healthy account with entry, doc, and provider reset.
func (env *testEnv) addAccount(provider, name string, resetAt time.Time) {
	env.t.Helper()
	doc := map[string]any{
		"type":          provider,
		"access_token":  token(name),
		"refresh_token": "refresh-" + token(name),
		"email":         name + "@example.com",
	}
	if provider == "codex" {
		doc["account_id"] = "account-" + name
	}
	env.host.setEntry(hostapi.AuthEntry{
		AuthIndex: "idx-" + name,
		ID:        "id-" + name,
		Name:      name + ".json",
		Provider:  provider,
		Type:      provider,
		Status:    "active",
		Source:    "file",
		Path:      "/auth/" + name + ".json",
		Email:     name + "@example.com",
	}, doc)
	p := env.provider(provider)
	if !resetAt.IsZero() {
		p.setReset(token(name), resetAt)
	} else {
		p.setNoWeekly(token(name))
	}
}

func (env *testEnv) provider(id string) *fakeProvider {
	if id == "claude" {
		return env.claude
	}
	return env.codex
}

func (env *testEnv) reconcile() {
	env.t.Helper()
	env.eng.Reconcile(context.Background(), "test")
}

// desired returns the desired priority for an account name from status.
func (env *testEnv) desired(name string) int {
	env.t.Helper()
	row, ok := env.statusRow(name)
	if !ok {
		env.t.Fatalf("account %s not in status", name)
	}
	return row.DesiredPriority
}

func (env *testEnv) statusRow(name string) (AccountStatus, bool) {
	snap := env.eng.Status()
	for _, group := range snap.Providers {
		for _, acct := range group.Accounts {
			if acct.Name == name+".json" {
				return acct, true
			}
		}
	}
	return AccountStatus{}, false
}

// assertDesired checks desired priorities by account name.
func (env *testEnv) assertDesired(want map[string]int) {
	env.t.Helper()
	for name, priority := range want {
		if got := env.desired(name); got != priority {
			env.t.Errorf("desired priority of %s = %d, want %d", name, got, priority)
		}
	}
}

// assertPhysical checks the persisted physical priorities by account name.
func (env *testEnv) assertPhysical(want map[string]int) {
	env.t.Helper()
	for name, priority := range want {
		got, ok := env.host.docPriority(env.t, "idx-"+name)
		if !ok {
			env.t.Errorf("account %s has no physical priority, want %d", name, priority)
			continue
		}
		if got != priority {
			env.t.Errorf("physical priority of %s = %d, want %d", name, got, priority)
		}
	}
}
