#!/usr/bin/env bash
# ARTEX update script: 1) Docker update (pull new image and rebuild)  2) local build update (rebuild binary)
# Counterpart of install.sh: install handles first-time setup, update upgrades to a new version.
# No manual DB migration needed -- artex idempotently re-runs schema.sql on every startup (including ADD COLUMN/CREATE
# INDEX IF NOT EXISTS), so "restart means migrate". Data (pgdata volume, ./data, ./skills) is unaffected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: sync repo to latest code (compose/scripts/local-build sources all update through it) ───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "non- git working copy, skip git pull"; return; }
  [ "$(ask 'pull the latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or diverged branch) -- please resolve manually and retry; continuing with current code this time"
  fi
}

# ── 1) Docker update ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "not detected docker / docker compose, please use (it) first ./install.sh install and deploy"
  [ -f .env ] || die "not found .env, please run first ./install.sh complete the first deployment"

  # Optional: upgrade to a specific version tag (leave blank to keep ARTEX_TAG from .env, default latest)
  local tag; tag="$(ask 'Target image tag (Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG set to ${tag}"
  fi

  # Only touch artex: postgres is pinned to 16-alpine and needs no upgrade (pulling it just wastes bandwidth,
  # and a major-version change would risk incompatibility). artex declares depends_on postgres, so with a service name
  # up automatically starts pg if it is down, and leaves it untouched (no rebuild) if already running.
  info "Pulling new image (artex only)..."
  docker compose pull artex
  info "Rebuilding and starting (artex auto-migrates schema on restart)..."
  docker compose up -d artex
  ok "Update complete -> http://localhost:8787"
  info "View logs: docker compose logs -f artex"
  info "Clean up old images (optional): docker image prune -f"
}

# ── 2) Local build update ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "not detected Go (>=1.26): https://go.dev/dl/"
  [ -f config.json ] || warn "not found config.json----for first-time deployment use (install.sh) instead ./install.sh"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding frontend static assets..."
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Recompiling embedded single binary..."
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not detected: building backend **without embedded frontend** (run frontend separately via npm run dev)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete -> ./artex"
  warn "Please restart the running artex process to take effect (schema auto-migrates on restart)"
}

echo "=============================="
echo "  ARTEX update"
echo "  1) Docker update (pull new image and rebuild)"
echo "  2) Local update (recompile with go)"
echo "=============================="
case "$(ask 'Select' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid selection" ;;
esac
