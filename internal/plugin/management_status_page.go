package plugin

import (
	"bytes"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugin-reset-priority/internal/config"
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
//   - Timestamps are shown in the operator's configured display-timezone in
//     a human-readable form; the exact RFC3339 UTC instant is preserved on
//     each <time> element (datetime attribute and hover title).

// displayTimeLayout is the human-readable timestamp format, e.g.
// "Tue Sep 1 2026 - 9:25:36 PM PDT". Table cells may wrap at the separator.
const (
	displayDateLayout  = "Mon Jan 2 2006"
	displayClockLayout = "3:04:05 PM MST"
	displayTimeLayout  = displayDateLayout + " - " + displayClockLayout
)

// statusPageData is the view model for the management status template.
type statusPageData struct {
	GeneratedAt     statusPageTime
	Timezone        string
	DryRun          bool
	Enabled         bool
	Stopped         bool
	Warnings        []string
	RosterError     string
	NextReconcileAt statusPageTime
	NextDeadlineAt  statusPageTime
	AccountCount    int
	Providers       []statusPageProvider
}

// statusPageTime carries one instant in the forms the template needs.
// A zero value (Set=false) renders as a placeholder.
type statusPageTime struct {
	Set      bool
	Display  string // human-readable "Tue Sep 1 2026 - 6:25:36 PM PDT"
	Date     string // date half of Display, e.g. "Tue Sep 1 2026"
	Clock    string // time half of Display, e.g. "6:25:36 PM PDT"
	Exact    string // RFC3339Nano UTC, for datetime/title attributes
	Relative string // "in 2d 4h" / "12m ago", relative to GeneratedAt
}

type statusPageProvider struct {
	Provider string
	Accounts []statusPageAccount
}

type statusPageAccount struct {
	Identifier       string
	Health           string
	HealthClass      string
	QuarantineReason string
	ResetState       string
	ResetStateClass  string
	ResetAt          statusPageTime
	CurrentPriority  string
	DesiredPriority  int
	PriorityPending  bool
	LastRefresh      statusPageTime
	ObservedAt       statusPageTime
	LastError        string
	WriteError       string
}

var managementStatusTemplate = template.Must(template.New("management-status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Reset Priority — Management Status</title>
<style>
:root{
  --bg:#f8fafc;--surface:#ffffff;--border:#e2e8f0;--border-strong:#cbd5e1;
  --text:#0f172a;--muted:#64748b;--faint:#94a3b8;
  --ok-bg:#ecfdf5;--ok-fg:#047857;--ok-bd:#a7f3d0;
  --warn-bg:#fffbeb;--warn-fg:#b45309;--warn-bd:#fde68a;
  --err-bg:#fef2f2;--err-fg:#b91c1c;--err-bd:#fecaca;
  --info-bg:#eff6ff;--info-fg:#1d4ed8;--info-bd:#bfdbfe;
  --neutral-bg:#f1f5f9;--neutral-fg:#334155;--neutral-bd:#e2e8f0;
  --accent:#2563eb;--accent-fg:#ffffff;--accent-hover:#1d4ed8;
  --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#0b1220;--surface:#111a2b;--border:#1f2a3d;--border-strong:#2c3a52;
    --text:#e5eaf3;--muted:#98a4b8;--faint:#6b778c;
    --ok-bg:#062f26;--ok-fg:#6ee7b7;--ok-bd:#0f5c47;
    --warn-bg:#3a2a06;--warn-fg:#fcd34d;--warn-bd:#6b4d0b;
    --err-bg:#3b0f0f;--err-fg:#fca5a5;--err-bd:#7f1d1d;
    --info-bg:#0f2148;--info-fg:#93c5fd;--info-bd:#1e3a8a;
    --neutral-bg:#1a2436;--neutral-fg:#cbd5e1;--neutral-bd:#2c3a52;
    --accent:#3b82f6;--accent-hover:#60a5fa;
  }
}
*{box-sizing:border-box}
html,body{margin:0;background:var(--bg);color:var(--text)}
body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
.page{max-width:1280px;margin:0 auto;padding:28px 24px 48px}
.header{display:flex;flex-wrap:wrap;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:20px}
h1{font-size:24px;font-weight:650;letter-spacing:-.01em;margin:0 0 4px}
.subtitle{margin:0;color:var(--muted)}
.pills{display:flex;flex-wrap:wrap;gap:6px;margin-top:10px}
.pill{display:inline-flex;align-items:center;gap:6px;border:1px solid var(--neutral-bd);background:var(--neutral-bg);color:var(--neutral-fg);border-radius:999px;padding:2px 10px;font-size:12px;font-weight:500;line-height:18px}
.pill::before{content:"";width:6px;height:6px;border-radius:50%;background:currentColor;opacity:.8}
.pill.ok{background:var(--ok-bg);color:var(--ok-fg);border-color:var(--ok-bd)}
.pill.warn{background:var(--warn-bg);color:var(--warn-fg);border-color:var(--warn-bd)}
.pill.err{background:var(--err-bg);color:var(--err-fg);border-color:var(--err-bd)}
.pill.info{background:var(--info-bg);color:var(--info-fg);border-color:var(--info-bd)}
.actions{display:flex;flex-direction:column;align-items:flex-end;gap:6px}
button{font:inherit;font-weight:500;padding:7px 14px;border-radius:8px;border:1px solid var(--accent);background:var(--accent);color:var(--accent-fg);cursor:pointer;transition:background .12s}
button:hover{background:var(--accent-hover);border-color:var(--accent-hover)}
button:disabled{opacity:.55;cursor:default}
#refresh-result{font-size:12px;color:var(--muted);min-height:18px}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px;margin-bottom:24px}
.stat{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:12px 14px}
.stat .label{font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);margin-bottom:4px}
.stat .value{font-size:14px;font-weight:550}
.stat .hint{font-size:12px;color:var(--muted);margin-top:2px}
.alert{border-radius:10px;padding:10px 14px;margin-bottom:12px;border:1px solid var(--err-bd);background:var(--err-bg);color:var(--err-fg)}
.alert.warn{border-color:var(--warn-bd);background:var(--warn-bg);color:var(--warn-fg)}
.alert ul{margin:4px 0 0;padding-left:18px}
.section{margin-top:28px}
.section-head{display:flex;align-items:baseline;gap:10px;margin-bottom:10px}
h2{font-size:16px;font-weight:650;margin:0;text-transform:capitalize}
.count{color:var(--muted);font-size:13px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:10px;overflow:hidden}
.table-wrap{overflow-x:auto}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{padding:10px 12px;text-align:left;vertical-align:top;border-bottom:1px solid var(--border)}
th{font-size:11px;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--muted);background:var(--neutral-bg);white-space:nowrap}
tbody tr:last-child td{border-bottom:0}
tbody tr:hover td{background:color-mix(in srgb,var(--neutral-bg) 55%,transparent)}
td.num{font-variant-numeric:tabular-nums;white-space:nowrap}
code{font-family:var(--mono);font-size:12px;background:var(--neutral-bg);border:1px solid var(--neutral-bd);padding:1px 6px;border-radius:5px;overflow-wrap:anywhere}
td.account{min-width:200px;max-width:320px}
time{font-variant-numeric:tabular-nums}
time .d,time .t{white-space:nowrap}
.stat time{white-space:nowrap}
.rel{display:block;font-size:12px;color:var(--muted)}
.dash{color:var(--faint)}
.reason{display:block;font-size:12px;color:var(--muted)}
.prio{display:inline-flex;align-items:center;gap:6px;font-variant-numeric:tabular-nums}
.prio .arrow{color:var(--faint)}
.prio.pending .desired{color:var(--warn-fg);font-weight:600}
.error-text{color:var(--err-fg)}
.err-label{font-size:11px;font-weight:600;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);margin-right:4px}
.empty{padding:18px 14px;color:var(--muted);font-style:italic}
.footnote{margin-top:28px;padding-top:14px;border-top:1px solid var(--border);font-size:12px;color:var(--muted);line-height:1.55}
.footnote code{font-size:11px}
</style>
</head>
<body>
<div class="page">
<div class="header">
  <div>
    <h1>Reset Priority</h1>
    <p class="subtitle">Authenticated management status view</p>
    <div class="pills">
      {{if .DryRun}}<span class="pill warn">dry-run</span>{{else}}<span class="pill info">live writes</span>{{end}}
      {{if .Enabled}}<span class="pill ok">enabled</span>{{else}}<span class="pill warn">disabled</span>{{end}}
      {{if .Stopped}}<span class="pill warn">stopped</span>{{else}}<span class="pill ok">running</span>{{end}}
    </div>
  </div>
  <div class="actions">
    <button id="refresh-now" type="button">Refresh now</button>
    <span id="refresh-result"></span>
  </div>
