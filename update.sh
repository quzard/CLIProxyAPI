#!/usr/bin/env bash
set -Eeuo pipefail

IFS=$'\n\t'

APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
cd "$APP_DIR"

DEFAULT_IMAGE="ghcr.io/quzard/cli-proxy-api:main"
DEFAULT_PANEL_REPO="https://github.com/quzard/Cli-Proxy-API-Management-Center"

TMP_FILES=()
LOCK_ACQUIRED=0
COMPOSE=()
USAGE_EXPORTED=0
COMPOSE_CONTAINER_NAME=""

log() {
  printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

warn() {
  printf '[%s] Warning: %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >&2
}

die() {
  printf '[%s] Error: %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >&2
  exit 1
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM

  local file
  for file in "${TMP_FILES[@]}"; do
    if [ -n "$file" ]; then
      rm -f "$file" 2>/dev/null || true
    fi
  done

  if [ "$LOCK_ACQUIRED" = "1" ] && [ -n "${LOCK_DIR:-}" ]; then
    rm -rf "$LOCK_DIR" 2>/dev/null || true
  fi

  exit "$status"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

dotenv_value() {
  local key="$1"
  local file="${2:-.env}"

  [ -f "$file" ] || return 1

  awk -v key="$key" '
    $0 ~ "^[[:space:]]*(export[[:space:]]+)?" key "[[:space:]]*=" {
      sub(/^[[:space:]]*export[[:space:]]+/, "", $0)
      sub(/^[^=]*=/, "", $0)
      sub(/^[[:space:]]+/, "", $0)
      sub(/[[:space:]]+$/, "", $0)
      if (($0 ~ /^".*"$/) || ($0 ~ /^'\''.*'\''$/)) {
        $0 = substr($0, 2, length($0) - 2)
      }
      print
      found = 1
    }
    END { if (!found) exit 1 }
  ' "$file" | tail -n 1
}

env_or_dotenv() {
  local key="$1"
  local default_value="$2"
  local current_value="${!key:-}"

  if [ -z "$current_value" ]; then
    current_value="$(dotenv_value "$key" 2>/dev/null || true)"
  fi

  printf '%s' "${current_value:-$default_value}"
}

export COMPOSE_PROJECT_NAME
COMPOSE_PROJECT_NAME="$(env_or_dotenv COMPOSE_PROJECT_NAME cliproxyapiplus)"

export CLI_PROXY_IMAGE
CLI_PROXY_IMAGE="$(env_or_dotenv CLI_PROXY_IMAGE "$DEFAULT_IMAGE")"

export CLI_PROXY_STATIC_PATH
CLI_PROXY_STATIC_PATH="$(env_or_dotenv CLI_PROXY_STATIC_PATH "${CLI_PROXY_STATIC_PATH:-}")"

SERVICE_URL="${SERVICE_URL:-http://127.0.0.1:8317}"
HEALTH_URL="${HEALTH_URL:-$SERVICE_URL/healthz}"
MGMT_URL="${MGMT_URL:-$SERVICE_URL/v0/management}"
MGMT_KEY_FILE="${MGMT_KEY_FILE:-temp/stats/.management_key}"
MGMT_KEY="${MGMT_KEY:-}"
STATS_DIR="${STATS_DIR:-temp/stats}"
USAGE_BACKUP="${USAGE_BACKUP:-$STATS_DIR/usage.json}"
USAGE_SYNC="${USAGE_SYNC:-auto}"
IMPORT_EXISTING_USAGE_BACKUP="${IMPORT_EXISTING_USAGE_BACKUP:-false}"
PANEL_STATIC_DIR="$(env_or_dotenv PANEL_STATIC_DIR "${PANEL_STATIC_DIR:-${CLI_PROXY_STATIC_PATH:-${MANAGEMENT_STATIC_PATH:-temp/static}}}")"
PANEL_REPO="$(env_or_dotenv PANEL_REPO "${PANEL_REPO:-}")"
SYNC_MANAGEMENT_PANEL="$(env_or_dotenv SYNC_MANAGEMENT_PANEL "${SYNC_MANAGEMENT_PANEL:-true}")"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-90}"
WAIT_INTERVAL="${WAIT_INTERVAL:-2}"
LOG_TAIL="${LOG_TAIL:-160}"
LOCK_DIR="${LOCK_DIR:-$STATS_DIR/update.lock}"
REQUIRE_CONFIG="${REQUIRE_CONFIG:-true}"
PRUNE_IMAGES="${PRUNE_IMAGES:-false}"
AUTO_REMOVE_CONFLICTING_CONTAINER="${AUTO_REMOVE_CONFLICTING_CONTAINER:-true}"

case "$PANEL_STATIC_DIR" in
  */management.html)
    PANEL_STATIC_DIR="$(dirname "$PANEL_STATIC_DIR")"
    ;;
