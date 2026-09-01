# Docker Compose and Coolify installation

This guide adds `reset-priority` without replacing existing CLIProxyAPI auth, logging, routing, model, remote-management, or session-affinity configuration.

## 1. Persist the plugin directory

Add a dedicated named volume mounted at `/CLIProxyAPI/plugins`. Preserve the existing auth and log volumes.

```yaml
services:
  cli-proxy-api:
    image: 'eceasy/cli-proxy-api:latest'
    container_name: cli-proxy-api
    restart: unless-stopped
    ports:
      - '8317:8317'
    volumes:
      - './config.yaml:/CLIProxyAPI/config.yaml'
      - 'cliproxy-auths:/root/.cli-proxy-api'
      - 'cliproxy-logs:/CLIProxyAPI/logs'
      - 'cliproxy-plugins:/CLIProxyAPI/plugins'

volumes:
  cliproxy-auths:
  cliproxy-logs:
  cliproxy-plugins:
```

The plugin volume is mandatory for durable installs: binaries written under `/CLIProxyAPI/plugins` otherwise disappear when Coolify or Compose recreates the container.

For local development only, a bind mount such as `./plugins:/CLIProxyAPI/plugins` can replace the named plugin volume when direct host access is useful.

Apply the Compose change using the normal deployment procedure. Confirm the mount before installing:

```bash
docker inspect cli-proxy-api \
  --format '{{range .Mounts}}{{println .Destination .Type .Name}}{{end}}'
```

The output must include `/CLIProxyAPI/plugins` as a volume destination.

## 2. Merge the plugin configuration

Merge the following keys into the existing `config.yaml`. Do not replace unrelated settings.

```yaml
plugins:
  enabled: true
  dir: "plugins"

  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-reset-priority/main/registry.json"

  configs:
    reset-priority:
      enabled: true
      priority: 10 # plugin load/order priority, NOT credential priority

      reconcile-interval: 1h
      request-timeout: 10s

      priority-floor: 100
      priority-step: 100
      quarantine-priority: 0

      manage-claude: true
      manage-codex: true

      dry-run: true
```

If the existing config already has `plugins`, merge the nested keys. YAML cannot contain two sibling `plugins:` mappings.

The recommended routing strategy is:

```yaml
routing:
  strategy: "fill-first"
```

Do not replace an existing routing section blindly. The plugin itself never edits routing.

Restart or redeploy CPA after changing the mount/configuration:

```bash
docker compose up -d
```

## 3. Choose an installation method

For normal operation, use the [custom Plugin Store guide](custom-plugin-store.md).

For pre-release testing, install a release archive or locally built library manually as described below.

## Manual Linux installation

### Determine the container architecture

```bash
docker exec cli-proxy-api uname -m
```

Typical mapping:

| `uname -m` | Release suffix |
| --- | --- |
| `x86_64` | `linux_amd64` |
| `aarch64` or `arm64` | `linux_arm64` |

Do not install an artifact for a different architecture.

### Install a release archive

Use placeholders for the version and platform until selecting a real published release:

```bash
export VERSION='<VERSION>'
export PLATFORM='<linux_amd64-or-linux_arm64>'

curl --fail --location --output "reset-priority_${VERSION}_${PLATFORM}.zip" \
  "https://github.com/NoorChasib/cpa-plugin-reset-priority/releases/download/v${VERSION}/reset-priority_${VERSION}_${PLATFORM}.zip"

curl --fail --location --output checksums.txt \
  "https://github.com/NoorChasib/cpa-plugin-reset-priority/releases/download/v${VERSION}/checksums.txt"

grep "  reset-priority_${VERSION}_${PLATFORM}.zip$" checksums.txt \
  | sha256sum --check --strict

unzip -o "reset-priority_${VERSION}_${PLATFORM}.zip" reset-priority.so LICENSE

docker cp reset-priority.so cli-proxy-api:/CLIProxyAPI/plugins/reset-priority.so
docker restart cli-proxy-api
```

