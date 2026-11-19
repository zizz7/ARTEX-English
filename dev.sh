#!/usr/bin/env bash
# Dev mode: run the backend (:8787) + traffic proxy (:8788) together with the frontend next dev (:5173).
# The frontend proxies /api to the backend; Ctrl-C exits all of them.
#
# For the single-binary (embedded frontend) setup, see the "Single binary" section of the README; this script is not used for it.
set -euo pipefail
cd "$(dirname "$0")"

# On exit, terminate all child processes in this process group (backend + frontend).
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Backend (plain go run, frontend not embedded); the number of concurrent work agents is configured in "System Settings".
go run ./cmd/artex -addr :8787 -proxy :8788 &

# Frontend hot reload (Vite/Next dev server, /api proxied to :8787).
( cd web && npm run dev ) &

echo "[dev] backend :8787 / proxy :8788 / frontend http://localhost:5173  (Ctrl-C to exit)"
wait
