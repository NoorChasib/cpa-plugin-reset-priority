#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${CPA_SMOKE_IMAGE:-eceasy/cli-proxy-api:latest}"
PLUGIN_INPUT="${1:-${ROOT_DIR}/reset-priority.so}"
TIMEOUT_SECONDS="${CPA_SMOKE_TIMEOUT_SECONDS:-90}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}"
CONTAINER_NAME="cpa-reset-priority-smoke-${RUN_ID}"
PLUGIN_VOLUME="cpa-reset-priority-smoke-plugins-${RUN_ID}"
ARTIFACT_DIR="${CPA_SMOKE_ARTIFACT_DIR:-${ROOT_DIR}/dist/smoke/${RUN_ID}}"

for command_name in docker curl install realpath; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    printf 'required command not found: %s\n' "${command_name}" >&2
    exit 2
  fi
done

if ! [[ "${TIMEOUT_SECONDS}" =~ ^[1-9][0-9]*$ ]]; then
  printf 'CPA_SMOKE_TIMEOUT_SECONDS must be a positive integer, got %s\n' "${TIMEOUT_SECONDS}" >&2
  exit 2
fi
if [[ ! -f "${PLUGIN_INPUT}" ]]; then
  printf 'local plugin library not found: %s\n' "${PLUGIN_INPUT}" >&2
  printf 'build it first with: make build\n' >&2
  exit 2
fi

PLUGIN_INPUT="$(realpath "${PLUGIN_INPUT}")"
TMP_DIR="$(mktemp -d)"
STAGED_PLUGIN="${TMP_DIR}/reset-priority.so"
LOG_FILE="${ARTIFACT_DIR}/container.log"
RESPONSE_FILE="${ARTIFACT_DIR}/status.html"
CONTAINER_CREATED=false
VOLUME_CREATED=false

mkdir -p "${ARTIFACT_DIR}"
install -m 0755 -- "${PLUGIN_INPUT}" "${STAGED_PLUGIN}"

# Invoked indirectly by the EXIT trap below.
# shellcheck disable=SC2329
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e

  if [[ "${CONTAINER_CREATED}" == true ]] && docker container inspect "${CONTAINER_NAME}" >/dev/null 2>&1; then
    docker logs "${CONTAINER_NAME}" >"${LOG_FILE}" 2>&1 || true
    docker container inspect "${CONTAINER_NAME}" >"${ARTIFACT_DIR}/container-inspect.json" 2>/dev/null || true
    local runtime_version
    runtime_version="$(grep -m1 '^CLIProxyAPI Version:' "${LOG_FILE}" 2>/dev/null || true)"
    printf 'runtime_version=%s\n' "${runtime_version:-unavailable in captured logs}" >>"${ARTIFACT_DIR}/image.txt"
    docker rm --force "${CONTAINER_NAME}" >/dev/null 2>&1 || true
  fi
  if [[ "${VOLUME_CREATED}" == true ]]; then
    docker volume rm --force "${PLUGIN_VOLUME}" >/dev/null 2>&1 || true
  fi
  rm -rf -- "${TMP_DIR}"

  if (( status != 0 )); then
    printf 'smoke test failed; captured evidence: %s\n' "${ARTIFACT_DIR}" >&2
    if [[ -s "${LOG_FILE}" ]]; then
      printf '%s\n' '--- CPA container logs (last 200 lines) ---' >&2
      tail -n 200 "${LOG_FILE}" >&2 || true
    fi
  else
    printf 'smoke evidence retained at %s\n' "${ARTIFACT_DIR}"
  fi
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'pulling CPA image %s\n' "${IMAGE}"
docker pull "${IMAGE}" | tee "${ARTIFACT_DIR}/pull.txt"
docker image inspect "${IMAGE}" >"${ARTIFACT_DIR}/image-inspect.json"

IMAGE_ID="$(docker image inspect --format '{{.Id}}' "${IMAGE}")"
IMAGE_ARCH="$(docker image inspect --format '{{.Architecture}}' "${IMAGE}")"
IMAGE_OS="$(docker image inspect --format '{{.Os}}' "${IMAGE}")"
IMAGE_DIGEST="$(docker image inspect --format '{{join .RepoDigests ","}}' "${IMAGE}")"
IMAGE_VERSION="$(docker image inspect --format '{{if .Config.Labels}}{{index .Config.Labels "org.opencontainers.image.version"}}{{end}}' "${IMAGE}")"
printf 'image=%s\nimage_id=%s\nimage_digest=%s\nimage_version_label=%s\nimage_platform=%s/%s\n' \
  "${IMAGE}" "${IMAGE_ID}" "${IMAGE_DIGEST:-unavailable}" "${IMAGE_VERSION:-unavailable}" "${IMAGE_OS}" "${IMAGE_ARCH}" \
  >"${ARTIFACT_DIR}/image.txt"
printf 'CPA image: %s (%s, version label: %s, %s/%s)\n' \
  "${IMAGE_ID}" "${IMAGE_DIGEST:-no digest}" "${IMAGE_VERSION:-unavailable}" "${IMAGE_OS}" "${IMAGE_ARCH}"