esac

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

is_truthy() {
  case "${1:-}" in
    1 | true | TRUE | yes | YES | y | Y | on | ON) return 0 ;;
    *) return 1 ;;
  esac
}

is_disabled() {
  case "${1:-}" in
    0 | false | FALSE | no | NO | n | N | off | OFF | never | NEVER) return 0 ;;
    *) return 1 ;;
  esac
}

ensure_positive_int() {
  local name="$1"
  local value="$2"
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || die "$name must be a positive integer, got: $value"
}

abs_path() {
  local path="$1"
  case "$path" in
    /*) printf '%s\n' "$path" ;;
    ~/*) printf '%s\n' "${HOME}${path#~}" ;;
    *) printf '%s\n' "$APP_DIR/${path#./}" ;;
  esac
}

looks_like_host_path() {
  case "$1" in
    /* | ./* | ../* | ~/*) return 0 ;;
    *) return 1 ;;
  esac
}

prepare_host_dir() {
  local path="$1"
  if looks_like_host_path "$path"; then
    mkdir -p "$(abs_path "$path")"
  fi
}

prepare_host_file_parent() {
  local path="$1"
  if looks_like_host_path "$path"; then
    mkdir -p "$(dirname "$(abs_path "$path")")"
  fi
}

acquire_lock() {
  mkdir -p "$(dirname "$(abs_path "$LOCK_DIR")")"
  LOCK_DIR="$(abs_path "$LOCK_DIR")"

  if mkdir "$LOCK_DIR" 2>/dev/null; then
    LOCK_ACQUIRED=1
    printf '%s\n' "$$" >"$LOCK_DIR/pid"
    return 0
  fi

  local old_pid=""
  if [ -f "$LOCK_DIR/pid" ]; then
    old_pid="$(head -n 1 "$LOCK_DIR/pid" 2>/dev/null || true)"
  fi

  if [ -n "$old_pid" ] && kill -0 "$old_pid" 2>/dev/null; then
    die "another update is already running (pid $old_pid, lock $LOCK_DIR)"
  fi

  warn "removing stale update lock: $LOCK_DIR"
  rm -rf "$LOCK_DIR"
  mkdir "$LOCK_DIR" || die "failed to acquire update lock: $LOCK_DIR"
  LOCK_ACQUIRED=1
  printf '%s\n' "$$" >"$LOCK_DIR/pid"
}

detect_compose() {
  require_command docker

  if docker compose version >/dev/null 2>&1; then
    COMPOSE=(docker compose --project-name "$COMPOSE_PROJECT_NAME")
    return 0
  fi

  if command -v docker-compose >/dev/null 2>&1; then
    COMPOSE=(docker-compose --project-name "$COMPOSE_PROJECT_NAME")
    return 0
  fi

  die "Docker Compose is not available; install docker compose plugin or docker-compose"
}

compose_logs() {
  if [ "${#COMPOSE[@]}" -eq 0 ]; then
    return 0
  fi

  "${COMPOSE[@]}" ps || true
  "${COMPOSE[@]}" logs --tail="$LOG_TAIL" || true
}

resolve_compose_container_name() {
  local name
  name="$("${COMPOSE[@]}" config 2>/dev/null | awk '
    /^[[:space:]]*container_name:[[:space:]]*/ {
      sub(/^[[:space:]]*container_name:[[:space:]]*/, "", $0)
      gsub(/^["'\'']|["'\'']$/, "", $0)
      print
      exit
    }
  ')"

  COMPOSE_CONTAINER_NAME="${name:-cli-proxy-api}"
}

