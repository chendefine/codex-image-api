# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27
ARG RUNTIME_IMAGE=debian:13-slim

# ---- builder: compile codex-image-api ----
FROM golang:${GO_VERSION}-alpine AS builder
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/codex-image-api ./cmd/codex-image-api

# ---- runner: codex-image-api + codex CLI ----
FROM ${RUNTIME_IMAGE} AS runner
# Codex release tag without the "rust-v" prefix, see https://github.com/openai/codex/releases
ARG CODEX_VERSION=0.162.0
ARG CODEX_DOWNLOAD_BASE=https://github.com/openai/codex/releases/download
# Must match the owner of the host directory mounted as CODEX_HOME.
ARG APP_UID=1000
ARG APP_GID=1000

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tini \
 && rm -rf /var/lib/apt/lists/*

# Install the full standalone package (bin/codex + bin/codex-code-mode-host +
# codex-path/rg + codex-resources/bwrap); the bare codex binary alone cannot run
# the built-in tools. The voice host is not needed by `codex exec`.
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) target=x86_64-unknown-linux-musl ;; \
      arm64) target=aarch64-unknown-linux-musl ;; \
      *) echo "unsupported architecture" >&2; exit 1 ;; \
    esac; \
    mkdir -p /opt/codex; \
    curl -fsSL "${CODEX_DOWNLOAD_BASE}/rust-v${CODEX_VERSION}/codex-package-${target}.tar.gz" | tar -xz --no-same-owner -C /opt/codex; \
    rm -rf /opt/codex/codex-resources/voice; \
    ln -s /opt/codex/bin/codex /usr/local/bin/codex; \
    codex --version

RUN set -eux; \
    getent group "${APP_GID}" >/dev/null || groupadd -g "${APP_GID}" app; \
    useradd -u "${APP_UID}" -g "${APP_GID}" -m -d /home/app -s /bin/sh app; \
    mkdir -p /home/app/.codex /data/workspace /etc/codex-image-api; \
    chown -R "${APP_UID}:${APP_GID}" /home/app /data

COPY --from=builder /out/codex-image-api /usr/local/bin/codex-image-api

USER ${APP_UID}:${APP_GID}
ENV HOME=/home/app CODEX_HOME=/home/app/.codex
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["tini", "--", "codex-image-api"]
# The config is bind-mounted here by compose (CONFIG_FILE in .env).
CMD ["-config", "/etc/codex-image-api/config.yaml"]
