package plugin

// staticStatusPage is the unauthenticated browser resource. It is deliberately
// static: complete operational and account status is available only from the
// authenticated management status route.
var staticStatusPage = []byte(`<!doctype html><html><head><meta charset="utf-8"><title>Reset Priority</title><style>body{font-family:system-ui,sans-serif;margin:2rem;max-width:48rem;color:#1f2933}p{line-height:1.5}.meta{color:#475467;font-size:.9rem}code{background:#f3f4f6;padding:.1rem .3rem;border-radius:.2rem}</style></head><body><h1>Reset Priority</h1><p data-reset-priority-status="ready">The plugin resource is available.</p><p>This public page is a read-only shell. Complete operational details are available only through the authenticated management API.</p><p class="meta">Use authenticated <code>GET /v0/management/plugins/reset-priority/status</code> to view complete status as JSON, or the authenticated browser view at <code>GET /v0/management/plugins/reset-priority/status/html</code>. Dry-run configuration recommended for initial validation.</p></body></html>`)

func renderStatusPage() []byte { return staticStatusPage }
