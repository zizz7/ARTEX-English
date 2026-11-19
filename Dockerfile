# syntax=docker/dockerfile:1
#
# Runtime image (no compilation inside the image): installs only common tools and adds a **prebuilt Linux single binary**.
# The binary is cross-compiled by the CI binaries job (pure Go, no QEMU) and placed per target architecture at
# dist/<TARGETARCH>/artex in the build context. This way, in multi-arch builds arm64 only needs to emulate the apt layer,
# not the Next/Go compilation, which is much faster.
#
# When building the image manually on your machine, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a set of standard recon utilities (add or remove as needed).
# Node 20.x is installed from NodeSource: the apt nodejs shipped with bookworm is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and CLI (globally), so nothing is downloaded via npx at runtime.
# @playwright/mcp: the browser MCP runs directly via `npx @playwright/mcp` (already installed globally, no -y/@latest needed).
# @playwright/cli: provides playwright-cli; --help is run after install to verify it is executable.
# Also install playwright (provides browser management), then use --with-deps to preinstall chromium and its system dependencies,
# so the MCP/CLI in the container work on first launch without downloading a browser over the network.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Prebuilt binary for the matching architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
RUN chmod +x /app/artex
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/artex"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
