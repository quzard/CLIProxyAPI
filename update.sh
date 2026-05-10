#!/usr/bin/env bash
set -Eeuo pipefail

docker compose pull && docker compose up -d