remove_conflicting_container() {
  if [ -z "$COMPOSE_CONTAINER_NAME" ]; then
    resolve_compose_container_name
  fi

  [ -n "$COMPOSE_CONTAINER_NAME" ] || return 0

  local existing_id existing_project existing_service
  existing_id="$(docker ps -aq --filter "name=^/${COMPOSE_CONTAINER_NAME}$" | head -n 1)"
  [ -n "$existing_id" ] || return 0

  existing_project="$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$existing_id" 2>/dev/null || true)"
  existing_service="$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.service" }}' "$existing_id" 2>/dev/null || true)"

  if [ "$existing_project" = "$COMPOSE_PROJECT_NAME" ]; then
    return 0
  fi

  if ! is_truthy "$AUTO_REMOVE_CONFLICTING_CONTAINER"; then
    die "container name /${COMPOSE_CONTAINER_NAME} is already used by $existing_id (project=${existing_project:-none}, service=${existing_service:-none}); set AUTO_REMOVE_CONFLICTING_CONTAINER=true or remove it manually"
  fi

  warn "removing old conflicting container /${COMPOSE_CONTAINER_NAME}: $existing_id (project=${existing_project:-none}, service=${existing_service:-none})"
  docker rm -f "$existing_id" >/dev/null
}

sha256_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

read_management_key() {
  if [ -n "$MGMT_KEY" ]; then
    return 0
  fi

  if [ -f "$MGMT_KEY_FILE" ]; then
    MGMT_KEY="$(head -n 1 "$MGMT_KEY_FILE" | tr -d '\r\n')"
  fi
}

read_panel_repo_from_config() {
  local config_path="${CLI_PROXY_CONFIG_PATH:-./config.yaml}"
  config_path="$(abs_path "$config_path")"
  [ -f "$config_path" ] || return 1

  awk '
    /^[[:space:]]*remote-management[[:space:]]*:/ { in_remote = 1; next }
    in_remote && /^[^[:space:]#][^:]*:/ { in_remote = 0 }
    in_remote && /^[[:space:]]*panel-github-repository[[:space:]]*:/ {
      sub(/^[^:]*:/, "", $0)
      sub(/[[:space:]]+#.*$/, "", $0)
      sub(/^[[:space:]]+/, "", $0)
      sub(/[[:space:]]+$/, "", $0)
      if (($0 ~ /^".*"$/) || ($0 ~ /^'\''.*'\''$/)) {
        $0 = substr($0, 2, length($0) - 2)
      }
      print
      exit
    }
  ' "$config_path"
}

prepare_runtime_paths() {
  mkdir -p "$(abs_path "$STATS_DIR")" "$(abs_path "$PANEL_STATIC_DIR")"
  prepare_host_dir "${CLI_PROXY_AUTH_PATH:-./auths}"
  prepare_host_dir "${CLI_PROXY_LOG_PATH:-./logs}"
  prepare_host_dir "${CLI_PROXY_STATIC_PATH:-$PANEL_STATIC_DIR}"
  prepare_host_file_parent "${CLI_PROXY_CONFIG_PATH:-./config.yaml}"

  if is_truthy "$REQUIRE_CONFIG"; then
    local config_path="${CLI_PROXY_CONFIG_PATH:-./config.yaml}"
    if looks_like_host_path "$config_path" && [ ! -f "$(abs_path "$config_path")" ]; then
      die "config file not found: $(abs_path "$config_path")"
    fi
  fi

  MGMT_KEY_FILE="$(abs_path "$MGMT_KEY_FILE")"
  STATS_DIR="$(abs_path "$STATS_DIR")"
  USAGE_BACKUP="$(abs_path "$USAGE_BACKUP")"
  PANEL_STATIC_DIR="$(abs_path "$PANEL_STATIC_DIR")"
  export CLI_PROXY_STATIC_PATH="${CLI_PROXY_STATIC_PATH:-$PANEL_STATIC_DIR}"
}

http_request() {
  local output_file="$1"
  shift

  local code
  if ! code="$(curl -sS --max-time 60 -w '%{http_code}' -o "$output_file" "$@")"; then
    printf '000'
    return 1
  fi

  printf '%s' "$code"
}

export_usage() {
  if is_disabled "$USAGE_SYNC"; then
    return 0
  fi

  if [ -z "$MGMT_KEY" ]; then
    log "Management key is empty; skipping legacy usage export."
    return 0
  fi

  local tmp code
  tmp="$(mktemp "$STATS_DIR/usage.XXXXXX.json")"
  TMP_FILES+=("$tmp")

  code="$(http_request "$tmp" -H "X-Management-Key: $MGMT_KEY" "$MGMT_URL/usage/export" || true)"
  case "$code" in
    2??)
      mv "$tmp" "$USAGE_BACKUP"
      chmod 600 "$USAGE_BACKUP" || true
      USAGE_EXPORTED=1
      log "Legacy usage stats exported to $USAGE_BACKUP."
      ;;
    404 | 405)
      if [ "$USAGE_SYNC" = "always" ]; then
        warn "legacy usage export endpoint is unavailable (HTTP $code)."
      else
        log "Legacy usage export endpoint is unavailable; skipping."
      fi
      ;;
    000)
      warn "failed to call legacy usage export endpoint; service may be down."
      ;;
    401 | 403)
      warn "management key was rejected during legacy usage export (HTTP $code)."
      ;;
    *)
      warn "legacy usage export failed with HTTP $code."
      ;;
  esac
}