The root-level filename is required for this manual path: `/CLIProxyAPI/plugins/reset-priority.so`.

### Install a local build

On a compatible Linux machine with Go 1.26.0 and a C compiler:

```bash
make build
docker cp reset-priority.so cli-proxy-api:/CLIProxyAPI/plugins/reset-priority.so
docker restart cli-proxy-api
```

Do not build on one CPU architecture and copy the library into another.

### Verify load

```bash
docker exec cli-proxy-api sh -c \
  'test -f /CLIProxyAPI/plugins/reset-priority.so && ls -l /CLIProxyAPI/plugins/reset-priority.so'

docker logs --tail=200 cli-proxy-api
```

Open the static unauthenticated readiness page:

```text
http://127.0.0.1:8317/v0/resource/plugins/reset-priority/status
```

The authenticated routes require the CPA management key:

```bash
export CPA_MANAGEMENT_KEY='<MANAGEMENT_KEY>'

curl --fail --silent --show-error \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/reset-priority/status

curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  http://127.0.0.1:8317/v0/management/plugins/reset-priority/refresh
```

The resource route is read-only, unauthenticated, and intentionally contains no account-level status. Use the authenticated management status routes for dry-run validation — JSON at `/v0/management/plugins/reset-priority/status`, or the browser HTML view at `/v0/management/plugins/reset-priority/status/html` (same management authentication; includes a Refresh now action and shows only sanitized fields with file-name or redacted identifiers). At the audited CPA revision, management authentication is header-only, so ordinary address-bar navigation and query parameters cannot authenticate the HTML route; use a browser/profile or authenticated reverse proxy that supplies the management header to both GET and POST requests. Physical filenames can be identifying. Do not expose filenames, the management key, or private status data in screenshots or shell transcripts.

## Dry-run-first validation

Inspect authenticated `GET /v0/management/plugins/reset-priority/status` and keep `dry-run: true` until all of the following are true:

1. every intended physical Claude/Codex OAuth auth appears;
2. Claude uses only `seven_day.resets_at`;
3. Codex uses only the exact `604800`-second weekly window;
4. desired priorities use the `100` floor and `100` step;
5. disabled/reauth-required/recovering accounts are at `0` and excluded from the healthy count;
6. quota, 429, cooldown, and ordinary unavailability do not create quarantine;
7. exact timestamps and the next deadline timer are plausible.

Note that dry-run suppresses all writes, so the quarantine sentinel `0` appears in status only. Once `dry-run: false`, the sentinel is written into the quarantined account's physical auth JSON once per quarantine; see the [write-safety tradeoff](troubleshooting.md#a-quarantined-auth-briefly-appears-enabled-after-the-sentinel-write).

Then change only:

```yaml
dry-run: false
```

Restart/redeploy or use CPA's current plugin-config reload mechanism. Trigger the authenticated refresh route, then validate the physical priorities and routing with new sessions. See the upstream selector caveat in [troubleshooting](troubleshooting.md#priorities-were-written-but-routing-did-not-change-immediately).

## Manual update

1. Return to `dry-run: true` while validating a new plugin/CPA combination.
2. Download and verify the new release archive/checksum.
3. Replace the root library and restart CPA:

```bash
docker cp reset-priority.so cli-proxy-api:/CLIProxyAPI/plugins/reset-priority.so
docker restart cli-proxy-api
```

4. Check logs and status, trigger a refresh, and complete the acceptance checklist before restoring `dry-run: false`.

## Manual uninstall

1. Disable or remove `plugins.configs.reset-priority` from `config.yaml`.
2. Remove the manual library and restart:

```bash
docker exec cli-proxy-api rm -f /CLIProxyAPI/plugins/reset-priority.so
docker restart cli-proxy-api
```

3. Confirm the plugin no longer appears in logs or CPA plugin state.

Uninstalling the plugin does not restore previous credential priorities. If the operator wants different static priorities afterward, edit them through the normal CPA auth-management path.

Do not delete the `cliproxy-plugins` volume unless intentionally removing every plugin stored in it.
