# Reset Priority for CLIProxyAPI

`reset-priority` is a native CLIProxyAPI (CPA) plugin that maintains hard credential priorities for Claude OAuth and Codex OAuth accounts. Within each provider, the account whose regular weekly quota resets soonest receives the highest priority.

The plugin is a priority manager, not a utilization-balancing scheduler. It does not use weights and does not modify CPA's global routing configuration.

## Policy invariant

The exact deadline of the regular weekly quota is the only value allowed to determine ordering among healthy accounts:

- Claude: `seven_day.resets_at`
- Codex: the window whose declared duration is exactly `604800` seconds (`10080` minutes), using `reset_at`/`resetAt` or `reset_after_seconds`/`resetAfterSeconds`; usage probes require a resolved ChatGPT account ID and send a Codex-CLI-shaped `User-Agent`

The plugin does not rank from utilization, percentage used or remaining, five-hour windows, model-scoped Claude windows, monthly Codex windows, additional/code-review limits, reset credits, plan type, burn rate, request success, cooldown state, or any other signal.

Claude and Codex are ranked independently.

## Priority model

For `N` healthy accounts in one provider group:

```text
priority = floor + step * (N - 1 - rank)
```

The v0.1.0 defaults are a hard floor of `100`, a step of `100`, and a quarantine sentinel of `0`:

```text
1 healthy account: 100
2 healthy accounts: 200, 100
3 healthy accounts: 300, 200, 100
5 healthy accounts: 500, 400, 300, 200, 100
```

Accounts with a future confirmed or still-future stale weekly reset sort first by the exact timestamp. Healthy accounts with `awaiting_new_window` or `unknown` reset state sort afterward with a stable filename/ID tie-break and still count in `N`.

### Credential health is separate from quota availability

Only definitive credential-health failures leave the active ranking pool:

- intentionally disabled credentials;
- narrow CPA auth-error states indicating unauthorized, revoked, forbidden, `invalid_grant`, or reauthentication required;
- recovered credentials that have not yet produced a fresh post-recovery future weekly reset.

These accounts receive priority `0`, do not count in the healthy `N`, and have that sentinel written into their physical auth JSON so CPA's selector actually demotes them.

Quota exhaustion, HTTP 429, CPA cooldown/backoff, temporary unavailability, five-hour exhaustion, model-specific exhaustion, and generic network/provider failures are not quarantine signals. If explicit transient wording appears alongside a generic auth marker such as `forbidden`, `403`, `revoked`, or `reauth`, the transient classification wins; disabled state, `unauthorized`, and `invalid_grant` remain authoritative. CPA continues to own request-time availability and failover.

While any account is quarantined or recovering, the plugin runs a short local health probe (about once a minute) over the `host.auth.get_runtime` callback. It reads runtime health fields only — no credential JSON and no provider quota request — so CPA self-recovery is detected in about a minute rather than at the next hourly reconciliation. The probe never mutates state; a detected change triggers the ordinary reconciliation. It is disarmed whenever every managed account is healthy. After the plugin itself saves a quarantine sentinel, it requires a later token-refresh or physical-file modification timestamp before accepting the host's `active` runtime state as recovery, preventing the audited save-side active rebuild from falsely re-enabling the account.

A reauthenticated account remains `recovering` at `0` until the plugin confirms a future regular weekly reset observed after recovery. Faster detection does not shortcut this: cached pre-failure data cannot re-promote it.

## Exact reset rollover

The hourly reconcile is a safety net, not reset-time precision. The plugin maintains an exact timer for the nearest known weekly deadline.

At the deadline it synchronously:

1. marks the expired observation `awaiting_new_window`;
2. removes that old timestamp from active earliest-deadline ordering;
3. recomputes priorities and writes the local demotion;
4. only then starts asynchronous provider reads.

Fresh reads use bounded retries at `+5s`, `+30s`, `+2m`, `+5m`, and `+15m`, then return to the normal reconciliation interval. An expired or repeated provider timestamp never re-promotes the account.

Codex can lazily continue reporting an expired window. v0.1.0 keeps the account in `awaiting_new_window` and performs passive retries; it does not send a quota-consuming activation request. `codex-reset-window-activation: true` is accepted but ignored with a status warning.

## Requirements and platform support

