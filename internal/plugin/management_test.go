package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/engine"
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
	if len(routes) != 3 {
		t.Fatalf("routes = %d, want 3", len(routes))
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
		"GET /plugins/reset-priority/status":      true,
		"GET /plugins/reset-priority/status/html": true,
		"POST /plugins/reset-priority/refresh":    true,
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
	resp := refreshCall(t, rt, nil, "")
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

func TestManagementRefreshPropagatesHostCallbackID(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")

	before := caller.httpCallCount()
	resp := refreshCall(t, rt, nil, "request-context-123")
	if resp.StatusCode != 200 {
		t.Fatalf("refresh = %d, want 200", resp.StatusCode)
	}
	calls := caller.httpCallsSince(before)
	if len(calls) == 0 {
		t.Fatalf("refresh did not issue a provider request")
	}
	for _, call := range calls {
		if call.HostCallbackID != "request-context-123" {
			t.Errorf("provider callback id = %q, want request-context-123", call.HostCallbackID)
		}
	}
}

func TestManagementRefreshReportsRosterFailure(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")
	caller.setListError(fmt.Errorf("roster unavailable"))

	resp := refreshCall(t, rt, nil, "")
	if resp.StatusCode != 503 {
		t.Fatalf("failed refresh = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), `"status":"error"`) {
		t.Fatalf("failed refresh body = %s, want error", resp.Body)
	}
}

func TestManagementRefreshRequiresCSRFHeader(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")
	before := caller.httpCallCount()

	resp := managementRequest(t, rt, hostapi.ManagementRequest{
		Method:  "POST",
		Path:    "/v0/management/plugins/reset-priority/refresh",
		Headers: map[string][]string{"Origin": {"https://attacker.example"}},
	})
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin simple POST = %d, want 403", resp.StatusCode)
	}
	if caller.httpCallCount() != before {
		t.Fatalf("cross-origin simple POST triggered provider traffic")
	}
}

func TestManagementRefreshRejectsCrossSiteFetch(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")
	before := caller.httpCallCount()

	resp := refreshCall(t, rt, map[string][]string{
		"Sec-Fetch-Site": {"cross-site"},
	}, "")
	if resp.StatusCode != 403 {
		t.Fatalf("cross-site refresh = %d, want 403", resp.StatusCode)
	}
	if caller.httpCallCount() != before {
		t.Fatalf("cross-site refresh triggered provider traffic")
	}
}

func TestManagementRefreshRejectsSameSiteSiblingOrigin(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")
	before := caller.httpCallCount()

	resp := refreshCall(t, rt, map[string][]string{
		"Origin":         {"https://sibling.example.test"},
		"Sec-Fetch-Site": {"same-site"},
	}, "")
	if resp.StatusCode != 403 {
		t.Fatalf("same-site sibling refresh = %d, want 403", resp.StatusCode)
	}
	if caller.httpCallCount() != before {
		t.Fatalf("same-site sibling refresh triggered provider traffic")
	}
}

func TestManagementRefreshRejectsBrowserOriginWithoutFetchMetadata(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")
	before := caller.httpCallCount()

	resp := refreshCall(t, rt, map[string][]string{
		"Origin": {"https://unknown-origin.example"},
	}, "")
	if resp.StatusCode != 403 {
		t.Fatalf("origin without fetch metadata = %d, want 403", resp.StatusCode)
	}
	if caller.httpCallCount() != before {
		t.Fatalf("origin without fetch metadata triggered provider traffic")
	}
}

func TestManagementRefreshRejectsInvalidFetchMetadata(t *testing.T) {
	for _, value := range []string{"", "unknown", "same-origin, cross-site"} {
		t.Run(value, func(t *testing.T) {
			caller := newFakeHostCaller()
			caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
			rt := newTestRuntime(t, caller)
			registerRuntime(t, rt, "enabled: true\n")
			before := caller.httpCallCount()

			resp := refreshCall(t, rt, map[string][]string{
				"Sec-Fetch-Site": {value},
			}, "")
			if resp.StatusCode != 403 {
				t.Fatalf("fetch metadata %q = %d, want 403", value, resp.StatusCode)
			}
			if caller.httpCallCount() != before {
				t.Fatalf("invalid fetch metadata %q triggered provider traffic", value)
			}
		})
	}
}

func TestManagementRefreshAllowsSameOriginBrowserAndHeaderClient(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string][]string
	}{
		{name: "same-origin browser", headers: map[string][]string{
			"Origin":         {"https://management.example.test"},
			"Sec-Fetch-Site": {"same-origin"},
		}},
		{name: "browser navigation origin", headers: map[string][]string{
			"Sec-Fetch-Site": {"none"},
		}},
		{name: "non-browser management client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := newFakeHostCaller()
			caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
			rt := newTestRuntime(t, caller)
			registerRuntime(t, rt, "enabled: true\n")
			before := caller.httpCallCount()

			resp := refreshCall(t, rt, tc.headers, "")
			if resp.StatusCode != 200 {
				t.Fatalf("refresh = %d, want 200", resp.StatusCode)
			}
			if caller.httpCallCount() <= before {
				t.Fatalf("allowed refresh did not trigger provider traffic")
			}
		})
	}
}

