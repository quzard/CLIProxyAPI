#!/usr/bin/env bash
set -euo pipefail

APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
cd "$APP_DIR"

export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-cliproxyapiplus}"
export CLI_PROXY_IMAGE="${CLI_PROXY_IMAGE:-ghcr.io/quzard/cli-proxy-api-plus:main}"

MGMT_URL="${MGMT_URL:-http://127.0.0.1:8317/v0/management}"
MGMT_KEY_FILE="${MGMT_KEY_FILE:-temp/stats/.management_key}"
MGMT_KEY="${MGMT_KEY:-}"
STATS_DIR="${STATS_DIR:-temp/stats}"
USAGE_BACKUP="$STATS_DIR/usage.json"
PANEL_STATIC_DIR="${PANEL_STATIC_DIR:-temp/static}"
PANEL_REPO="${PANEL_REPO:-}"

mkdir -p "$STATS_DIR" "$PANEL_STATIC_DIR" logs auths

if [ -z "$MGMT_KEY" ] && [ -f "$MGMT_KEY_FILE" ]; then
  MGMT_KEY="$(head -n 1 "$MGMT_KEY_FILE")"
fi

if [ -z "$PANEL_REPO" ] && [ -f config.yaml ]; then
  PANEL_REPO="$(awk -F: '/^[[:space:]]*panel-github-repository[[:space:]]*:/ { sub(/^[[:space:]]+/, "", $2); sub(/[[:space:]]+$/, "", $2); gsub(/^'\''|'\''$/, "", $2); print $2; exit }' config.yaml)"
fi
PANEL_REPO="${PANEL_REPO:-https://github.com/quzard/Cli-Proxy-API-Management-Center}"

export_usage() {
  if [ -z "$MGMT_KEY" ]; then
    echo "Warning: management key is empty; skipping usage export."
    return 0
  fi

  if curl -fsS --max-time 30 -H "X-Management-Key: $MGMT_KEY" "$MGMT_URL/usage/export" -o "$USAGE_BACKUP.tmp"; then
    mv "$USAGE_BACKUP.tmp" "$USAGE_BACKUP"
    chmod 600 "$USAGE_BACKUP" || true
    echo "Usage stats exported."
  else
    echo "Warning: failed to export usage stats (service may be down), skipping."
    rm -f "$USAGE_BACKUP.tmp"
  fi
}

wait_service() {
  echo "Waiting for service to start..."
  for i in $(seq 1 30); do
    if curl -fsS http://127.0.0.1:8317/ >/dev/null 2>&1; then
      echo "Service is up."
      return 0
    fi
    sleep 2
  done

  echo "Error: service failed to start after 60s"
  docker compose --project-name "$COMPOSE_PROJECT_NAME" logs --tail=120 || true
  return 1
}

import_usage() {
  if [ -z "$MGMT_KEY" ] || [ ! -s "$USAGE_BACKUP" ]; then
    return 0
  fi

  if curl -fsS --max-time 60 -X POST \
    -H "X-Management-Key: $MGMT_KEY" \
    -H "Content-Type: application/json" \
    --data-binary "@$USAGE_BACKUP" \
    "$MGMT_URL/usage/import" >/dev/null; then
    echo "Usage stats imported."
  else
    echo "Warning: failed to import usage stats; persisted usage file remains on disk."
  fi
}

resolve_panel_release_api() {
  python3 - "$PANEL_REPO" <<'PY_PANEL_API'
import sys
from urllib.parse import urlparse

repo = (sys.argv[1] if len(sys.argv) > 1 else '').strip()

def default():
    print('https://api.github.com/repos/quzard/Cli-Proxy-API-Management-Center/releases/latest')

if not repo:
    default()
    raise SystemExit

parsed = urlparse(repo)
if parsed.netloc.lower() == 'api.github.com':
    path = parsed.path.rstrip('/')
    if not path.lower().endswith('/releases/latest'):
        path += '/releases/latest'
    print(parsed._replace(path=path).geturl())
elif parsed.netloc.lower() == 'github.com':
    parts = [p for p in parsed.path.strip('/').split('/') if p]
    if len(parts) >= 2:
        repo_name = parts[1].removesuffix('.git')
        print(f'https://api.github.com/repos/{parts[0]}/{repo_name}/releases/latest')
    else:
        default()
else:
    default()
PY_PANEL_API
}

sync_management_panel() {
  local release_api release_json panel_tmp expected_hash actual_hash download_url release_tag
  release_api="$(resolve_panel_release_api)"
  release_json="$(mktemp)"
  panel_tmp=""
  trap 'rm -f "$release_json"; if [ -n "$panel_tmp" ]; then rm -f "$panel_tmp"; fi' RETURN

  if ! curl -fsS --retry 3 --retry-delay 2 --max-time 30 \
    -H 'Accept: application/vnd.github+json' \
    -H 'User-Agent: cliproxy-update-script' \
    "$release_api" -o "$release_json"; then
    echo "Warning: failed to fetch management panel release metadata."
    return 0
  fi

  mapfile -t panel_meta < <(python3 - "$release_json" <<'PY_PANEL_META'
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
  )

  download_url="${panel_meta[0]:-}"
  expected_hash="${panel_meta[1]:-}"
  release_tag="${panel_meta[2]:-latest}"
  if [ -z "$download_url" ]; then
    echo "Warning: latest management panel release has no download URL."
    return 0
  fi

  mkdir -p "$PANEL_STATIC_DIR"
  panel_tmp="$(mktemp "$PANEL_STATIC_DIR/management.XXXXXX.html")"
  if ! curl -LfsS --retry 3 --retry-delay 2 --max-time 120 "$download_url" -o "$panel_tmp"; then
    echo "Warning: failed to download latest management panel."
    return 0
  fi

  actual_hash="$(sha256sum "$panel_tmp" | awk '{print $1}')"
  if [ -n "$expected_hash" ] && [ "$actual_hash" != "$expected_hash" ]; then
    echo "Warning: management panel digest mismatch: expected $expected_hash got $actual_hash."
    return 0
  fi

  chmod 644 "$panel_tmp"
  mv "$panel_tmp" "$PANEL_STATIC_DIR/management.html"
  panel_tmp=""
  echo "Management panel synced: $release_tag ($actual_hash)."
}

export_usage

docker compose --project-name "$COMPOSE_PROJECT_NAME" pull
docker compose --project-name "$COMPOSE_PROJECT_NAME" up -d --remove-orphans

wait_service
import_usage
sync_management_panel