- A current plugin-capable CLIProxyAPI build with native plugin ABI v1 support
- Persistent access to CPA's plugin directory
- Physical Claude and/or Codex OAuth auth files exposed by CPA host callbacks (`host.auth.list`, `host.auth.get`, `host.auth.get_runtime`, `host.auth.save`, `host.http.do`, `host.log`)
- Go 1.26.0 and a native C toolchain only when building from source

The v0.1.0 release workflow builds these five native targets:

| Platform | Asset suffix | Library |
| --- | --- | --- |
| Linux x86-64 | `linux_amd64` | `reset-priority.so` |
| Linux ARM64 | `linux_arm64` | `reset-priority.so` |
| macOS Intel | `darwin_amd64` | `reset-priority.dylib` |
| macOS Apple Silicon | `darwin_arm64` | `reset-priority.dylib` |
| Windows x86-64 | `windows_amd64` | `reset-priority.dll` |

Windows ARM64, FreeBSD, Linux architectures other than amd64/arm64, and portable/no-plugin CPA builds are not release targets in v0.1.0. The Linux amd64 and arm64 artifacts are compiled on matching-architecture GitHub runners inside pinned `manylinux2014` containers and must pass a `readelf` gate proving that every referenced GLIBC symbol version is `2.17` or older. macOS and Windows artifacts are built natively on matching hosted runners. Ordinary Go cross-compilation is not sufficient for these CGO `c-shared` libraries.

## Installation

For Docker Compose or Coolify, first persist `/CLIProxyAPI/plugins` with a named volume. Then install manually or from this repository's custom Plugin Store source.

- [Docker Compose and manual installation](docs/install-docker-compose.md)
- [Custom Plugin Store install, update, and uninstall](docs/custom-plugin-store.md)
- [Troubleshooting and upstream compatibility caveats](docs/troubleshooting.md)

Start with `dry-run: true`. Do not enable writes until every real Claude/Codex account and desired priority is correct.

A complete example is in [`config.example.yaml`](config.example.yaml). Merge it into the existing CPA configuration; do not replace unrelated auth, logging, model, management, session-affinity, or routing settings.

### Plugin load priority versus credential priority

These are different settings:

```yaml
plugins:
  configs:
    reset-priority:
      priority: 10
```

`plugins.configs.reset-priority.priority` is CPA's plugin registration/load-order priority. The plugin ignores it when ranking credentials.

The credential priorities managed inside physical OAuth auth JSON are `100`, `200`, `300`, and so on, with `0` reserved for quarantine/recovery.

## Configuration reference

| Field | Default | Meaning |
| --- | ---: | --- |
| `enabled` | `false` | Enables the plugin runtime. |
| `priority` | host-owned | CPA plugin load/order priority; ignored by credential ranking. |
| `reconcile-interval` | `1h` | Full roster/provider reconciliation interval. `refresh-interval` is an alias. |
| `request-timeout` | `10s` | Per-account provider quota-request timeout. |
| `priority-floor` | `100` | Lowest healthy priority; must be positive. |
| `priority-step` | `100` | Gap between healthy ranks; must be positive. |
| `quarantine-priority` | `0` | Disabled/reauth/recovering sentinel; must be non-negative and below the floor. |
| `manage-claude` | `true` | Manage physical Claude OAuth credentials. |
| `manage-codex` | `true` | Manage physical Codex OAuth credentials. |
| `providers` | — | Alias list containing `claude` and/or `codex`; explicit `manage-*` fields win. |
| `dry-run` | `false` | Computes, schedules, reports, and logs proposed writes without calling `host.auth.save`. Use `true` first. |
| `codex-reset-window-activation` | `false` | Phase-2 option; `true` is ignored with a warning in v0.1.0. |

Durations accept Go duration strings such as `1h` and `10s`, or bare YAML integers interpreted as seconds.

## Status and management routes

The plugin exposes:

| Route | Access | Behavior |
| --- | --- | --- |
| `GET /v0/management/plugins/reset-priority/status` | Authenticated CPA Management API | Sanitized JSON snapshot. |
| `GET /v0/management/plugins/reset-priority/status/html` | Authenticated CPA Management API | Browser HTML status view rendered from the same sanitized snapshot, with a Refresh now action. |
| `POST /v0/management/plugins/reset-priority/refresh` | Authenticated CPA Management API | Synchronous full reconciliation. |
| `GET /v0/resource/plugins/reset-priority/status` | Unauthenticated resource route | Static readiness shell with no account-level data; never mutates state. |

