package plugin

import (
	"bytes"
	"html/template"
	"strconv"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/engine"
)

// This file renders the AUTHENTICATED management HTML status view. It is
// reachable only through the CPA Management API (GET
// /v0/management/plugins/reset-priority/status/html) and is entirely separate
// from the static, account-free resource shell in status_page.go.
//
// Data rules:
//   - Only fields from the already-sanitized engine.Snapshot are rendered
//     (LastError/WriteError pass through internal/sanitize before publish).
//   - Account rows are identified by the physical auth file name, or by a
//     redacted stable auth-index when no file name exists. The snapshot Label
//     (which may carry an account email) is deliberately never rendered.
//   - All dynamic values are emitted through html/template's contextual
//     auto-escaping.

// statusPageData is the view model for the management status template.
type statusPageData struct {
	GeneratedAt     string
	DryRun          bool
	Enabled         bool
	Stopped         bool
	Warnings        []string
	RosterError     string
	NextReconcileAt string
	NextDeadlineAt  string
	Providers       []statusPageProvider
}

type statusPageProvider struct {
	Provider string
	Accounts []statusPageAccount
}

type statusPageAccount struct {
	Identifier       string
	Health           string
	QuarantineReason string
	ResetState       string
	ResetAtUTC       string
	ResetAt          string
	CurrentPriority  string
	DesiredPriority  int
	LastRefresh      string
	ObservedAt       string
	LastError        string
	WriteError       string
}

