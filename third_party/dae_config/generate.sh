#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANTLR_JAR="${ANTLR_JAR:-/Users/lihu/git/dae-config/.cache/antlr/antlr-4.12.0-complete.jar}"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

java -jar "$ANTLR_JAR" \
  -Dlanguage=Go \
  -package dae_config \
  -o "$OUT" \
  "$ROOT/third_party/dae_config/dae_config.g4"

GEN_DIR="$(find "$OUT" -type f -name dae_config_parser.go -exec dirname {} \; | head -1)"
if [[ -z "$GEN_DIR" ]]; then
  echo "generated parser not found" >&2
  exit 1
fi

find "$ROOT/third_party/dae_config" -maxdepth 1 -type f \
  \( -name 'dae_config*.go' -o -name 'dae_config*.interp' -o -name 'dae_config*.tokens' \) -delete
cp "$GEN_DIR"/dae_config* "$ROOT/third_party/dae_config"/
gofmt -w "$ROOT"/third_party/dae_config/*.go