Example with a placeholder management key:

```bash
export CPA_MANAGEMENT_KEY='<MANAGEMENT_KEY>'

curl --fail --silent --show-error \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/reset-priority/status

curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/reset-priority/refresh
```

The HTML status view at `GET /v0/management/plugins/reset-priority/status/html` is part of the authenticated Management API and requires the same management authentication as the JSON route. It shows per-account rows keyed by the physical auth file name (or a redacted stable auth-index when no file name exists) with provider, reset timestamp/state, current and desired priority, refresh health and sanitized errors, next deadline/reconcile timers, and the dry-run/stopped state. It deliberately omits the snapshot account label/email fields, tokens, and raw provider errors; physical filenames can still be identifying and should be redacted in screenshots. Its Refresh now button makes a same-origin `POST .../refresh` request and preserves the query string.

At the audited CPA revision, Management API authentication is header-only (`Authorization: Bearer ...` or `X-Management-Key`); a query parameter is not a management credential, and ordinary address-bar navigation cannot add that header. Open the HTML route only through a browser/profile or authenticated reverse proxy that supplies the management header to both the page GET and its same-origin refresh POST. Otherwise use the authenticated JSON route and `curl` commands above.

The browser resource is intentionally unauthenticated under CPA's resource-route model. It is a static readiness shell with no account labels, identifiers, timestamps, priorities, errors, configuration values, or actions:

```text
http://127.0.0.1:8317/v0/resource/plugins/reset-priority/status
```

No route or log includes access tokens, refresh tokens, auth headers, raw auth JSON, or provider response bodies.

## Write safety

`host.auth.save` replaces a complete auth JSON document; it is not a field patch and has no compare-and-swap operation. Before every write, the plugin re-reads the latest physical document, changes only top-level `priority`, preserves unrelated JSON values, skips no-op writes, and continues after per-account failures.

The plugin never calls `host.auth.save` in dry-run.

Quarantined credentials **are** written, once, to persist the sentinel `0`. This is a deliberate tradeoff. At the audited revision the save/upsert path rebuilds the runtime record as `Status=active` with `Disabled` unset and does not read a `disabled` key from the document, so any save transiently reactivates the runtime record. A sentinel that lives only in plugin status demotes nothing, because CPA's selector reads priority from the auth record — so the write is what actually removes a dead credential from competition. The exposure is bounded by the no-op-skip rule (one write per quarantine, never per reconciliation), by verbatim preservation of every other field including any `disabled` key, by the sentinel priority itself putting the credential last in selection order, by requiring post-write refresh/file evidence before accepting recovery, and by CPA re-quarantining a genuinely broken credential on next use. See [troubleshooting](docs/troubleshooting.md).

## Current upstream compatibility caveats

The ABI and auth behavior were audited against CLIProxyAPI commit `81e1b5374f99c212f196f34956eeed964a46b8fa` (nearest release `v7.2.146`). Revalidate after upgrading CPA.

