#!/usr/bin/env bash
# ARTEX native install for Kali Linux (also works on Debian-based systems).
# Does NOT download Go or build anything: provide a prebuilt ./artex at the repo root
# (from a GitHub release, or by running ./build.sh).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL_DIR=/opt/artex
DB_NAME=artex
DB_USER=artex

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Run as root (e.g. sudo $0)"

# --- OS check ---------------------------------------------------------------
[ -r /etc/os-release ] || die "/etc/os-release not found; cannot detect OS"
# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-}:${ID_LIKE:-}" in
  kali:*|debian:*|*:*debian*) ok "Detected ${PRETTY_NAME:-$ID}" ;;
  *) die "This script supports Kali/Debian only (found: ${PRETTY_NAME:-unknown})" ;;
esac
if [ "${ID:-}" != kali ]; then
  warn "Not Kali: some pentest packages (nuclei, httpx-toolkit, feroxbuster, ...) may be unavailable in your repos"
fi

# --- prebuilt binary --------------------------------------------------------
[ -f "$REPO_ROOT/artex" ] || die "Prebuilt binary not found at $REPO_ROOT/artex. Download it from the GitHub release (or run ./build.sh), place it in the repo root, and re-run."
[ -f "$REPO_ROOT/config.example.json" ] || die "config.example.json not found in $REPO_ROOT"
[ -f "$REPO_ROOT/deploy/artex.service" ] || die "deploy/artex.service not found in $REPO_ROOT"

# --- packages ---------------------------------------------------------------
export DEBIAN_FRONTEND=noninteractive
# jwt-tool is omitted: its Kali package name is uncertain; install it manually if needed.
PKGS=(
  ca-certificates curl wget git jq unzip ripgrep vim
  python3 python3-pip nodejs npm
  bind9-dnsutils iputils-ping netcat-openbsd whois
  nmap masscan nuclei httpx-toolkit subfinder ffuf feroxbuster gobuster
  sqlmap nikto whatweb wafw00f dirb hydra seclists exploitdb
  tcpdump socat proxychains4
  postgresql openssl sudo
)
info "Installing packages via apt (this can take a while)..."
apt-get update
if ! apt-get install -y --no-install-recommends "${PKGS[@]}"; then
  # One unknown package name aborts the whole transaction, so retry individually.
  warn "Bulk install failed; retrying package by package"
  for p in "${PKGS[@]}"; do
    apt-get install -y --no-install-recommends "$p" || warn "Skipped unavailable package: $p"
  done
fi
# Kali names ProjectDiscovery's httpx "httpx-toolkit"; agents and skills call it as httpx.
if ! command -v httpx >/dev/null 2>&1 && [ -x /usr/bin/httpx-toolkit ]; then
  ln -s /usr/bin/httpx-toolkit /usr/local/bin/httpx
fi
command -v psql >/dev/null 2>&1 || die "PostgreSQL (psql) did not install"
command -v openssl >/dev/null 2>&1 || die "openssl did not install"
command -v python3 >/dev/null 2>&1 || die "python3 did not install"

# --- system user and directories -------------------------------------------
if id artex >/dev/null 2>&1; then
  info "System user 'artex' already exists"
else
  useradd --system --home-dir "$INSTALL_DIR" --shell /usr/sbin/nologin artex
  ok "Created system user 'artex'"
fi
install -d -o artex -g artex -m 755 "$INSTALL_DIR"
install -d -o artex -g artex -m 700 "$INSTALL_DIR/keys" "$INSTALL_DIR/data"
install -o artex -g artex -m 755 "$REPO_ROOT/artex" "$INSTALL_DIR/artex"
if [ -d "$REPO_ROOT/skills" ]; then
  rm -rf "$INSTALL_DIR/skills"
  cp -r "$REPO_ROOT/skills" "$INSTALL_DIR/skills"
  chown -R artex:artex "$INSTALL_DIR/skills"
fi
ok "Installed binary and skills to $INSTALL_DIR"

# --- PostgreSQL + config.json ----------------------------------------------
systemctl enable --now postgresql >/dev/null 2>&1 || service postgresql start || warn "Could not start postgresql automatically"
CONFIG="$INSTALL_DIR/config.json"
if [ -f "$CONFIG" ]; then
  info "$CONFIG already exists; keeping it (and not touching the database role)"
else
  DB_PASS="$(openssl rand -hex 24)"
  if [ "$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='${DB_USER}'")" = 1 ]; then
    info "Role '${DB_USER}' exists; resetting its password for the new config"
    sudo -u postgres psql -v ON_ERROR_STOP=1 -c "ALTER ROLE ${DB_USER} WITH LOGIN PASSWORD '${DB_PASS}'"
  else
    sudo -u postgres psql -v ON_ERROR_STOP=1 -c "CREATE ROLE ${DB_USER} WITH LOGIN PASSWORD '${DB_PASS}'"
    ok "Created role '${DB_USER}'"
  fi
  if [ "$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'")" = 1 ]; then
    info "Database '${DB_NAME}' already exists"
  else
    sudo -u postgres createdb -O "$DB_USER" "$DB_NAME"
    ok "Created database '${DB_NAME}'"
  fi
  PG_PORT="$(sudo -u postgres psql -tAc 'SHOW port' 2>/dev/null | tr -d '[:space:]')"
  PG_PORT="${PG_PORT:-5432}"

  # Build config.json from config.example.json with python/json (no sed: special characters stay safe).
  ARTEX_DSN="postgres://${DB_USER}:${DB_PASS}@127.0.0.1:${PG_PORT}/${DB_NAME}?sslmode=disable" \
  ARTEX_SRC="$REPO_ROOT/config.example.json" \
  ARTEX_DST="$CONFIG" \
  ARTEX_SKILLS="$INSTALL_DIR/skills" \
  python3 -I -c '
import json, os
with open(os.environ["ARTEX_SRC"]) as f:
    cfg = json.load(f)
cfg = {k: v for k, v in cfg.items() if not k.startswith("_")}
cfg["database"] = {"dsn": os.environ["ARTEX_DSN"]}
cfg["skill_dir"] = os.environ["ARTEX_SKILLS"]
with open(os.environ["ARTEX_DST"], "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
'
  chown artex:artex "$CONFIG"
  chmod 600 "$CONFIG"
  ok "Wrote $CONFIG (mode 600)"
fi

# --- systemd ----------------------------------------------------------------
install -m 644 "$REPO_ROOT/deploy/artex.service" /etc/systemd/system/artex.service
systemctl daemon-reload
ok "Installed /etc/systemd/system/artex.service"

cat <<MSG

Next steps:
  1. Start ARTEX:        systemctl enable --now artex
  2. Check status/logs:  systemctl status artex ; journalctl -u artex -f
  3. Open the UI:        http://127.0.0.1:8787  (bound to localhost only)
     Remote access: use an SSH tunnel (ssh -L 8787:127.0.0.1:8787 user@host)
     or a reverse proxy with TLS. Do not expose the port directly.
  4. Configure your LLM provider in the UI (or via environment variables).
  5. Only test systems you are authorized to test.
MSG