</div>

<div class="stats">
  <div class="stat">
    <div class="label">Next reconcile</div>
    <div class="value">{{template "when" .NextReconcileAt}}</div>
  </div>
  <div class="stat">
    <div class="label">Next reset deadline</div>
    <div class="value">{{template "when" .NextDeadlineAt}}</div>
  </div>
  <div class="stat">
    <div class="label">Generated</div>
    <div class="value">{{template "when" .GeneratedAt}}</div>
    <div class="hint">Times shown in {{.Timezone}}</div>
  </div>
</div>

{{if .RosterError}}<div class="alert"><strong>Roster error:</strong> {{.RosterError}}</div>{{end}}
{{if .Warnings}}<div class="alert warn"><strong>Warnings</strong><ul>{{range .Warnings}}<li>{{.}}</li>{{end}}</ul></div>{{end}}

{{range .Providers}}
<div class="section">
  <div class="section-head">
    <h2>{{.Provider}}</h2>
    <span class="count">{{len .Accounts}} account{{if ne (len .Accounts) 1}}s{{end}}</span>
  </div>
  <div class="card">
  {{if .Accounts}}
  <div class="table-wrap">
  <table>
    <thead>
      <tr>
        <th>Account</th>
        <th>Health</th>
        <th>Reset state</th>
        <th>Weekly reset</th>
        <th>Priority</th>
        <th>Last quota refresh</th>
        <th>Observed</th>
        <th>Errors</th>
      </tr>
    </thead>
    <tbody>
    {{range .Accounts}}
      <tr>
        <td class="account"><code>{{.Identifier}}</code></td>
        <td><span class="pill {{.HealthClass}}">{{.Health}}</span>{{if .QuarantineReason}}<span class="reason">{{.QuarantineReason}}</span>{{end}}</td>
        <td><span class="pill {{.ResetStateClass}}">{{.ResetState}}</span></td>
        <td>{{template "when" .ResetAt}}</td>
        <td class="num"><span class="prio{{if .PriorityPending}} pending{{end}}"><span class="current">{{.CurrentPriority}}</span><span class="arrow">→</span><span class="desired">{{.DesiredPriority}}</span></span></td>
        <td>{{if .LastRefresh.Set}}{{template "when" .LastRefresh}}{{else}}<span class="dash">never</span>{{end}}</td>
        <td>{{template "when" .ObservedAt}}</td>
        <td>{{if or .LastError .WriteError}}{{if .LastError}}<div><span class="err-label">Last</span><span class="error-text">{{.LastError}}</span></div>{{end}}{{if .WriteError}}<div><span class="err-label">Write</span><span class="error-text">{{.WriteError}}</span></div>{{end}}{{else}}<span class="dash">—</span>{{end}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}
  <p class="empty">No managed accounts discovered for this provider.</p>
  {{end}}
  </div>
