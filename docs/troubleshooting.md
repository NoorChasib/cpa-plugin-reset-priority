# Troubleshooting

Never include access tokens, refresh tokens, cookies, authorization headers, raw auth JSON, provider response bodies, or a real CPA management key in diagnostics, screenshots, or bug reports.

## Compatibility baseline

The plugin ABI, auth callbacks, packaging, and selector behavior were audited against CLIProxyAPI commit:

```text
81e1b5374f99c212f196f34956eeed964a46b8fa
```

The nearest upstream release is `v7.2.146`. Recheck these caveats after changing CPA versions.

## The plugin does not load

1. Confirm the CPA image/build supports native plugins. Portable/no-plugin builds cannot load the shared library.
2. Confirm the plugin directory is configured and persisted:

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
   ```

3. Confirm the library matches the container OS/architecture:

   ```bash
   docker exec cli-proxy-api uname -m
   docker exec cli-proxy-api sh -c \
     'find /CLIProxyAPI/plugins -maxdepth 4 -type f -name "reset-priority*" -print'
   ```

4. Manual Linux installs must use `reset-priority.so`. Store installs can use a versioned platform path such as `linux/amd64/reset-priority-v<VERSION>.so` after CPA extracts the unversioned library from the release ZIP.
5. For Linux, confirm the runtime GLIBC and the library's symbol-version requirements:

   ```bash
   docker exec cli-proxy-api ldd --version
   readelf --version-info --wide ./reset-priority.so \
     | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' \
     | sort -Vu
   ```

   Official v0.1.0 Linux assets are built in matching-architecture pinned `manylinux2014` containers and are gated at GLIBC `2.17`. If an official asset reports any higher version, treat it as a defective release. A locally compiled Ubuntu binary is not equivalent to the release artifact even if its filename matches.
6. Ensure the library is readable/executable and restart CPA.
7. Inspect sanitized host logs:

   ```bash
   docker logs --tail=200 cli-proxy-api
   ```

Do not post complete debug logs without reviewing them for unrelated secrets.

## The status route returns 404

- Confirm the plugin registered successfully.
- Confirm the exact paths:

  ```text
  GET  /v0/management/plugins/reset-priority/status
  POST /v0/management/plugins/reset-priority/refresh
  GET  /v0/resource/plugins/reset-priority/status
  ```

- The management routes require CPA management authentication.
- The resource route is a static, account-free readiness shell and is read-only and unauthenticated under CPA's resource model.
- Restart CPA after first installing a native library.

## The status route returns 503

The plugin route exists but its runtime has not completed `plugin.register`. Check config parsing and lifecycle errors in CPA logs. Validate YAML indentation and make sure `plugins.configs.reset-priority` is a mapping.

## No accounts are discovered

The plugin intentionally manages only physical Claude and Codex OAuth credentials exposed by CPA:

- runtime-only/virtual credentials are skipped because they have no safe physical file to update;
- unrelated providers and API-key providers are ignored;
- entries need a physical filename and CPA auth index;
- a disabled auth can appear quarantined rather than healthy.

Confirm the OAuth credentials already work in CPA and inspect the authenticated status JSON. Do not print their raw files.

## Reset state is `unknown`

`unknown` means no usable regular weekly reset has been confirmed. Common causes:

- a newly added account has not completed its first quota read;
- a provider request failed before any successful observation;
- the payload did not include the regular weekly window;
- an upstream schema changed;
- required OAuth routing data was missing.

The plugin does not substitute five-hour, model-scoped, monthly, additional, code-review, credit, utilization, or plan signals. A healthy unknown account remains at the low end of the provider's healthy ranking and still counts in `N`.

Trigger the authenticated refresh route and inspect sanitized `last_error`.

## Reset state is `stale`

A routine fetch failed, but the last confirmed reset is still in the future. The plugin retains that deadline to avoid priority churn during a transient outage. At the exact deadline, it stops using the timestamp even if provider reads still fail.

## Reset state is `awaiting_new_window`

The known weekly reset has passed and the provider has not confirmed a fresh future window. The plugin has already demoted the expired timestamp locally and will retry at `+5s`, `+30s`, `+2m`, `+5m`, and `+15m`, followed by normal hourly reconciliation.

This is expected with Codex lazy reset behavior. v0.1.0 performs passive reads only. `codex-reset-window-activation: true` does not send activation traffic; it is ignored with a warning.

## An account is `quarantined`

Quarantine is limited to definitive credential-health conditions such as:

- intentionally disabled;
- unauthorized/forbidden/revoked;
- `invalid_grant`;
- explicit reauthentication-required state.

The desired priority is `0`, and the account is excluded from the healthy count.

`Unavailable == true` alone is not enough. Quota exhaustion, HTTP 429, CPA cooldown/backoff, temporary model cooldown, overloaded errors, and generic network/5xx/timeouts must not cause quarantine.

## An account is `recovering`

CPA reports that a previously quarantined credential is healthy again, but the plugin has not confirmed a fresh future weekly reset observed after recovery. The account stays at `0` by design.

Do not work around this by copying an old reset or manually forcing a normal priority. Trigger a refresh and allow the bounded recovery retries. Reauthenticate again if the provider quota endpoint remains unauthorized.

## Priorities were written but routing did not change immediately

This is a known upstream integration limitation at the audited CPA revision.

`host.auth.save`:

1. writes the complete supplied JSON document to the auth directory;
2. rebuilds/upserts the runtime auth record;
3. does not provide a field patch or compare-and-swap operation.

The request selector reads priority from runtime `Attributes["priority"]`. The audited immediate save/upsert path places the JSON `priority` in metadata but does not synchronously synthesize that selector attribute. Therefore the physical file and plugin status can reflect a new priority before the active selector does.

Actions:

1. wait for CPA's auth-file watcher to resynthesize the file;
2. restart/reload CPA if routing still uses the old tier;
3. trigger the plugin management refresh after restart;
4. test with a new session ID;
5. confirm only safe fields from the physical auth files if needed, for example:

   ```bash
   docker exec cli-proxy-api sh -c \
     'for f in /root/.cli-proxy-api/*.json; do jq -c "{file:\"${f##*/}\",type,email,priority,disabled}" "$f"; done'
   ```

This limitation is why release/install acceptance includes a CPA restart and new-session routing validation. The plugin does not patch CPA's selector in v0.1.0.

## A disabled auth appears enabled after another tool saved it

At the audited upstream revision, the immediate `host.auth.save` rebuild path constructs an active runtime record and does not preserve quarantine state reliably. `reset-priority` avoids this hazard by never saving a credential CPA definitively reports as disabled, unauthorized, revoked, or reauthentication-required; it reports desired sentinel `0` in authenticated status instead.

If another plugin or management operation saved a disabled auth, reapply the intended disabled state through CPA and restart/reload. Do not ask `reset-priority` to force-write `0` to a disabled file.

## Existing sessions do not move to the new highest priority

At the audited revision, an established session-affinity binding outranks credential priority while the bound credential remains available. Priority controls:

- cold/new session bindings;
- requests without a session identity;
- failover when the bound credential is genuinely unavailable.

A priority change is not a session migration mechanism. Test with a new session ID. Older CPA revisions had a reported priority-prefilter interaction that could preempt affinity bindings, so validate behavior after CPA upgrades/downgrades.

## A quota, 429, cooldown, or unavailable auth kept its normal priority

That is intentional. These are request-time availability conditions owned by CPA, not credential-health evidence and not reset-ranking signals. CPA should skip/fall through while the condition applies, without the plugin rewriting the weekly priority order.

If CPA does not fail over, troubleshoot CPA's cooldown/retry/selector behavior separately; do not add utilization or 429 scoring to this plugin.

## Dry-run logged changes but auth files did not change

That is correct. Dry-run performs discovery, provider reads, ranking, timers, status, and proposed-change logs, but makes zero `host.auth.save` calls.

Set `dry-run: false` only after the computed order is accepted, then restart/reconfigure CPA as required and trigger an authenticated refresh.

## Management refresh works; browser resource cannot refresh

This separation is intentional:

- `POST /v0/management/plugins/reset-priority/refresh` is authenticated and mutating.
- `GET /v0/resource/plugins/reset-priority/status` is an unauthenticated, read-only, static readiness shell with no account-level details.

Query parameters sent to the resource route are inert. Never expose mutation or operational account data through the resource route.

## Provider request failures

The plugin uses CPA's proxy-aware `host.http.do` callback with a per-request timeout (default `10s`). It never logs provider bodies or authorization headers.

- Claude uses `https://api.anthropic.com/api/oauth/usage` and reads only `seven_day.resets_at`.
- Codex uses `https://chatgpt.com/backend-api/wham/usage` and reads only a window declared as exactly `604800` seconds/`10080` minutes.

