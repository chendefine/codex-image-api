# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27
ARG RUNTIME_IMAGE=debian:13-slim

# ---- builder: compile codex-image-api ----
# Runs on the build platform and cross-compiles, so multi-arch builds need no emulation for Go.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
ARG GOPROXY=https://proxy.golang.org,direct
ARG TARGETOS TARGETARCH
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
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

# tzdata lets TZ (e.g. Asia/Shanghai) set the container's timezone; unset means UTC.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tini tzdata \
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

# Everything in the image belongs to root except the app user's state dirs.
# The "app" user is a placeholder: docker/entrypoint.sh remaps it to
# APP_UID/APP_GID at startup and then drops root, so one prebuilt image works
# for any host uid.
RUN set -eux; \
    groupadd -g 1000 app; \
    useradd -u 1000 -g app -d /home/app -M -s /bin/sh app; \
    install -d -o app -g app -m 0755 /home/app /home/app/.codex /data /data/workspace; \
    install -d -m 0755 /etc/codex-image-api

COPY --from=builder /out/codex-image-api /usr/local/bin/codex-image-api
COPY --chmod=0644 docker/config.yaml /etc/codex-image-api/config.yaml
COPY --chmod=0755 docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# No USER: the entrypoint starts as root only to remap the user, then execs the
# service via setpriv as app.
ENV HOME=/home/app CODEX_HOME=/home/app/.codex APP_UID=1000 APP_GID=1000
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["tini", "--", "docker-entrypoint.sh", "codex-image-api"]
# Default config baked in from docker/config.yaml; compose bind-mounts CONFIG_FILE over it.
CMD ["-config", "/etc/codex-image-api/config.yaml"]