func TestManagementRefreshCannotActivateDisabledRegistration(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: false\n")

	resp := refreshCall(t, rt, nil, "")
	if resp.StatusCode != 409 {
		t.Fatalf("disabled refresh = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), `"status":"no_op"`) {
		t.Fatalf("disabled refresh body = %s, want no_op", resp.Body)
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

func TestManagementStatusPageRendersSanitizedHTML(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	caller.addClaudeAccount("b", testBase.Add(24*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")
	snap := statusSnapshot(t, rt)

	resp := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status/html", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status page = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Headers["Content-Type"]; len(ct) == 0 || !strings.Contains(ct[0], "text/html") {
		t.Errorf("content type = %v", ct)
	}
	if cc := resp.Headers["Cache-Control"]; len(cc) == 0 || cc[0] != "no-store" {
		t.Errorf("cache control = %v, want no-store", cc)
	}
	if csp := resp.Headers["Content-Security-Policy"]; len(csp) == 0 || !strings.Contains(csp[0], "frame-ancestors 'self'") {
		t.Errorf("content security policy = %v", csp)
	}

	page := string(resp.Body)
	for _, marker := range []string{
		"<h2>claude</h2>",
		"<code>a.json</code>",
		"<code>b.json</code>",
		"<td>200</td>", // desired priority of the earliest-reset account
		"confirmed",
		"Next reconcile:",
		"Next reset deadline:",
		`id="refresh-now"`,
		"Refresh now",
		"POST /v0/management/plugins/reset-priority/refresh",
		`method: "POST"`,
		`"X-Reset-Priority-Refresh": "1"`,
	} {
		if !strings.Contains(page, marker) {
			t.Errorf("management status page missing %q", marker)
		}
	}

	// Exact reset timestamps and per-account fields come from the snapshot;
	// the account label (an email in this fixture) must never be rendered.
	for _, group := range snap.Providers {
		for _, account := range group.Accounts {
			if account.ResetAtUTC == "" || !strings.Contains(page, account.ResetAtUTC) {
				t.Errorf("status page missing reset timestamp %q", account.ResetAtUTC)
			}
			if account.Label == "" {
				t.Fatalf("test fixture lost its email label")
			}
			if strings.Contains(page, account.Label) {
				t.Errorf("status page renders account label %q", account.Label)
			}
		}
	}
	if strings.Contains(page, "@example.com") {
		t.Errorf("status page contains an email address")
	}

	// Secret safety: no access token, refresh token, or bearer material.
	for _, secret := range []string{testAccessToken, "refresh-token-super-secret", "Bearer "} {
		if strings.Contains(page, secret) {
			t.Errorf("management status page leaks %q", secret)
		}
	}
}

func TestManagementStatusPageShowsDryRunAndStopped(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\ndry-run: true\n")

	resp := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status/html", nil)
	page := string(resp.Body)
	for _, marker := range []string{
		">dry-run<",
		">enabled<",
		">running<",
		testBase.Add(48 * time.Hour).UTC().Format(time.RFC3339Nano), // next deadline
	} {
		if !strings.Contains(page, marker) {
			t.Errorf("dry-run status page missing %q", marker)
		}
	}

	env := decodeEnvelope(t, rt.Dispatch(hostapi.MethodPluginReconfigure,
		lifecycleRequest(t, "enabled: false\n", 4)))
	if !env.OK {
		t.Fatalf("reconfigure failed: %+v", env.Error)
	}
	resp = managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status/html", nil)
	page = string(resp.Body)
	for _, marker := range []string{">disabled<", ">stopped<"} {
		if !strings.Contains(page, marker) {
			t.Errorf("disabled status page missing %q", marker)
		}
	}
}

func TestRenderManagementStatusPageEscapesHTMLAndRedactsIdentifiers(t *testing.T) {
	hostile := `<script>alert("x")</script>`
	current := 100
	snap := engine.Snapshot{
		GeneratedAt: testBase,
		Warnings:    []string{hostile},
		RosterError: hostile,
		Providers: []engine.ProviderGroup{{
			Provider: "claude",
			Accounts: []engine.AccountStatus{
				{
					Name:            hostile + ".json",
					Label:           "leak@example.com",
					Provider:        "claude",
					Health:          "healthy",
					ResetState:      "confirmed",
					CurrentPriority: &current,
					DesiredPriority: 200,
					LastError:       hostile,
					WriteError:      hostile,
				},
				{
					AuthIndex:       "idx-0123456789abcdef",
					Provider:        "claude",
					Health:          "healthy",
					ResetState:      "unknown",
					DesiredPriority: 100,
				},
			},
		}},
	}

	raw, err := renderManagementStatusPage(snap)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	page := string(raw)

	if strings.Contains(page, "<script>alert") {
		t.Errorf("hostile markup rendered unescaped")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Errorf("hostile markup missing in escaped form")
	}
	if strings.Contains(page, "leak@example.com") {
		t.Errorf("account label rendered on management page")
	}
	if !strings.Contains(page, "auth-index idx-0123…") {
		t.Errorf("nameless account missing redacted stable identifier")
	}
	if strings.Contains(page, "idx-0123456789abcdef") {
		t.Errorf("complete auth index rendered without redaction")
	}
	if !strings.Contains(page, "<td>unknown</td>") {
		t.Errorf("missing current priority placeholder for unknown physical priority")
	}
}

func TestManagementStatusPagePathOnResourceRouteStaysStatic(t *testing.T) {
	caller := newFakeHostCaller()
	caller.addClaudeAccount("a", testBase.Add(48*time.Hour))
	rt := newTestRuntime(t, caller)
	registerRuntime(t, rt, "enabled: true\n")

	// A GET outside /v0/management must always fall back to the static
	// account-free shell, even with the HTML page's path suffix.
	resp := managementCall(t, rt, "GET", "/v0/resource/plugins/reset-priority/status/html", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("resource-path html = %d, want 200", resp.StatusCode)
	}
	page := string(resp.Body)
	if !strings.Contains(page, `data-reset-priority-status="ready"`) {
		t.Errorf("resource-path html is not the static shell")
	}
	for _, dynamic := range []string{"a.json", "<table", "Refresh now"} {
		if strings.Contains(page, dynamic) {
			t.Errorf("resource-path html contains dynamic content %q", dynamic)
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
	// The browser HTML status view is read-only as well.
	resp = managementCall(t, rt, "POST", "/v0/management/plugins/reset-priority/status/html", nil)
	if resp.StatusCode != 404 {
		t.Errorf("POST status/html = %d, want 404", resp.StatusCode)
	}
	// A resource-shaped path can never reach the mutating refresh action.
	resp = managementCall(t, rt, "POST", "/v0/resource/plugins/reset-priority/refresh", nil)
	if resp.StatusCode != 404 {
		t.Errorf("resource POST refresh = %d, want 404", resp.StatusCode)
	}
}

func TestManagementBeforeRegistrationReturns503(t *testing.T) {
	rt := newTestRuntime(t, newFakeHostCaller())
	resp := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status", nil)
	if resp.StatusCode != 503 {
		t.Errorf("pre-registration status = %d, want 503", resp.StatusCode)
	}
	page := managementCall(t, rt, "GET", "/v0/management/plugins/reset-priority/status/html", nil)
	if page.StatusCode != 503 {
		t.Errorf("pre-registration status page = %d, want 503", page.StatusCode)
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
