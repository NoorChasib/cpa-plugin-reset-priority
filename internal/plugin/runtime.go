// Package plugin implements the RPC-facing runtime: lifecycle dispatch
// (register/reconfigure/quiesce/shutdown), management route registration,
// the authenticated status (JSON and browser HTML) and refresh handlers,
// plus the static read-only browser resource shell.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/config"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/engine"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/providers"
)

// Plugin identity constants.
const (
	PluginID      = "reset-priority"
	PluginName    = "Reset Priority"
	PluginVersion = "0.1.3"
	PluginAuthor  = "NoorChasib"
	PluginRepo    = "https://github.com/NoorChasib/cpa-plugin-reset-priority"
)

// Management/resource route paths (relative; the host prefixes
// /v0/management and /v0/resource/plugins/<id> respectively).
const (
	managementStatusPath     = "/plugins/" + PluginID + "/status"
	managementStatusPagePath = "/plugins/" + PluginID + "/status/html"
	managementRefreshPath    = "/plugins/" + PluginID + "/refresh"
	resourceStatusPath       = "/status"

	// refreshRequestHeader makes the mutating management request non-simple in
	// browsers, preventing an ambient reverse-proxy management credential from
	// being ridden by a cross-origin HTML form POST.
	refreshRequestHeader      = "X-Reset-Priority-Refresh"
	refreshRequestHeaderValue = "1"
)

// Runtime dispatches host RPC calls and owns the engine lifecycle.
type Runtime struct {
	mu     sync.Mutex
	bridge *hostapi.Bridge
	clk    clock.Clock
	// runAsync lets tests run the startup reconcile synchronously.
	runAsync func(func())

	engine *engine.Engine

	dispatchMu   sync.Mutex
	dispatchWG   sync.WaitGroup
	shuttingDown bool
	shutdownOnce sync.Once
}

// Option customizes a Runtime (test hooks).
type Option func(*Runtime)

// WithClock overrides the clock (tests).
func WithClock(clk clock.Clock) Option {
	return func(r *Runtime) { r.clk = clk }
}

// WithRunAsync overrides deferred execution (tests run inline).
func WithRunAsync(run func(func())) Option {
	return func(r *Runtime) { r.runAsync = run }
}

// NewRuntime builds a Runtime over a raw host caller.
func NewRuntime(caller hostapi.Caller, opts ...Option) *Runtime {
	r := &Runtime{
		bridge:   hostapi.NewBridge(caller),
		clk:      clock.Real{},
		runAsync: func(f func()) { go f() },
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Dispatch handles one RPC method call from the host and returns envelope
// bytes. It never panics across the boundary. Native shutdown closes the
// dispatch gate and waits for calls that already entered it.
func (r *Runtime) Dispatch(method string, request []byte) (response []byte) {
	if !r.beginDispatch() {
		return errorEnvelope("plugin_shutdown", "reset-priority is shutting down")
	}
	defer r.dispatchWG.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			response = errorEnvelope("plugin_panic", fmt.Sprintf("reset-priority: internal error: %v", recovered))
		}
	}()
	switch method {
	case hostapi.MethodPluginRegister, hostapi.MethodPluginReconfigure:
		return r.handleLifecycle(request)
	case hostapi.MethodPluginQuiesce, hostapi.MethodPluginShutdown:
		// The host performs terminal shutdown through the native function
		// pointer. JSON lifecycle messages only quiesce so this Dispatch can
		// return normally and a later reconfigure may restart the engine.
		r.Quiesce()
		return okEnvelope(struct{}{})
	case hostapi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case hostapi.MethodManagementHandle:
		return r.handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "reset-priority does not handle method "+method)
	}
}

func (r *Runtime) beginDispatch() bool {
	if r == nil {
		return false
	}
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	if r.shuttingDown {
		return false
	}
	r.dispatchWG.Add(1)
	return true
}

// Quiesce stops engine scheduling while retaining status and allowing a later
// lifecycle reconfigure to restart it.
func (r *Runtime) Quiesce() {
	if r == nil {
		return
	}
	r.mu.Lock()
	eng := r.engine
	r.mu.Unlock()
	if eng != nil {
		eng.Stop()
	}
}

// Shutdown performs terminal native shutdown. It gates new dispatches and host
// callbacks, quiesces promptly, then drains entered dispatches, engine-owned
// work, and native callback workers before returning to the ABI layer.
func (r *Runtime) Shutdown() {
	if r == nil {
		return
	}
	r.shutdownOnce.Do(func() {
		r.dispatchMu.Lock()
		r.shuttingDown = true
		r.dispatchMu.Unlock()

		// Close the outbound gate before stopping work. A synchronous native
		// callback that already entered cannot be cancelled; Bridge.Drain below
		// waits for it to return.
		r.bridge.Quiesce()
		r.Quiesce()

		// No lifecycle dispatch can still install or reconfigure an engine after
		// this wait, so the final engine pointer is stable.
		r.dispatchWG.Wait()
		r.mu.Lock()
		eng := r.engine
		r.mu.Unlock()
		if eng != nil {
			eng.Shutdown()
		}
		r.bridge.Drain()
	})
}

