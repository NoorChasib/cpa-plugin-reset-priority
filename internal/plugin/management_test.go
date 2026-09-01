package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/hostapi"
)

func TestManagementRegistrationRouteCasing(t *testing.T) {
	rt := newTestRuntime(t, newFakeHostCaller())
	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodManagementRegister, nil))
	if !env.OK {
		t.Fatalf("management.register failed: %+v", env.Error)
	}

	// The wrapper keys are lowercase; route fields use the capitalized Go
	// names because the upstream structs carry no JSON tags.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(env.Result, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, hasRoutes := decoded["routes"]; !hasRoutes {
		t.Fatalf("registration missing lowercase \"routes\" key: %s", env.Result)
	}
	if _, hasResources := decoded["resources"]; !hasResources {
		t.Fatalf("registration missing lowercase \"resources\" key: %s", env.Result)
	}

	var routes []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["routes"], &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(routes))
	}
	for _, route := range routes {
		for _, key := range []string{"Method", "Path"} {
			if _, present := route[key]; !present {
				t.Errorf("route missing capitalized %q key: %v", key, route)
			}
		}
		// Authenticated GET routes must not carry Menu: the host converts
		// GET+Menu routes into UNAUTHENTICATED legacy resources.
		if menu, present := route["Menu"]; present && string(menu) != `""` {
			t.Errorf("management route declares Menu %s; it would become unauthenticated", menu)
		}
	}

	var seen []string
	for _, route := range routes {
		var method, path string
		_ = json.Unmarshal(route["Method"], &method)
		_ = json.Unmarshal(route["Path"], &path)
		seen = append(seen, method+" "+path)
	}
	want := map[string]bool{
		"GET /plugins/reset-priority/status":   true,
		"POST /plugins/reset-priority/refresh": true,
	}
	for _, s := range seen {
		if !want[s] {
			t.Errorf("unexpected route %q", s)
		}
		delete(want, s)
	}
	for missing := range want {
		t.Errorf("missing route %q", missing)
	}

	var resources []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["resources"], &resources); err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 {
		t.Fatalf("resources = %d, want 1", len(resources))
	}
	var resourcePath string
	_ = json.Unmarshal(resources[0]["Path"], &resourcePath)
	if resourcePath != "/status" {
		t.Errorf("resource path = %q, want /status", resourcePath)
	}
	if _, present := resources[0]["Menu"]; !present {
		t.Errorf("resource route should declare a Menu label")
	}
}