Check outbound connectivity/proxy configuration and OAuth health in CPA. A single timeout, network failure, 5xx, or telemetry schema error does not quarantine the auth.

## Release archive or checksum validation fails

A release ZIP is valid only when its root contains exactly:

```text
LICENSE
reset-priority.so      # Linux
reset-priority.dylib   # macOS
reset-priority.dll     # Windows
```

Only the library for that archive's target OS is allowed. Versioned aliases, a second native library, nested paths, directories, README files, and other extras are rejected. Per-archive `.sha256` files and combined `checksums.txt` must use lowercase SHA-256 digests and bare asset filenames without `./`, `dist/`, or other path prefixes. The full release verifier also rejects a sixth platform ZIP; v0.1.0 has exactly five ZIP assets plus `checksums.txt`.

## A Plugin Store update installed but the old version still loads

- Restart CPA after updating a loaded native library.
- Check for both a manual root copy and a store version:

  ```bash
  docker exec cli-proxy-api sh -c \
    'find /CLIProxyAPI/plugins -maxdepth 4 -type f -name "reset-priority*" -print'
  ```

- Remove the obsolete manual copy if the store-managed version should win.
- On Windows, a loaded DLL cannot be overwritten; stop/restart as directed by the Management API.
- Confirm the install path and `restart_required` fields returned by CPA.