// handleLifecycle processes plugin.register / plugin.reconfigure. The
// request's config_yaml field is a []byte on the wire (base64 via standard
// encoding/json), containing the preserved plugins.configs.reset-priority
// YAML subtree.
func (r *Runtime) handleLifecycle(request []byte) []byte {
	var req hostapi.LifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "decode lifecycle request: malformed JSON")
		}
	}
	cfg, errCfg := config.Parse(req.ConfigYAML)
	if errCfg != nil {
		return errorEnvelope("invalid_config", errCfg.Error())
	}

	r.mu.Lock()
	if r.engine == nil {
		r.engine = r.buildEngineLocked(cfg)
		if cfg.Enabled {
			r.engine.Start()
		}
		r.mu.Unlock()
	} else {
		eng := r.engine
		r.mu.Unlock()
		eng.Reconfigure(cfg)
	}

	// Negotiate the schema: never claim more than the host offered, never
	// more than we implement. A missing/zero host schema means the original
	// contract (1).
	schema := hostapi.SchemaVersion
	if req.SchemaVersion == 0 {
		schema = 1
	} else if req.SchemaVersion < schema {
		schema = req.SchemaVersion
	}
	return okEnvelope(hostapi.Registration{
		SchemaVersion: schema,
		Metadata: hostapi.Metadata{
			Name:             PluginName,
			Version:          PluginVersion,
			Author:           PluginAuthor,
			GitHubRepository: PluginRepo,
			ConfigFields:     configFields(),
		},
		Capabilities: hostapi.Capabilities{ManagementAPI: true},
	})
}

// buildEngineLocked wires the production engine over the host bridge.
func (r *Runtime) buildEngineLocked(cfg config.Config) *engine.Engine {
	now := r.clk.Now
	return engine.New(cfg, engine.Deps{
		Clock: r.clk,
		Host:  r.bridge,
		Providers: map[string]providers.Provider{
			config.ProviderClaude: providers.NewClaude(r.bridge, now),
			config.ProviderCodex:  providers.NewCodex(r.bridge, now),
		},
		Log:      func(level, message string) { r.bridge.Log(level, message) },
		RunAsync: r.runAsync,
	})
}

// managementRegistration declares the plugin routes.
//
// Wire casing (audited): the wrapper keys are lowercase routes/resources;
// route fields are the capitalized Go names because the upstream structs are
// untagged. Authenticated GET routes must leave Menu empty, otherwise the
// host converts them into unauthenticated legacy resource routes.
func managementRegistration() hostapi.ManagementRegistration {
	return hostapi.ManagementRegistration{
		Routes: []hostapi.ManagementRoute{
			{
				Method:      "GET",
				Path:        managementStatusPath,
				Description: "Reset-priority status snapshot (JSON)",
			},
			{
				Method:      "GET",
				Path:        managementStatusPagePath,
				Description: "Reset-priority status page (browser HTML)",
			},
			{
				Method:      "POST",
				Path:        managementRefreshPath,
				Description: "Run a full reconciliation now",
			},
		},
		Resources: []hostapi.ResourceRoute{
			{
				Path:        resourceStatusPath,
				Menu:        "Reset Priority",
				Description: "Public read-only plugin information",
			},
		},
	}
}

// handleManagement dispatches management.handle requests for both the
// authenticated management routes and the read-only resource page.
func (r *Runtime) handleManagement(request []byte) []byte {
	var req hostapi.ManagementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "decode management request: malformed JSON")
		}
	}

	path := strings.TrimRight(req.Path, "/")
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "GET" && isResourcePath(path) {
		// Static read-only browser shell. Resource routes are NOT management-
		// authenticated, so this handler exposes no snapshot, performs no
		// mutations, and accepts no operations regardless of query parameters.
		// The shell's embedded script may upgrade the view client-side by
		// fetching the authenticated management HTML route, but only with
		// credentials the browser itself already holds.
		return okEnvelope(htmlResponse(200, renderStatusPage()))
	}

	r.mu.Lock()
	eng := r.engine
	r.mu.Unlock()
	if eng == nil {
		return okEnvelope(jsonResponse(503, map[string]string{"error": "plugin is not registered yet"}))
	}

	switch {
	case method == "GET" && strings.HasSuffix(path, managementStatusPagePath) && !isResourcePath(path):
		// Authenticated browser HTML status view. Renders only the published,
		// sanitized snapshot through an auto-escaping template; the account
		// label (which may carry an email) is never rendered.
		page, errPage := renderManagementStatusPage(eng.Status())
		if errPage != nil {
			return okEnvelope(jsonResponse(500, map[string]string{"error": "render status page failed"}))
		}
		return okEnvelope(htmlResponse(200, page))

	case method == "GET" && strings.HasSuffix(path, managementStatusPath) && !isResourcePath(path):
		return okEnvelope(jsonResponse(200, eng.Status()))

	case method == "POST" && strings.HasSuffix(path, managementRefreshPath) && !isResourcePath(path):
		// Authenticated "Refresh now" action (spec section 15). The custom
		// non-simple header blocks cross-origin form submissions when a reverse
		// proxy makes CPA management authentication ambient. Browser requests must
		// also be explicitly same-origin (or navigation-origin "none") so a sibling
		// site cannot use CPA's broad CORS preflight behavior to ride proxy auth.
		if !refreshRequestAllowed(req.Headers) {
			return okEnvelope(jsonResponse(403, map[string]string{
				"status": "forbidden",
				"detail": "refresh requires the plugin CSRF request header and same-origin browser metadata",
			}))
		}
		ctx := hostapi.WithHostCallbackID(context.Background(), req.HostCallbackID)
		switch eng.Reconcile(ctx) {
		case engine.ReconcileResultSuccess:
			return okEnvelope(jsonResponse(200, map[string]string{
				"status": "ok",
				"detail": "reconciliation completed",
			}))
		case engine.ReconcileResultNoOp:
			return okEnvelope(jsonResponse(409, map[string]string{
				"status": "no_op",
				"detail": "reconciliation did not complete because the plugin is disabled or stopping",
			}))
		default:
			return okEnvelope(jsonResponse(503, map[string]string{
				"status": "error",
				"detail": "roster reconciliation deferred; existing roster retained",
			}))
		}

	default:
		return okEnvelope(jsonResponse(404, map[string]string{"error": "unknown route"}))
	}
}