if command -v file >/dev/null 2>&1; then
  PLUGIN_FILE_TYPE="$(file -b "${STAGED_PLUGIN}")"
  printf 'plugin_file=%s\nplugin_type=%s\n' "${PLUGIN_INPUT}" "${PLUGIN_FILE_TYPE}" >"${ARTIFACT_DIR}/plugin.txt"
  if [[ "${PLUGIN_FILE_TYPE}" != ELF*shared\ object* ]]; then
    printf 'local plugin is not an ELF shared object: %s\n' "${PLUGIN_FILE_TYPE}" >&2
    exit 1
  fi
  case "${IMAGE_ARCH}" in
    amd64)
      [[ "${PLUGIN_FILE_TYPE}" == *x86-64* ]] || {
        printf 'plugin architecture does not match amd64 image: %s\n' "${PLUGIN_FILE_TYPE}" >&2
        exit 1
      }
      ;;
    arm64)
      [[ "${PLUGIN_FILE_TYPE}" == *ARM\ aarch64* || "${PLUGIN_FILE_TYPE}" == *ARM64* ]] || {
        printf 'plugin architecture does not match arm64 image: %s\n' "${PLUGIN_FILE_TYPE}" >&2
        exit 1
      }
      ;;
    *)
      printf 'unsupported CPA image architecture for this smoke test: %s\n' "${IMAGE_ARCH}" >&2
      exit 1
      ;;
  esac
fi

cat >"${TMP_DIR}/config.yaml" <<'YAML'
host: "0.0.0.0"
port: 8317
auth-dir: "/root/.cli-proxy-api"
debug: true
logging-to-file: false
plugins:
  enabled: true
  dir: "plugins"
  configs:
    reset-priority:
      enabled: true
      priority: 10
      manage-claude: true
      manage-codex: true
      reconcile-interval: 1h
      request-timeout: 10s
      priority-floor: 100
      priority-step: 100
      quarantine-priority: 0
      dry-run: true
YAML
chmod 0600 "${TMP_DIR}/config.yaml"

docker volume create "${PLUGIN_VOLUME}" >/dev/null
VOLUME_CREATED=true

docker create \
  --name "${CONTAINER_NAME}" \
  --publish "127.0.0.1::8317" \
  --mount "type=volume,src=${PLUGIN_VOLUME},dst=/CLIProxyAPI/plugins" \
  --mount "type=bind,src=${TMP_DIR}/config.yaml,dst=/CLIProxyAPI/config.yaml,readonly" \
  "${IMAGE}" >/dev/null
CONTAINER_CREATED=true

# Copy a staged, fixed-name, executable library into the stopped container. The
# destination is the named plugin volume, so CPA never sees a partial write.
docker cp "${STAGED_PLUGIN}" "${CONTAINER_NAME}:/CLIProxyAPI/plugins/reset-priority.so"
docker start "${CONTAINER_NAME}" >/dev/null

HOST_PORT=""
DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
while (( SECONDS < DEADLINE )); do
  if [[ "$(docker container inspect --format '{{.State.Running}}' "${CONTAINER_NAME}" 2>/dev/null || true)" != true ]]; then
    printf 'CPA container exited before plugin verification\n' >&2
    exit 1
  fi

  if [[ -z "${HOST_PORT}" ]]; then
    PORT_MAPPING="$(docker port "${CONTAINER_NAME}" 8317/tcp 2>/dev/null | grep -m1 '^127\.0\.0\.1:' || true)"
    if [[ -n "${PORT_MAPPING}" ]]; then
      HOST_PORT="${PORT_MAPPING##*:}"
    fi
  fi

  if [[ -n "${HOST_PORT}" ]]; then
    HTTP_CODE="$(curl --silent --show-error --max-time 3 \
      --output "${RESPONSE_FILE}" --write-out '%{http_code}' \
      "http://127.0.0.1:${HOST_PORT}/v0/resource/plugins/reset-priority/status" 2>/dev/null || true)"
    if [[ "${HTTP_CODE}" == 200 ]] &&
      grep -Fq '<title>Reset Priority</title>' "${RESPONSE_FILE}" &&
      grep -Fq 'Dry-run configuration recommended' "${RESPONSE_FILE}"; then
      docker logs "${CONTAINER_NAME}" >"${LOG_FILE}" 2>&1 || true
      printf 'plugin load verified by HTTP 200 from %s\n' \
        "http://127.0.0.1:${HOST_PORT}/v0/resource/plugins/reset-priority/status"
      exit 0
    fi
  fi

  sleep 2
done

printf 'timed out after %s seconds waiting for the reset-priority status resource' "${TIMEOUT_SECONDS}" >&2
if [[ -n "${HOST_PORT}" ]]; then
  printf ' (last HTTP status: %s)' "${HTTP_CODE:-none}" >&2
fi
printf '\n' >&2
exit 1