## Reinstalling at the same library path fails to initialize until CPA restarts

This is by design. When the resident CPA process runs the plugin's terminal native shutdown (unload/remove), the shared library's Go runtime state in that process becomes permanently terminal: `dlclose` does not reset a Go runtime, and reinitializing terminal state would be unsafe. A subsequent `cliproxy_plugin_init` for the same library path in the same CPA process is therefore refused with a nonzero result rather than partially reviving the old runtime.

A same-path native unload/reinstall (or remove-then-install) always requires a CPA restart before the plugin can initialize again. Treat any init failure after an in-process unload as this condition, not as a defective library.

## Plugin unload or CPA shutdown hangs while draining the plugin

At ABI v1, host callbacks carry no cancellation. During terminal native shutdown the plugin must wait for every host callback that already entered the host (including a `host.http.do` whose plugin-side request timeout already expired) to fully return before the host may release the host API table and unload the library. Abandoning such a callback on a plugin-side timer would risk a use-after-free crash inside CPA during unload, so the plugin deliberately implements no bounded drain.

Consequence at the audited CPA revision: if a host callback never returns (for example an upstream HTTP request with no effective host-side deadline through a broken proxy), plugin shutdown blocks inside CPA's unload path until the CPA process itself exits. If an unload hangs, check outbound connectivity/proxy health and stop the CPA process; this is a current-host/ABI limitation, not plugin state corruption.

## Smoke test

Build the native Linux library and run:

```bash
make build
./scripts/smoke-test.sh ./reset-priority.so
```

The script pulls `eceasy/cli-proxy-api:latest`, verifies plugin architecture, uses a disposable named plugin volume and dry-run config, and retains evidence under `dist/smoke/<run-id>/`. Success requires HTTP 200 plus the static page title and the non-sensitive marker `Dry-run configuration recommended`; it does not depend on dynamic account or runtime status text.

Real OAuth accounts are not required and must not be injected into the smoke environment.
