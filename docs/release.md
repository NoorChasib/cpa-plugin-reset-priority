# Release process

This repository releases native CPA shared libraries through GitHub Actions. Do not create a release until the operator acceptance checklist has passed against the intended CLIProxyAPI deployment.

## Release matrix

v0.1.0 supports exactly these five native targets:

```text
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
windows/amd64
```

The gaps are intentional: Windows ARM64, FreeBSD, and other Linux architectures are not built. Linux amd64 and arm64 are built on matching-architecture GitHub runners inside pinned `manylinux2014` containers (GLIBC 2.17 baseline), not as generic Ubuntu 24.04 binaries and not under CPU emulation. macOS and Windows are built natively on matching hosted runners. Ordinary Go cross-compilation is not sufficient for these CGO `c-shared` artifacts.

A version `<VERSION>` release must contain:

```text
reset-priority_<VERSION>_linux_amd64.zip
reset-priority_<VERSION>_linux_arm64.zip
reset-priority_<VERSION>_darwin_amd64.zip
reset-priority_<VERSION>_darwin_arm64.zip
reset-priority_<VERSION>_windows_amd64.zip
checksums.txt
```

Each ZIP contains exactly the platform library and `LICENSE` at the ZIP root. Directories, documentation files, versioned library aliases, and foreign-platform libraries are release errors. The library basename is:

```text
Linux   reset-priority.so
Darwin  reset-priority.dylib
Windows reset-priority.dll
```

`checksums.txt` uses sha256sum format with bare asset filenames:

```text
<SHA256>  reset-priority_<VERSION>_<GOOS>_<GOARCH>.zip
```

## 1. Establish the compatibility baseline

Before release, record:

- the CLIProxyAPI image reference and immutable digest tested;
- the CPA version/commit printed in logs, when available;
- host OS/architecture;
- whether the smoke test and real-account dry-run were completed;
- known upstream selector/session-affinity behavior.

The implementation was audited against CLIProxyAPI commit `81e1b5374f99c212f196f34956eeed964a46b8fa` (nearest release `v7.2.146`). Re-audit host callbacks, selector priority propagation, route shapes, and Plugin Store behavior when the release targets a newer host.

## 2. Prepare the version

The authoritative plugin version is:

```text
internal/plugin/runtime.go: PluginVersion
```

The Makefile derives its default `VERSION` from this constant. The packager rejects a different requested version.

For a version bump:

1. update `PluginVersion` in `internal/plugin/runtime.go`;
2. update version-specific documentation where needed;
3. do not add a leading `v` inside code or asset names;
4. use the leading `v` only for the Git tag, for example `v0.1.0`.

The root `registry.json` intentionally omits a version field. CPA discovers updates from the repository's latest valid GitHub release, so a normal release does not require a registry version edit.

## 3. Run local automated validation

```bash
make fmt-check
make vet
make lint
make test
make race
make build
make check-linux-glibc  # Linux only; package runs this automatically on Linux
make package
make check-release
bash -n scripts/smoke-test.sh
```

Expected native outputs include:

```text
reset-priority.so                 # Linux; extension varies by host OS
dist/reset-priority_<VERSION>_<GOOS>_<GOARCH>.zip
dist/reset-priority_<VERSION>_<GOOS>_<GOARCH>.zip.sha256
```

`make package` creates a deterministic archive and self-validates its exact two-file root layout. On Linux it first runs `make check-linux-glibc`, which verifies the ELF machine and rejects any referenced GLIBC symbol version newer than `2.17`. `make check-release` verifies the archive, canonical lowercase bare-filename sidecar, and exact requested platform set.

On a Linux distribution newer than the manylinux2014 baseline (for example Ubuntu 24.04), a locally built library legitimately references newer GLIBC symbol versions, so run `make package GLIBC_ENFORCE=0` to exercise packaging and `make check-release` without the release-only GLIBC gate. Ordinary CI does the same. The strict gate remains the default and is enforced unchanged inside the pinned `manylinux2014` release containers.

A local Linux package is only a release-equivalent artifact when it was built in the matching `manylinux2014` environment. Passing the GLIBC symbol-version gate on a different distribution is useful evidence, but the tagged workflow remains the authoritative release build path.

## 4. Run the Docker smoke test

On a compatible Linux host with Docker:

```bash
./scripts/smoke-test.sh ./reset-priority.so
```

Optionally pin a specific image rather than relying on a moving tag:

```bash
CPA_SMOKE_IMAGE='<CPA_IMAGE_REFERENCE_OR_DIGEST>' \
  ./scripts/smoke-test.sh ./reset-priority.so
```

The smoke script:

- pulls and records the CPA image ID/digest/platform;
- validates that the plugin is an architecture-compatible ELF shared object;
- starts a disposable container with a disposable named plugin volume;
- supplies a placeholder-only dry-run config;
- verifies HTTP 200, `<title>Reset Priority</title>`, and the static non-sensitive marker `Dry-run configuration recommended` from the read-only status resource;
- captures logs and inspect data under `dist/smoke/<run-id>/`;
- removes the disposable container and volume.

Do not add real OAuth credentials or a management secret to the smoke environment.

If Docker cannot run, mark the smoke test as unexecuted in release notes. Do not claim it passed.

## 5. Run real-deployment dry-run acceptance

Against the target CPA deployment:

1. persist `/CLIProxyAPI/plugins` with a named volume;
2. install the candidate plugin;
3. set `dry-run: true`;
4. restart/reload CPA;
5. trigger authenticated `POST /v0/management/plugins/reset-priority/refresh`;
6. complete the README operator checklist;
7. verify Claude uses only `seven_day.resets_at`;
8. verify Codex uses only the exact `604800`-second weekly window;
9. verify 429/quota/cooldown/unavailable do not quarantine;
10. verify disabled/reauth-required accounts are at sentinel `0`;
11. verify recovered accounts require a fresh post-recovery future reset;
12. verify exact deadline demotion occurs before network recovery;
13. test routing with new session IDs after any necessary CPA restart.

Keep all evidence sanitized. Status screenshots can contain account labels; redact them if they are private.

## 6. Review the release diff

Before tagging:

```bash
git status --short
git diff --check
git diff --stat
git diff
```

Confirm:

- no secret/token/auth fixture was added;
- only intended files changed;
- `LICENSE` is included and current;
- `config.example.yaml` remains dry-run-first;
- routes and config names match implementation;
- `registry.json` is valid;
- release documentation names the five supported platforms and gaps.

## 7. Tag and push

Only after CI passes on the release commit:

```bash
export VERSION='<VERSION>'
git tag -a "v${VERSION}" -m "Release v${VERSION}"
git push origin "v${VERSION}"
```

The tag must match the compiled `PluginVersion`. The release workflow triggers for `v*` tags and validates the version before packaging.

## 8. GitHub Actions release behavior

The release workflow:

1. runs Linux amd64 and arm64 jobs on matching-architecture runners;
2. builds each Linux library inside its pinned architecture-specific `manylinux2014` container with the checksum-verified Go 1.26.0 toolchain archive;
3. runs `readelf` checks for the expected ELF machine and rejects any GLIBC requirement newer than `2.17` before packaging;
4. builds macOS amd64/arm64 and Windows amd64 natively on matching hosted runners;
5. packages exactly `reset-priority.<ext>` plus `LICENSE` at the ZIP root;
6. creates and validates a canonical lowercase per-archive `.sha256` sidecar;
7. downloads all artifacts and enforces the exact five-platform matrix, rejecting extra platform archives;
8. creates bare-filename lowercase `checksums.txt` with exactly five lines;
9. uploads only the five ZIPs and `checksums.txt`, refusing to proceed if an existing release contains any other attached asset;
10. verifies the published attached-asset set after upload.

Build jobs have read-only repository permission. The tag-triggered release job alone receives `contents: write`.

## 9. Verify published assets

After the workflow completes:

```bash
export VERSION='<VERSION>'

gh release view "v${VERSION}" \
  --repo NoorChasib/cpa-plugin-reset-priority

gh release download "v${VERSION}" \
  --repo NoorChasib/cpa-plugin-reset-priority \
  --dir "dist/release-v${VERSION}"

(
  cd "dist/release-v${VERSION}"
  sha256sum --check --strict checksums.txt
)

go run ./tools/packager \
  -verify \
  -version "${VERSION}" \
  -out "dist/release-v${VERSION}" \
  -require 'linux_amd64,linux_arm64,darwin_amd64,darwin_arm64,windows_amd64' \
  -checksums "dist/release-v${VERSION}/checksums.txt"
```

Published releases contain exactly the five ZIPs plus `checksums.txt` and no per-ZIP `.sha256` sidecars, so `-checksums` verifies every downloaded archive's layout and digest against the combined file. (Local `make check-release` continues to verify the per-archive sidecars the packager writes into `dist/`.)

Inspect each ZIP listing and ensure the dynamic library is at root, not nested:

```bash
for archive in "dist/release-v${VERSION}"/*.zip; do
  unzip -l "${archive}"
done
```

Each listing must contain only `LICENSE` and the expected unversioned library. Independently inspect both published Linux libraries when `readelf` is available:

```bash
for arch in amd64 arm64; do
  lib="dist/release-v${VERSION}/reset-priority-linux-${arch}.so"
  unzip -p "dist/release-v${VERSION}/reset-priority_${VERSION}_linux_${arch}.zip" \
    reset-priority.so > "${lib}"
  readelf -h "${lib}" | grep 'Machine:'
  readelf --version-info --wide "${lib}" \
    | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' \
    | sort -Vu
done
```

The machine values must be x86-64 and AArch64 respectively, and no listed GLIBC version may exceed `2.17`.

## 10. Verify custom-store update discovery

1. Fetch the raw registry URL from the target network.
2. Confirm the Plugin Store lists `reset-priority` from the expected source.
3. Confirm the latest release version and matching platform asset are shown.
4. Install/update in a disposable or dry-run deployment.
5. Restart when requested.
6. Verify the installed library remains after container recreation.

A new GitHub release should be sufficient for discovery; do not add a registry `version` solely to announce an update.

## 11. Release notes checklist

Release notes must state:

- plugin version and Git commit;
- CPA image reference, digest, and version/commit tested;
- smoke-test result or explicit unexecuted status;
- real-account dry-run result or explicit unexecuted status;
- supported five-platform matrix and unsupported gaps;
- `host.auth.save` whole-document/quarantined-auth safety caveat;
- runtime selector propagation caveat and whether restart was needed;
- session-affinity behavior observed;
- Codex lazy-reset passive-only limitation;
- install/update/uninstall documentation links;
- SHA-256 verification result.

Do not include account labels, auth IDs, tokens, provider response bodies, management keys, or private deployment URLs.

## 12. Post-release operator acceptance

Install from the published release in dry-run and repeat the README checklist. Enable writes only after expected ordering is confirmed. Then validate new-session routing and persistence through a CPA restart/redeploy.

If a release is defective, disable the plugin, return to dry-run, and publish a corrected version. Do not silently replace tag contents without documenting why; immutable release history is preferable even though the workflow can clobber assets on an existing tag.