1. **Runtime selector refresh is not synchronously guaranteed by `host.auth.save`.** At the audited revision, selection reads priority from the runtime auth `Attributes["priority"]`, while the immediate `host.auth.save` rebuild/upsert path places the file's priority in metadata without synthesizing that selector attribute. The physical file is updated, but an active selector may not observe the new priority until CPA's auth watcher resynthesizes the file or CPA restarts. If status looks correct but routing does not, restart/reload CPA and see [troubleshooting](docs/troubleshooting.md).
2. **Session affinity can delay visible priority changes.** At the audited revision, an already-bound, still-available session remains on its bound credential even when a higher-priority credential is available; priority applies to new sessions, requests without a session, and failover. Older CPA revisions had a reported priority-prefilter interaction that could replace affinity bindings. Do not assume priority changes migrate existing sessions; validate with new session IDs on the deployed CPA version.
3. **The plugin does not implement request-level routing.** It writes credential priorities and relies on CPA for quota exhaustion, cooldowns, 429 handling, retries, and fallthrough.
4. **A same-path native unload/reinstall requires a CPA restart.** Once a resident CPA process has run the plugin's terminal native shutdown, the library's Go runtime state in that process is permanently terminal. A later `cliproxy_plugin_init` for the same library path is refused (nonzero) instead of reinitializing terminal state. Restart CPA to load a reinstalled or updated library.
5. **Terminal shutdown waits unboundedly for host callbacks already in flight.** ABI v1 host callbacks carry no cancellation, so shutdown must drain an entered callback before the host may release the host API table; abandoning it on a timer would risk use-after-free during unload. If a host callback (typically `host.http.do` without a host-side deadline) never returns, plugin unload blocks until CPA exits. There is no safe plugin-side bounded drain at this ABI; see [troubleshooting](docs/troubleshooting.md).
6. **`host.auth.save` transiently reactivates the runtime auth record.** At the audited revision the save-side rebuild sets `Status=active`, leaves `Disabled`/`Unavailable` unset, and does not read a `disabled` key from the supplied document; it carries over only `CreatedAt`, `LastRefreshedAt`, `NextRetryAfter`, and `Runtime`. This affects any caller of the callback. The plugin accepts it in order to persist the quarantine sentinel, bounds it to one write per quarantine, and does not accept the resulting active record as recovery unless a later refresh/file timestamp proves a post-write change; see [Write safety](#write-safety).
7. **`host.auth.get_runtime` returns an error rather than an entry for some healthy-looking cases.** At the audited revision it errors on an unknown auth index, on a runtime-only auth that is disabled, and on a disabled or management-removed file-backed auth whose file no longer exists. The health probe therefore treats every probe error as "no observation" and defers to roster reconciliation, never as a health transition.
8. **Browser management authentication remains header-only.** CPA accepts `Authorization: Bearer ...` or `X-Management-Key`; it does not authenticate a management route from its query string. The HTML status route and Refresh action therefore require a browser/profile or authenticated reverse proxy that supplies the header to both requests.

## Build and validation

```bash
make fmt-check
make vet
make test
make race
make build
make check-linux-glibc  # Linux only; package runs this automatically on Linux
make package
make check-release
bash -n scripts/smoke-test.sh
```

On Linux, `make build` creates `reset-priority.so`. The smoke test uses a disposable `eceasy/cli-proxy-api:latest` container, a temporary config, and a disposable named plugin volume:

```bash
./scripts/smoke-test.sh ./reset-priority.so
```

It performs no real OAuth calls and keeps evidence under `dist/smoke/<run-id>/`. Real-account validation must remain dry-run-first.

## Operator acceptance checklist

- [ ] `/CLIProxyAPI/plugins` is backed by a persistent named volume.
- [ ] The correct native library loads after a CPA restart.
- [ ] Claude physical OAuth accounts are discovered.
- [ ] Codex physical OAuth accounts are discovered.
- [ ] Dry-run computes the expected `100`-point hard priorities.
- [ ] Only Claude `seven_day.resets_at` affects Claude ranking.
- [ ] Only the exact `604800`-second Codex window affects Codex ranking.
- [ ] Utilization, five-hour, model-scoped, monthly, additional, code-review, and credit signals do not change ordering.
- [ ] A one-account provider group remains at `100`.
- [ ] Adding a second healthy account automatically yields `200 / 100`.
- [ ] The next exact reset deadline appears in status.
- [ ] Quota/429/cooldown/unavailable states do not cause quarantine by themselves.
- [ ] A disabled or definitive reauth-required auth is outside the healthy count at `0`, and (outside dry-run) its physical auth JSON carries `"priority": 0`.
- [ ] A steady quarantined account produces no repeated writes across several reconciliations.
- [ ] CPA self-recovery of a quarantined credential is reflected in status within about a minute.
- [ ] A recovered auth remains at `0` until a fresh post-recovery future weekly reset is confirmed.
- [ ] A deadline event demotes locally before a provider network refresh succeeds.
- [ ] `dry-run: false` is enabled only after all desired priorities are verified.
- [ ] New sessions route according to updated priorities after any required CPA reload/restart.
- [ ] Existing session-affinity behavior is understood and tested separately.
- [ ] CPA restart/redeploy retains the installed plugin.

## Releases

See [docs/release.md](docs/release.md) for the version, test, native build, tag, artifact, checksum, smoke, and release-note procedure.

## Security

Examples contain placeholders only. Never paste OAuth JSON, access/refresh tokens, provider bodies, cookies, or real management keys into issues, logs, release notes, test fixtures, or status screenshots.

## License

MIT. See [`LICENSE`](LICENSE).