func refreshRequestAllowed(headers map[string][]string) bool {
	if !headerHasToken(headers, refreshRequestHeader, refreshRequestHeaderValue) {
		return false
	}

	fetchSite, hasFetchSite := headerTokens(headers, "Sec-Fetch-Site")
	if hasFetchSite {
		if len(fetchSite) == 0 {
			return false
		}
		for _, site := range fetchSite {
			if !strings.EqualFold(site, "same-origin") && !strings.EqualFold(site, "none") {
				return false
			}
		}
		return true
	}

	// Non-browser management clients normally send neither Origin nor fetch
	// metadata. If Origin is present, fail closed unless the browser also supplied
	// an explicitly allowed Sec-Fetch-Site value above.
	return !headerPresent(headers, "Origin")
}

func headerHasToken(headers map[string][]string, name, token string) bool {
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(part), token) {
					return true
				}
			}
		}
	}
	return false
}

func headerTokens(headers map[string][]string, name string) ([]string, bool) {
	var tokens []string
	present := false
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		present = true
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				if token := strings.TrimSpace(part); token != "" {
					tokens = append(tokens, token)
				}
			}
		}
	}
	return tokens, present
}

func headerPresent(headers map[string][]string, name string) bool {
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return true
		}
	}
	return false
}

func isResourcePath(path string) bool {
	return strings.Contains(path, "/v0/resource/") || !strings.Contains(path, "/v0/management")
}

func configFields() []hostapi.ConfigField {
	return []hostapi.ConfigField{
		{Name: "enabled", Type: "boolean", Description: "Enable the reset-priority plugin."},
		{Name: "priority", Type: "integer", Description: "CPA plugin load/order priority. NOT related to the credential priorities this plugin manages."},
		{Name: "reconcile-interval", Type: "string", Description: "Background reconciliation interval (default 1h). Exact reset deadlines fire independently."},
		{Name: "request-timeout", Type: "string", Description: "Per provider quota request timeout (default 10s)."},
		{Name: "priority-floor", Type: "integer", Description: "Priority of the latest-resetting healthy account (default 100)."},
		{Name: "priority-step", Type: "integer", Description: "Priority gap between adjacent ranks (default 100)."},
		{Name: "quarantine-priority", Type: "integer", Description: "Sentinel priority for disabled/reauth-required accounts (default 0)."},
		{Name: "manage-claude", Type: "boolean", Description: "Manage Claude OAuth credentials (default true)."},
		{Name: "manage-codex", Type: "boolean", Description: "Manage Codex OAuth credentials (default true)."},
		{Name: "dry-run", Type: "boolean", Description: "Compute and report priorities without writing auth files (default false; recommended true for first install)."},
		{Name: "display-timezone", Type: "string", Description: "IANA time zone for timestamps on the HTML status view, e.g. America/Los_Angeles, or \"local\" for the host zone (default UTC). Presentation only."},
	}
}

func jsonResponse(status int, payload any) hostapi.ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":"encode response failed"}`)
		status = 500
	}
	return hostapi.ManagementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body,
	}
}

func htmlResponse(status int, body []byte) hostapi.ManagementResponse {
	return hostapi.ManagementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
		},
		Body: body,
	}
}

func okEnvelope(result any) []byte {
	raw, err := json.Marshal(result)
	if err != nil {
		return errorEnvelope("encode_failed", "encode result failed")
	}
	env, errEnv := json.Marshal(hostapi.Envelope{OK: true, Result: raw})
	if errEnv != nil {
		return errorEnvelope("encode_failed", "encode envelope failed")
	}
	return env
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(hostapi.Envelope{
		OK:    false,
		Error: &hostapi.EnvelopeError{Code: code, Message: message},
	})
	return raw
}