var managementStatusTemplate = template.Must(template.New("management-status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<title>Reset Priority — Management Status</title>
<style>
body{font-family:system-ui,sans-serif;margin:2rem;max-width:72rem;color:#1f2933}
h1{margin-bottom:.25rem}
h2{margin-top:2rem;text-transform:capitalize}
table{border-collapse:collapse;width:100%;font-size:.9rem}
th,td{border:1px solid #d0d5dd;padding:.4rem .6rem;text-align:left;vertical-align:top}
th{background:#f3f4f6}
code{background:#f3f4f6;padding:.1rem .3rem;border-radius:.2rem}
.meta{color:#475467;font-size:.9rem}
.badge{display:inline-block;border-radius:.3rem;padding:.1rem .5rem;font-size:.8rem;margin-right:.4rem;background:#eef2f6}
.badge-warn{background:#fef0c7}
.badge-err{background:#fee4e2}
.error-text{color:#b42318}
.empty{color:#475467;font-style:italic}
button{font:inherit;padding:.4rem .9rem;border-radius:.3rem;border:1px solid #475467;background:#f3f4f6;cursor:pointer}
button:disabled{opacity:.6;cursor:default}
#refresh-result{margin-left:.6rem;font-size:.9rem;color:#475467}
</style>
</head>
<body>
<h1>Reset Priority</h1>
<p class="meta">Authenticated management status view. Generated at {{.GeneratedAt}}.</p>
<p>
{{if .DryRun}}<span class="badge badge-warn">dry-run</span>{{else}}<span class="badge">live writes</span>{{end}}
{{if .Enabled}}<span class="badge">enabled</span>{{else}}<span class="badge badge-warn">disabled</span>{{end}}
{{if .Stopped}}<span class="badge badge-warn">stopped</span>{{else}}<span class="badge">running</span>{{end}}
</p>
<p class="meta">
Next reconcile: {{if .NextReconcileAt}}{{.NextReconcileAt}}{{else}}not scheduled{{end}}<br>
Next reset deadline: {{if .NextDeadlineAt}}{{.NextDeadlineAt}}{{else}}not scheduled{{end}}
</p>
{{if .RosterError}}<p class="error-text">Roster error: {{.RosterError}}</p>{{end}}
{{if .Warnings}}<ul>{{range .Warnings}}<li class="error-text">{{.}}</li>{{end}}</ul>{{end}}
<p>
<button id="refresh-now" type="button">Refresh now</button><span id="refresh-result"></span>
</p>
<p class="meta">Refresh now issues <code>POST /v0/management/plugins/reset-priority/refresh</code> on the same origin, preserves the query string, and then reloads. CPA must receive the management authentication header through the browser or reverse-proxy setup; query parameters are not management credentials.</p>
{{range .Providers}}
<h2>{{.Provider}}</h2>
{{if .Accounts}}
<table>
<thead>
<tr><th>Account</th><th>Health</th><th>Reset state</th><th>Weekly reset (UTC)</th><th>Weekly reset (provider zone)</th><th>Current</th><th>Desired</th><th>Last quota refresh</th><th>Observed</th><th>Last error</th><th>Write error</th></tr>
</thead>
<tbody>
{{range .Accounts}}
<tr>
<td><code>{{.Identifier}}</code></td>
<td>{{.Health}}{{if .QuarantineReason}} ({{.QuarantineReason}}){{end}}</td>
<td>{{.ResetState}}</td>
<td>{{if .ResetAtUTC}}{{.ResetAtUTC}}{{else}}—{{end}}</td>
<td>{{if .ResetAt}}{{.ResetAt}}{{else}}—{{end}}</td>
<td>{{.CurrentPriority}}</td>
<td>{{.DesiredPriority}}</td>
<td>{{if .LastRefresh}}{{.LastRefresh}}{{else}}never{{end}}</td>
<td>{{if .ObservedAt}}{{.ObservedAt}}{{else}}—{{end}}</td>
<td>{{if .LastError}}<span class="error-text">{{.LastError}}</span>{{else}}—{{end}}</td>
<td>{{if .WriteError}}<span class="error-text">{{.WriteError}}</span>{{else}}—{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
{{else}}
<p class="empty">No managed accounts discovered for this provider.</p>
{{end}}
{{end}}
<script>
(function () {
	var button = document.getElementById("refresh-now");
	var result = document.getElementById("refresh-result");
	if (!button || !result) {
		return;
	}
	button.addEventListener("click", function () {
		button.disabled = true;
		result.textContent = "Refreshing…";
		var refreshURL = location.pathname.replace(/\/status\/html\/?$/, "/refresh") + location.search;
		fetch(refreshURL, { method: "POST", credentials: "same-origin" })
			.then(function (resp) {
				if (resp.ok) {
					result.textContent = "Reconciliation completed; reloading…";
					location.reload();
					return;
				}
				result.textContent = "Refresh failed: HTTP " + resp.status;
				button.disabled = false;
			})
			.catch(function () {
				result.textContent = "Refresh failed: network error";
				button.disabled = false;
			});
	});
})();
</script>
</body>
</html>
`))

// renderManagementStatusPage renders the authenticated HTML status view from
// the published, already-sanitized snapshot.
func renderManagementStatusPage(snap engine.Snapshot) ([]byte, error) {
	data := statusPageData{
		GeneratedAt: snap.GeneratedAt.UTC().Format(time.RFC3339Nano),
		DryRun:      snap.DryRun,
		Enabled:     snap.Enabled,
		Stopped:     snap.Stopped,
		Warnings:    snap.Warnings,
		RosterError: snap.RosterError,
	}
	if snap.NextReconcileAt != nil {
		data.NextReconcileAt = snap.NextReconcileAt.UTC().Format(time.RFC3339Nano)
	}
	if snap.NextDeadlineAt != nil {
		data.NextDeadlineAt = snap.NextDeadlineAt.UTC().Format(time.RFC3339Nano)
	}
	for _, group := range snap.Providers {
		provider := statusPageProvider{Provider: group.Provider}
		for _, row := range group.Accounts {
			current := "unknown"
			if row.CurrentPriority != nil {
				current = strconv.Itoa(*row.CurrentPriority)
			}
			provider.Accounts = append(provider.Accounts, statusPageAccount{
				Identifier:       safeAccountIdentifier(row),
				Health:           row.Health,
				QuarantineReason: row.QuarantineReason,
				ResetState:       row.ResetState,
				ResetAtUTC:       row.ResetAtUTC,
				ResetAt:          row.ResetAt,
				CurrentPriority:  current,
				DesiredPriority:  row.DesiredPriority,
				LastRefresh:      row.LastSuccessAt,
				ObservedAt:       row.ObservedAt,
				LastError:        row.LastError,
				WriteError:       row.WriteError,
			})
		}
		data.Providers = append(data.Providers, provider)
	}

	var buf bytes.Buffer
	if err := managementStatusTemplate.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// safeAccountIdentifier deliberately ignores the snapshot label/email fields.
// It displays the physical auth file name when present (operators should still
// treat filenames as potentially identifying), otherwise a redacted auth index.
func safeAccountIdentifier(row engine.AccountStatus) string {
	if row.Name != "" {
		return row.Name
	}
	return redactStableIdentifier(row.AuthIndex)
}

// redactStableIdentifier shortens an opaque stable identifier for display so
// the complete value is never rendered.
func redactStableIdentifier(id string) string {
	if id == "" {
		return "(unidentified account)"
	}
	const keep = 8
	if len(id) > keep {
		id = id[:keep] + "…"
	}
	return "auth-index " + id
}