</div>
{{end}}

<p class="footnote">Refresh now issues <code>POST /v0/management/plugins/reset-priority/refresh</code> on the same origin with the required plugin CSRF header, preserves the query string, and then reloads. CPA must receive the management authentication header through the browser, a same-origin management-console session, or the reverse-proxy setup; query parameters are not management credentials. Hover any timestamp for the exact UTC instant.</p>
</div>
<script>` + browserAuthScript + `
(function () {
	var button = document.getElementById("refresh-now");
	var result = document.getElementById("refresh-result");
	if (!button || !result) {
		return;
	}
	button.addEventListener("click", function () {
		button.disabled = true;
		result.textContent = "Refreshing…";
		var auth = window.resetPriorityAuth;
		var refreshURL = auth.managementPath("/refresh") + location.search;
		fetch(refreshURL, {
			method: "POST",
			credentials: "same-origin",
			headers: auth.authHeaders({ "X-Reset-Priority-Refresh": "1" })
		})
			.then(function (resp) {
				if (resp.ok) {
					result.textContent = "Reconciliation completed; reloading…";
					location.reload();
					return;
				}
				return resp.text().then(function (text) {
					var detail = "";
					try {
						var parsed = JSON.parse(text);
						if (parsed && typeof parsed.detail === "string") {
							detail = parsed.detail;
						}
					} catch (ignored) {}
					result.textContent = "Refresh failed: HTTP " + resp.status + (detail ? ": " + detail : "");
					button.disabled = false;
				});
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
{{define "when"}}{{if .Set}}<time datetime="{{.Exact}}" title="{{.Exact}}"><span class="d">{{.Date}}</span>{{if .Clock}} - <span class="t">{{.Clock}}</span>{{end}}</time>{{if .Relative}}<span class="rel">{{.Relative}}</span>{{end}}{{else}}<span class="dash">—</span>{{end}}{{end}}
`))

// renderManagementStatusPage renders the authenticated HTML status view from
// the published, already-sanitized snapshot.
func renderManagementStatusPage(snap engine.Snapshot) ([]byte, error) {
	loc := config.LoadDisplayLocation(snap.DisplayTimezone)
	now := snap.GeneratedAt
	fmtTime := func(t time.Time) statusPageTime { return newStatusPageTime(t, now, loc) }
	fmtString := func(s string) statusPageTime { return parseStatusPageTime(s, now, loc) }

	data := statusPageData{
		GeneratedAt: fmtTime(snap.GeneratedAt),
		Timezone:    displayZoneName(snap.DisplayTimezone, loc),
		DryRun:      snap.DryRun,
		Enabled:     snap.Enabled,
		Stopped:     snap.Stopped,
		Warnings:    snap.Warnings,
		RosterError: snap.RosterError,
	}
	// The "generated" instant is the reference point, so it has no relative
	// hint of its own.
	data.GeneratedAt.Relative = ""
	if snap.NextReconcileAt != nil {
		data.NextReconcileAt = fmtTime(*snap.NextReconcileAt)
	}
	if snap.NextDeadlineAt != nil {
		data.NextDeadlineAt = fmtTime(*snap.NextDeadlineAt)
	}
	for _, group := range snap.Providers {
		provider := statusPageProvider{Provider: group.Provider}
		for _, row := range group.Accounts {
			current := "unknown"
			pending := true
			if row.CurrentPriority != nil {
				current = strconv.Itoa(*row.CurrentPriority)
				pending = *row.CurrentPriority != row.DesiredPriority
			}
			provider.Accounts = append(provider.Accounts, statusPageAccount{
				Identifier:       safeAccountIdentifier(row),
				Health:           row.Health,
				HealthClass:      healthClass(row.Health),
				QuarantineReason: row.QuarantineReason,
				ResetState:       row.ResetState,
				ResetStateClass:  resetStateClass(row.ResetState),
				ResetAt:          fmtString(row.ResetAtUTC),
				CurrentPriority:  current,
				DesiredPriority:  row.DesiredPriority,
				PriorityPending:  pending,
				LastRefresh:      fmtString(row.LastSuccessAt),
				ObservedAt:       fmtString(row.ObservedAt),
				LastError:        row.LastError,
				WriteError:       row.WriteError,
			})
		}
		data.AccountCount += len(provider.Accounts)
		data.Providers = append(data.Providers, provider)
	}

	var buf bytes.Buffer
	if err := managementStatusTemplate.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// newStatusPageTime builds the display forms for one instant.
func newStatusPageTime(t, now time.Time, loc *time.Location) statusPageTime {
	if t.IsZero() {
		return statusPageTime{}
	}
	local := t.In(loc)
	return statusPageTime{
		Set:      true,
		Display:  local.Format(displayTimeLayout),
		Date:     local.Format(displayDateLayout),
		Clock:    local.Format(displayClockLayout),
		Exact:    t.UTC().Format(time.RFC3339Nano),
		Relative: relativeTime(t, now),
	}
}

// parseStatusPageTime formats an RFC3339 snapshot string. A value that does
// not parse is shown verbatim rather than dropped, so nothing is hidden.
func parseStatusPageTime(s string, now time.Time, loc *time.Location) statusPageTime {
	if s == "" {
		return statusPageTime{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return statusPageTime{Set: true, Display: s, Date: s, Exact: s}
	}
	return newStatusPageTime(t, now, loc)
}

// relativeTime renders a coarse human hint ("in 2d 4h", "12m ago") relative
// to the snapshot generation time. It is intentionally approximate.
func relativeTime(t, now time.Time) string {
	if now.IsZero() {
		return ""
	}
	d := t.Sub(now)
	future := d >= 0
	if !future {
		d = -d
	}
	if d < time.Second {
		return "now"
	}
	var s string
	switch {
	case d < time.Minute:
		s = fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		s = fmt.Sprintf("%dh %dm", h, m)
	default:
		days := int(d / (24 * time.Hour))
		h := int((d % (24 * time.Hour)) / time.Hour)
		s = fmt.Sprintf("%dd %dh", days, h)
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

// displayZoneName is the label shown under the generated timestamp.
func displayZoneName(configured string, loc *time.Location) string {
	if strings.EqualFold(strings.TrimSpace(configured), "local") {
		return "host local time (" + loc.String() + ")"
	}
	if loc == nil || loc.String() == "" {
		return "UTC"
	}
	return loc.String()
}

// healthClass maps engine.Health values to pill styles: healthy is green,
// quarantined (disabled / reauth-required) is red, recovering is amber.
func healthClass(health string) string {
	switch health {
	case string(engine.HealthHealthy):
		return "ok"
	case string(engine.HealthQuarantined):
		return "err"
	case string(engine.HealthRecovering):
		return "warn"
	default:
		return ""
	}
}

// resetStateClass maps engine.ResetState values to pill styles.
func resetStateClass(state string) string {
	switch state {
	case string(engine.ResetConfirmed):
		return "ok"
	case string(engine.ResetStale), string(engine.ResetAwaitingNewWindow):
		return "warn"
	default:
		return ""
	}
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
