#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
site_dir="$repo_root/docs/site"

if [[ ! -x "$site_dir/node_modules/.bin/vite" ]]; then
  echo "Documentation preview dependencies are not installed." >&2
  echo "Run: npm ci --prefix docs/site" >&2
  exit 1
fi

case "${1:-build}" in
  build)
    npm --prefix "$site_dir" run build
    ;;
  serve)
    exec npm --prefix "$site_dir" run dev -- --host 127.0.0.1
    ;;
  *)
    echo "usage: $0 [build|serve]" >&2
    exit 2
    ;;
esac