func TestManagementStatusRouteReturnsSanitizedJSON(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	caller.addClaudeAccount("b", testBase.Add(24*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")

	resp := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Headers["Content-Type"]; len(ct) == 0 || !strings.Contains(ct[0], "application/json") {
		t.Errorf("content type = %v", ct)
	}

	body := string(resp.Body)
	// Both accounts, ranked: b resets first (200), a second (100).
	var snap struct {
		Providers []struct {
			Provider string `json:"provider"`
			Accounts []struct {
				Name            string `json:"name"`
				DesiredPriority int    `json:"desired_priority"`
				ResetState      string `json:"reset_state"`
				ResetAtUTC      string `json:"reset_at_utc"`
			} `json:"accounts"`
		} `json:"providers"`
		DryRun bool `json:"dry_run"`
	}
	if err := json.Unmarshal(resp.Body, &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Providers) == 0 || len(snap.Providers[0].Accounts) != 2 {
		t.Fatalf("unexpected snapshot: %s", body)
	}
	first := snap.Providers[0].Accounts[0]
	if first.Name != "b.json" || first.DesiredPriority != 200 {
		t.Errorf("top account = %s/%d, want b.json/200", first.Name, first.DesiredPriority)
	}
	if first.ResetState != "confirmed" || first.ResetAtUTC == "" {
		t.Errorf("status lacks exact reset info: %+v", first)
	}

	// Secret safety: no access token, refresh token, or bearer material.
	for _, secret := range []string{testAccessToken, "refresh-token-super-secret", "Bearer "} {
		if strings.Contains(body, secret) {
			t.Errorf("management status leaks %q", secret)
		}
	}
}

func TestManagementRefreshRouteTriggersReconcile(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")

	before := caller.httpCallCount()
	resp := managementCall(t, rt, "POST", "/v0/management/plugins/reset-priority/refresh", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("refresh = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), `"status":"ok"`) {
		t.Errorf("refresh body = %s", resp.Body)
	}
	if caller.httpCallCount() <= before {
		t.Errorf("refresh did not trigger provider fetches")
	}
}

func TestManagementRefreshCannotActivateDisabledRegistration(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: false\n")

	resp := managementCall(t, rt, "POST", "/v0/management/plugins/reset-priority/refresh", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("disabled refresh = %d, want an inert 200 response", resp.StatusCode)
	}
	if got := caller.httpCallCount(); got != 0 {
		t.Fatalf("disabled refresh performed %d provider fetches, want 0", got)
	}
	if got := caller.saveCount(); got != 0 {
		t.Fatalf("disabled refresh performed %d saves, want 0", got)
	}
	snap := statusSnapshot(t, rt)
	if !snap.Stopped || snap.Enabled {
		t.Fatalf("disabled refresh changed engine state: stopped=%v enabled=%v", snap.Stopped, snap.Enabled)
	}
	if snap.NextReconcileAt != nil || snap.NextDeadlineAt != nil {
		t.Fatalf("disabled refresh armed timers: reconcile=%v deadline=%v", snap.NextReconcileAt, snap.NextDeadlineAt)
	}
}

func TestResourceStatusPageIsStaticAndReadOnly(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	caller.addClaudeAccount("broken", testBase.Add(24*time.Hour))
	caller.mu.Lock()
	caller.httpBody[testAccessToken+"-broken"] = "{"
	caller.mu.Unlock()
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")
	savesAfterRegister := caller.saveCount()
	snap := statusSnapshot(t, rt)

	// Hostile query parameters must be inert: the resource route is not
	// management-authenticated and must never mutate anything.
	resp := managementCall(t, rt, "GET", "/v0/resource/plugins/reset-priority/status", map[string][]string{
		"op":   {"save"},
		"name": {"a.json"},
		"json": {`{"priority":9999}`},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("resource page = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Headers["Content-Type"]; len(ct) == 0 || !strings.Contains(ct[0], "text/html") {
		t.Errorf("content type = %v", ct)
	}
	if caller.saveCount() != savesAfterRegister {
		t.Errorf("resource page performed a mutation: saves %d -> %d", savesAfterRegister, caller.saveCount())
	}

	page := string(resp.Body)
	for _, marker := range []string{
		"<title>Reset Priority</title>",
		`data-reset-priority-status="ready"`,
		"read-only shell",
		"Dry-run configuration recommended",
		"GET /v0/management/plugins/reset-priority/status",
	} {
		if !strings.Contains(page, marker) {
			t.Errorf("resource page missing static marker %q", marker)
		}
	}

	// The public resource must not render any account-level snapshot fields.
	var checkedError bool
	for _, group := range snap.Providers {
		for _, account := range group.Accounts {
			for _, dynamic := range []string{
				account.Label,
				account.ResetAt,
				account.ResetAtUTC,
				account.LastSuccessAt,
				account.LastError,
				account.WriteError,
				fmt.Sprintf("%d", account.DesiredPriority),
			} {
				if dynamic != "" && strings.Contains(page, dynamic) {
					t.Errorf("public resource contains account-level value %q", dynamic)
				}
			}
			if account.LastError != "" {
				checkedError = true
			}
			if account.CurrentPriority != nil && strings.Contains(page, fmt.Sprintf("%d", *account.CurrentPriority)) {
				t.Errorf("public resource contains current priority %d", *account.CurrentPriority)
			}
		}
	}
	if !checkedError {
		t.Fatalf("test setup did not produce an account error")
	}
	for _, heading := range []string{"Weekly reset", "Current</th>", "Desired</th>", "Last error"} {
		if strings.Contains(page, heading) {
			t.Errorf("public resource contains account-status heading %q", heading)
		}
	}
	for _, secret := range []string{testAccessToken, "refresh-token-super-secret"} {
		if strings.Contains(page, secret) {
			t.Errorf("resource page leaks %q", secret)
		}
	}
}

func TestManagementUnknownRouteReturns404(t *testing.T) {
	caller := newFakeHostCaller()
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")

	resp := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/nope", nil)
	if resp.StatusCode != 404 {
		t.Errorf("unknown route = %d, want 404", resp.StatusCode)
	}
	// A POST to the read-only status route is also rejected.
	resp = managementCall(t, rt, "POST", "/v0/management/plugins/reset-priority/status", nil)
	if resp.StatusCode != 404 {
		t.Errorf("POST status = %d, want 404", resp.StatusCode)
	}
}

func TestManagementBeforeRegistrationReturns503(t *testing.T) {
	rt := newTestRuntime(t, newFakeHostCaller())
	resp := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status", nil)
	if resp.StatusCode != 503 {
		t.Errorf("pre-registration status = %d, want 503", resp.StatusCode)
	}

	resource := managementCall(t, rt, "GET", "/v0/resource/plugins/reset-priority/status", nil)
	if resource.StatusCode != 200 || !strings.Contains(string(resource.Body), `data-reset-priority-status="ready"`) {
		t.Errorf("static resource before registration = %d %s", resource.StatusCode, resource.Body)
	}
}

func TestAuthenticatedStatusShowsDryRunAndDeadline(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\ndry-run: true\n")

	if caller.saveCount() != 0 {
		t.Fatalf("dry-run register performed %d saves", caller.saveCount())
	}
	snap := statusSnapshot(t, rt)
	if !snap.DryRun {
		t.Errorf("status does not report dry-run")
	}
	if snap.NextDeadlineAt == nil || !snap.NextDeadlineAt.Equal(testBase.Add(48*time.Hour)) {
		t.Errorf("next deadline = %v, want %s", snap.NextDeadlineAt, testBase.Add(48*time.Hour))
	}
	if snap.NextReconcileAt == nil {
		t.Errorf("next reconcile missing from status")
	}

	page := managementCall(t, rt, "GET", "/v0/resource/plugins/reset-priority/status", nil)
	if strings.Contains(string(page.Body), "Dry-run: <strong>true</strong>") {
		t.Errorf("public resource exposes the configured dry-run value")
	}
	if !strings.Contains(string(page.Body), `data-reset-priority-status="ready"`) {
		t.Errorf("public resource missing smoke-compatible readiness marker")
	}
}
