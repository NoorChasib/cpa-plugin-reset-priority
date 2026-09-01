# Custom Plugin Store installation

The root [`registry.json`](../registry.json) is a single-plugin custom CPA registry. It points at this repository; CPA resolves the latest GitHub release and selects the archive for its current operating system and architecture.

Store downloads are executable code. Use only registries and releases you trust.

## Prerequisites

- `/CLIProxyAPI/plugins` is backed by a persistent named volume.
- `plugins.enabled: true` and `plugins.dir: "plugins"` are present.
- A published release contains the matching platform ZIP and `checksums.txt`.
- The CPA build supports native plugins.
- The plugin config starts with `dry-run: true`.

See [Docker Compose installation](install-docker-compose.md) first.

## Add the registry source

Merge this source into the existing plugin configuration:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-reset-priority/main/registry.json"
```

Keep any existing trusted store sources. Restart/reload CPA after changing `config.yaml` if the deployed host does not hot-reload this field.

The built-in official registry remains available; this entry adds another source.

## Install through the Management Center

Current Management Center labels can change between CPA releases. The intended flow is:

1. Open CPA Management Center.
2. Open **Plugin Store**.
3. Refresh/reload the store sources.
4. Select **Reset Priority** (`reset-priority`).
5. Install the latest release.
6. Confirm the plugin is enabled and configure it with `dry-run: true`.
7. Restart CPA if the result reports that a restart is required.
8. Open the read-only status resource and run the authenticated refresh action.

If the UI wording differs, use the Management API below rather than guessing at a different endpoint.

## Install through the Management API

All Management API examples use a placeholder, never a real key:

```bash
export CPA_MANAGEMENT_KEY='<MANAGEMENT_KEY>'
export CPA_BASE_URL='http://127.0.0.1:8317'
```

List store entries and source IDs:

```bash
curl --fail --silent --show-error \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  "${CPA_BASE_URL}/v0/management/plugin-store"
```

Install the latest release:

```bash
curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  "${CPA_BASE_URL}/v0/management/plugin-store/reset-priority/install"
```

If more than one store source publishes the same plugin ID, select the `source_id` returned by the list request:

```bash
export CPA_PLUGIN_SOURCE_ID='<SOURCE_ID>'

curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  "${CPA_BASE_URL}/v0/management/plugin-store/reset-priority/install?source=${CPA_PLUGIN_SOURCE_ID}"
```

A fixed version can be requested when supported by the deployed CPA release:

```bash
export VERSION='<VERSION>'

curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  -H 'Content-Type: application/json' \
  --data "{\"version\":\"${VERSION}\"}" \
  "${CPA_BASE_URL}/v0/management/plugin-store/reset-priority/install"
```

The install response includes the installed path/version and whether a restart is required.

Current store installs are versioned beneath the platform directory, for example on Linux amd64:

```text
/CLIProxyAPI/plugins/linux/amd64/reset-priority-v<VERSION>.so
```

Manual root installs use `/CLIProxyAPI/plugins/reset-priority.so`; both layouts are loader candidates in the audited CPA host. Do not keep both unless intentionally testing precedence. Remove an obsolete manual root copy before relying on the store-managed version.

## Configure and validate

Merge the plugin subtree:

```yaml
plugins:
  configs:
    reset-priority:
      enabled: true
      priority: 10 # CPA plugin load priority, not credential priority
      reconcile-interval: 1h
      request-timeout: 10s
      priority-floor: 100
      priority-step: 100
      quarantine-priority: 0
      manage-claude: true
      manage-codex: true
      dry-run: true
```

Restart/reload CPA when required. Then:

```bash
curl --fail --silent --show-error \
  "${CPA_BASE_URL}/v0/resource/plugins/reset-priority/status"

curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  "${CPA_BASE_URL}/v0/management/plugins/reset-priority/refresh"
```

The public resource call confirms only that the plugin route loaded. Inspect authenticated `GET /v0/management/plugins/reset-priority/status`; only after that dry-run status matches all real accounts should `dry-run` become `false`.

## Update

Publishing a new GitHub release is sufficient for update discovery; the registry normally does not need a version change. CPA compares the installed version with the repository's latest valid release.

1. Return to `dry-run: true`.
2. Refresh the Plugin Store list and confirm `update_available`.
3. Run the same install endpoint; it is also the update endpoint:

```bash
curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  "${CPA_BASE_URL}/v0/management/plugin-store/reset-priority/install"
```

4. Restart if `restart_required` is true or if the loaded library cannot be replaced/reloaded safely.
5. Validate status, logs, the Docker volume, a management refresh, and new-session routing.
6. Restore `dry-run: false` only after acceptance.

Windows does not permit overwriting a loaded DLL; an update may require stopping/restarting CPA before the file can be replaced. Treat any `restart_required` or loaded-library conflict as authoritative.

Additionally, if CPA has already run the plugin's terminal native shutdown in the current process (unload/remove), reinstalling at the same library path cannot re-initialize without a restart: the plugin refuses `cliproxy_plugin_init` after terminal shutdown rather than reinitialize terminal Go runtime state. A same-path native unload/reinstall always requires a CPA restart. See [troubleshooting](troubleshooting.md).

## Disable without uninstalling

The current Management API can update only the enabled flag:

```bash
curl --fail --silent --show-error -X PATCH \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  -H 'Content-Type: application/json' \
  --data '{"enabled":false}' \
  "${CPA_BASE_URL}/v0/management/plugins/reset-priority/enabled"
```

A loaded native library may remain mapped until restart, but its plugin runtime is disabled/quiesced according to the host lifecycle.

## Uninstall

The store-aware uninstall endpoint removes the local plugin file and saved plugin configuration:

```bash
curl --silent --show-error -X DELETE \
  -H "Authorization: Bearer ${CPA_MANAGEMENT_KEY}" \
  "${CPA_BASE_URL}/v0/management/plugins/reset-priority"
```

A loaded plugin that cannot be unloaded can return HTTP `409` with `restart_required: true`. Restart CPA and repeat/complete removal as directed by the response.

Once removal has run the plugin's terminal native shutdown in the resident CPA process, a reinstall at the same library path will not initialize until CPA restarts (init is refused after terminal shutdown by design).

After uninstall:

1. remove any separate manual root copy at `/CLIProxyAPI/plugins/reset-priority.so`;
2. confirm `plugins.configs.reset-priority` was removed or remains intentionally disabled;
3. restart CPA;
4. confirm the plugin no longer appears;
5. optionally remove the custom `store-sources` URL.

Do not delete the entire named plugin volume unless intentionally uninstalling every persisted plugin.

Uninstall does not restore historical OAuth credential priorities. Set any desired static priorities through CPA after removal.