import_usage() {
  if is_disabled "$USAGE_SYNC"; then
    return 0
  fi

  if [ -z "$MGMT_KEY" ] || [ ! -s "$USAGE_BACKUP" ]; then
    return 0
  fi

  if [ "$USAGE_EXPORTED" != "1" ] && ! is_truthy "$IMPORT_EXISTING_USAGE_BACKUP"; then
    return 0
  fi

  local tmp code
  tmp="$(mktemp "$STATS_DIR/usage-import.XXXXXX.out")"
  TMP_FILES+=("$tmp")

  code="$(http_request "$tmp" -X POST \
    -H "X-Management-Key: $MGMT_KEY" \
    -H "Content-Type: application/json" \
    --data-binary "@$USAGE_BACKUP" \
    "$MGMT_URL/usage/import" || true)"

  case "$code" in
    2??)
      log "Legacy usage stats imported."
      ;;
    404 | 405)
      if [ "$USAGE_SYNC" = "always" ]; then
        warn "legacy usage import endpoint is unavailable (HTTP $code)."
      else
        log "Legacy usage import endpoint is unavailable; skipping."
      fi
      ;;
    000)
      warn "failed to call legacy usage import endpoint."
      ;;
    401 | 403)
      warn "management key was rejected during legacy usage import (HTTP $code)."
      ;;
    *)
      warn "legacy usage import failed with HTTP $code; backup remains at $USAGE_BACKUP."
      ;;
  esac
}

wait_service() {
  log "Waiting for service health check: $HEALTH_URL"

  local elapsed=0
  while [ "$elapsed" -lt "$WAIT_TIMEOUT" ]; do
    if curl -fsS --connect-timeout 3 --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; then
      log "Service is healthy."
      return 0
    fi

    sleep "$WAIT_INTERVAL"
    elapsed=$((elapsed + WAIT_INTERVAL))
  done

  warn "service failed health check after ${WAIT_TIMEOUT}s"
  compose_logs
  return 1
}

resolve_panel_release_api() {
  python3 - "$PANEL_REPO" "$DEFAULT_PANEL_REPO" <<'PY_PANEL_API'
import sys
from urllib.parse import urlparse

repo = (sys.argv[1] if len(sys.argv) > 1 else '').strip()
fallback = (sys.argv[2] if len(sys.argv) > 2 else '').strip()

if not repo:
    repo = fallback

parsed = urlparse(repo)

def fallback_api():
    parsed_fallback = urlparse(fallback)
    parts = [p for p in parsed_fallback.path.strip('/').split('/') if p]
    if len(parts) >= 2:
        print(f'https://api.github.com/repos/{parts[0]}/{parts[1]}/releases/latest')
    else:
        print('https://api.github.com/repos/quzard/Cli-Proxy-API-Management-Center/releases/latest')

if parsed.netloc.lower() == 'api.github.com':
    path = parsed.path.rstrip('/')
    if not path.lower().endswith('/releases/latest'):
        path += '/releases/latest'
    print(parsed._replace(path=path, query='', fragment='').geturl())
elif parsed.netloc.lower() == 'github.com':
    parts = [p for p in parsed.path.strip('/').split('/') if p]
    if len(parts) >= 2:
        repo_name = parts[1]
        if repo_name.endswith('.git'):
            repo_name = repo_name[:-4]
        print(f'https://api.github.com/repos/{parts[0]}/{repo_name}/releases/latest')
    else:
        fallback_api()
else:
    fallback_api()
PY_PANEL_API
}

