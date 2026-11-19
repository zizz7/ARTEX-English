#!/usr/bin/env bash
# ARTEX update script: (1) Docker update (pull new image and recreate)  (2) local build update (rebuild binary)
# Counterpart to install.sh: install handles the first deployment, update handles upgrading to a new version.
# No manual DB migration is needed: artex idempotently re-runs schema.sql on every start (including ADD COLUMN/CREATE
# INDEX IF NOT EXISTS), so "restart means migrate". Data (pgdata volume, ./data, ./skills) is unaffected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: sync the repo to the latest code (compose/scripts/local-build sources all rely on this) ───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or diverged branch) - please resolve manually and retry; continuing with the current code"
  fi
}

# ── (1) Docker update ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not found; please deploy with ./install.sh first"
  [ -f .env ] || die ".env not found; please run ./install.sh to complete the first deployment"

  # Only touch artex: postgres is pinned to 16-alpine and does not need upgrading. artex declares
  # depends_on postgres, so running up with the service name starts pg automatically if it is not
  # running; if it is already running it is kept as-is, not recreated.
  local files=(-f docker-compose.yml)
  [ "${ARTEX_FLAVOR:-slim}" = kali ] && files+=(-f docker-compose.kali.yml)
  info "Rebuilding the ARTEX image from the current code…"
  ./scripts/build-image.sh "${ARTEX_FLAVOR:-slim}"
  info "Recreating and starting (artex migrates the schema automatically on restart)…"
  docker compose "${files[@]}" up -d artex
  ok "Update complete → http://localhost:8787"
  info "View logs: docker compose logs -f artex"
  info "Clean up old images (optional): docker image prune -f"
}

# ── (2) Local build update ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go (>=1.26) not found: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found - if this is a first deployment, use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding static frontend assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Recompiling the single embedded binary…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: building the backend **without the embedded frontend** (run the frontend separately with npm run dev)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete → ./artex"
  warn "Restart the running artex process for the update to take effect (the schema is migrated automatically on restart)"
}

echo "=============================="
echo "  ARTEX Update"
echo "  1) Docker update (pull new image and recreate)"
echo "  2) Local update (rebuild with go)"
echo "=============================="
case "$(ask 'Choose' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid choice" ;;
esac
