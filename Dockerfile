# syntax=docker/dockerfile:1
#
# Runtime image (no compilation inside the image): only installs common tools, plus the **prebuilt Linux single binary**.
# The binary is cross-compiled by the CI binaries job (pure Go, no QEMU), placed per target architecture at
# dist/<TARGETARCH>/artex in the build context. That way, in multi-arch builds arm64 only emulates the apt layer,
# no longer emulating Next/Go compilation -- much faster.
#
# When building the image manually locally, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a set of recon staples (add/remove as needed).
# Install Node 20.x from NodeSource: bookworm's stock apt nodejs is 18, while Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and CLI (globally), so no npx network download happens at runtime.
# @playwright/mcp: run the browser MCP directly with `npx @playwright/mcp` (already installed globally, no -y/@latest needed).
# @playwright/cli: provides playwright-cli; verify it runs with --help after installing.
# Then install playwright (provides browser management), and use --with-deps to preinstall chromium and its system dependencies,
# so MCP/CLI work on first launch inside the container with no browser download over the network.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Prebuilt binary for the matching architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Guardian startup script: relaunches or not based on the exit code after the process exits; the page's one-click update relies on it to complete the swap.
# It also forwards SIGTERM to artex -- docker stop only delivers the signal to PID 1,
# without forwarding, artex never receives it and cannot shut down gracefully, getting SIGKILLed after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