sync_management_panel() {
  if ! is_truthy "$SYNC_MANAGEMENT_PANEL"; then
    return 0
  fi

  require_command python3

  if [ -z "$PANEL_REPO" ]; then
    PANEL_REPO="$(read_panel_repo_from_config || true)"
  fi
  PANEL_REPO="${PANEL_REPO:-$DEFAULT_PANEL_REPO}"

  local release_api release_json panel_meta download_url expected_hash release_tag
  local local_panel local_hash panel_tmp actual_hash

  release_api="$(resolve_panel_release_api)"
  release_json="$(mktemp "$STATS_DIR/panel-release.XXXXXX.json")"
  TMP_FILES+=("$release_json")

  log "Checking management panel release: $release_api"
  if ! curl -fsS --retry 3 --retry-delay 2 --connect-timeout 10 --max-time 30 \
    -H 'Accept: application/vnd.github+json' \
    -H 'User-Agent: cliproxy-update-script' \
    "$release_api" -o "$release_json"; then
    warn "failed to fetch management panel release metadata."
    return 0
  fi

  if ! panel_meta="$(python3 - "$release_json" <<'PY_PANEL_META'
import json
import sys

with open(sys.argv[1], encoding='utf-8') as fh:
    release = json.load(fh)

asset = next((a for a in release.get('assets', []) if a.get('name') == 'management.html'), None)
if not asset:
    raise SystemExit('management.html asset not found')

digest = (asset.get('digest') or '').strip().lower()
if ':' in digest:
    digest = digest.split(':', 1)[1]

print(asset.get('browser_download_url') or '')
print(digest)
print(release.get('tag_name') or '')
PY_PANEL_META
  )"; then
    warn "latest management panel release does not contain management.html."
    return 0
  fi

  download_url="$(printf '%s\n' "$panel_meta" | sed -n '1p')"
  expected_hash="$(printf '%s\n' "$panel_meta" | sed -n '2p')"
  release_tag="$(printf '%s\n' "$panel_meta" | sed -n '3p')"
  release_tag="${release_tag:-latest}"

  if [ -z "$download_url" ]; then
    warn "latest management panel release has no download URL."
    return 0
  fi

  mkdir -p "$PANEL_STATIC_DIR"
  local_panel="$PANEL_STATIC_DIR/management.html"
  if [ -n "$expected_hash" ] && [ -f "$local_panel" ]; then
    local_hash="$(sha256_file "$local_panel")"
    if [ "$local_hash" = "$expected_hash" ]; then
      log "Management panel already up to date: $release_tag ($local_hash)."
      return 0
    fi
  fi

  panel_tmp="$(mktemp "$PANEL_STATIC_DIR/management.XXXXXX.html")"
  TMP_FILES+=("$panel_tmp")

  if ! curl -LfsS --retry 3 --retry-delay 2 --connect-timeout 10 --max-time 120 \
    "$download_url" -o "$panel_tmp"; then
    warn "failed to download latest management panel."
    return 0
  fi

  actual_hash="$(sha256_file "$panel_tmp")"
  if [ -n "$expected_hash" ] && [ "$actual_hash" != "$expected_hash" ]; then
    warn "management panel digest mismatch: expected $expected_hash got $actual_hash."
    return 0
  fi

  chmod 644 "$panel_tmp"
  mv "$panel_tmp" "$local_panel"
  log "Management panel synced: $release_tag ($actual_hash)."
}

main() {
  ensure_positive_int WAIT_TIMEOUT "$WAIT_TIMEOUT"
  ensure_positive_int WAIT_INTERVAL "$WAIT_INTERVAL"
  ensure_positive_int LOG_TAIL "$LOG_TAIL"

  require_command curl
  require_command awk
  require_command sed
  detect_compose
  prepare_runtime_paths
  acquire_lock
  read_management_key

  log "Updating CLIProxyAPI in $APP_DIR"
  log "Compose project: $COMPOSE_PROJECT_NAME"
  log "Image: $CLI_PROXY_IMAGE"

  export_usage

  log "Validating Docker Compose configuration..."
  "${COMPOSE[@]}" config >/dev/null
  resolve_compose_container_name

  log "Pulling image..."
  "${COMPOSE[@]}" pull

  remove_conflicting_container

  log "Starting service..."
  "${COMPOSE[@]}" up -d --remove-orphans

  wait_service
  import_usage
  sync_management_panel

  if is_truthy "$PRUNE_IMAGES"; then
    log "Pruning dangling Docker images..."
    docker image prune -f >/dev/null
  fi

  log "Update completed."
}

main "$@"
