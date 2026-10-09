# codex-image-api

[![CI](https://github.com/chendefine/codex-image-api/actions/workflows/docker.yml/badge.svg)](https://github.com/chendefine/codex-image-api/actions/workflows/docker.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/chendefine/codex-image-api)](go.mod)
[![Docker Hub](https://img.shields.io/badge/docker-chendefine%2Fcodex--image--api-blue?logo=docker)](https://hub.docker.com/r/chendefine/codex-image-api)

English | [简体中文](README.zh-CN.md)

An OpenAI-compatible image API backed by [Codex CLI](https://github.com/openai/codex). Each request runs `codex exec` with the `$imagegen` skill, and Codex's **built-in `image_gen` tool** generates the images using your ChatGPT sign-in. No `OPENAI_API_KEY` is needed.

## Contents

- [Features](#features)
- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Usage](#usage)
- [Configuration](#configuration)
- [API reference](#api-reference)
- [Operations](#operations)
- [Docker deployment notes](#docker-deployment-notes)
- [Development](#development)

## Features

- **OpenAI-compatible endpoints**: `POST /v1/images/generations` and `POST /v1/images/edits` (`multipart/form-data` or JSON). Point any OpenAI SDK at the server and it works.
- **Uses your ChatGPT sign-in**: the server never calls an image API itself. Images come from the Codex built-in `image_gen` tool.
- **Concurrency and queueing**: you set how many Codex processes run at once. Waiting requests sit in a bounded queue, and overflow gets `503` with `Retry-After`.
- **OpenAI error format**: every error, including 404/405, auth failures and panics, uses the OpenAI error envelope.
- **Easy to debug**: each request gets its own working directory with the prompt, input images, output images and Codex logs. Retention is configurable.
- **Graceful shutdown**: in-flight requests can finish, and anything still running after the grace period is killed and cleaned up.
- **Ready-made Docker image** for `linux/amd64` and `linux/arm64`. It runs as any UID/GID you choose.

## How it works

```text
client ──HTTP──▶ codex-image-api ──stdin prompt──▶ codex exec ($imagegen skill)
                       ▲                                   │ built-in image_gen
                       │                                   ▼
                       └──── b64_json ◀── collect ── $CODEX_HOME/generated_images/<thread_id>/
```

1. The server creates a working directory for the request and writes any input images to it.
2. It starts `codex exec` and passes the prompt on stdin. The prompt starts with `$imagegen` and contains no file paths. Input images are attached with `-i` and referred to as `[Image #1]…[Image #K]`.
3. When Codex exits, the server reads the session rollout to find the completed `image_gen` calls. It takes their images from `$CODEX_HOME`, copies them to the working directory and returns them as `b64_json`.
4. Finally, it deletes the files this session left in `$CODEX_HOME`.

## Quick start

### Docker Compose (recommended)

Requirements: Docker with Compose, and a host Codex installation signed in with ChatGPT (`codex login`). The container uses that installation's `auth.json`.

```bash
git clone https://github.com/chendefine/codex-image-api.git
cd codex-image-api
cp .env.example .env          # set at least APP_UID, APP_GID and CODEX_AUTH_FILE
mkdir -p codex-home workspace # create as APP_UID, otherwise Docker creates them owned by root
docker compose up -d
curl -s localhost:8080/healthz
```

By default `compose.yaml` pulls the prebuilt image `chendefine/codex-image-api:latest` from Docker Hub. The same image is published as `ghcr.io/chendefine/codex-image-api`. To update, run `docker compose pull && docker compose up -d`. To build from source instead, set `IMAGE` in `.env` to a local name such as `codex-image-api:latest`, then run `docker compose up -d --build`.

See [Docker deployment notes](#docker-deployment-notes) for UID/GID, networking and sandbox requirements.

### From source

Requirements: Go (the version in [go.mod](go.mod)), and `codex` signed in with the `image_generation` feature enabled. Check with `codex features list | grep image_generation`.

```bash
go build -o bin/codex-image-api ./cmd/codex-image-api
cp config.example.yaml config.yaml   # edit as needed; workspace.dir is required
./bin/codex-image-api -config config.yaml
```

## Usage

### curl

```bash
# Text-to-image
curl -s localhost:8080/v1/images/generations -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"a red apple on a white table","size":"1536x1024"}' \
  | jq -r '.data[0].b64_json' | base64 -d > apple.png

# Edit (multipart). Use image[] for several images; a single image can also be sent as "image".
curl -s localhost:8080/v1/images/edits -F 'prompt=make the apple green' -F 'image[]=@apple.png' \
  | jq -r '.data[0].b64_json' | base64 -d > green.png

# Edit (JSON). images[].image_url takes data URLs; http(s) URLs need limits.allow_image_urls.
curl -s localhost:8080/v1/images/edits -H 'Content-Type: application/json' \
  -d "{\"prompt\":\"add a hat\",\"images\":[{\"image_url\":\"data:image/png;base64,$(base64 -w0 apple.png)\"}]}" \
  | jq -r '.data[0].b64_json' | base64 -d > hat.png
```

If `server.api_keys` is set, add `-H 'Authorization: Bearer <key>'`.

### OpenAI SDK

Set `base_url` to `http://<host>:8080/v1`:

```python
import base64
from openai import OpenAI

# api_key must match server.api_keys; any value works when api_keys is empty
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-local")

result = client.images.generate(model="gpt-image-2", prompt="a watercolor fox", size="1024x1536")
with open("fox.png", "wb") as f:
    f.write(base64.b64decode(result.data[0].b64_json))
```

One request runs a full Codex agent session and can take minutes. Set a generous client timeout.

## Configuration

The server reads a YAML file given by `-config` (default `config.yaml`). See the annotated [config.example.yaml](config.example.yaml). The Docker image ships with [docker/config.yaml](docker/config.yaml).

| Key | Default | Description |
|---|---|---|
| `server.listen` | `:8080` | Listen address. |
| `server.api_keys` | `[]` | Accepted Bearer tokens. Leave empty to disable auth. `/healthz` never requires auth. |
| `server.shutdown_timeout` | `30s` | How long in-flight requests may keep running after SIGINT/SIGTERM. |
| `workspace.dir` | — (required) | Root directory for per-request working directories. |
| `workspace.keep` | `true` | Working directory retention: `false`/`0`, `N` days, or `true` (forever). See [Working directories](#working-directories). |
| `codex.bin` | `codex` | Path to the Codex executable. |
| `codex.model` | `""` | Passed as `codex exec -m`. Empty uses the model from Codex's `config.toml`. |
| `codex.profile` | `""` | Passed as `codex exec -p`. |
| `codex.sandbox` | `workspace-write` | `read-only`, `workspace-write` or `danger-full-access`. Use `danger-full-access` in Docker. |
| `codex.codex_home` | `$CODEX_HOME` or `~/.codex` | Where Codex stores sessions and generated images. |
| `codex.timeout` | `15m` | Time limit for one Codex run. The clock starts once the request gets an execution slot. |
| `codex.max_concurrency` | `2` | Maximum number of Codex processes running at once. |
| `codex.max_queue` | `16` | Maximum number of requests waiting for a slot. |
| `codex.queue_timeout` | `5m` | Maximum time a request waits for a slot. |
| `codex.extra_args` | `[]` | Extra arguments for `codex exec`, e.g. `["-c", "model_reasoning_effort=\"low\""]`. |
| `codex.env` | `{}` | Extra environment variables for Codex. |
| `limits.max_image_bytes` | `52428800` (50 MiB) | Maximum size of each input image. |
| `limits.max_images` | `5` | Maximum number of input images per edit request. Values above 5 are capped at 5 (the limit of the built-in `image_gen` tool). |
| `limits.max_n` | `10` | Maximum `n`. Values above 10 are capped at 10. |
| `limits.allow_image_urls` | `false` | Lets JSON edit requests use `http(s)` image URLs, which the server downloads. |

The defaults above apply when a key is missing. The example configs set `max_concurrency: 4` and `max_queue: 32`.

## API reference

### Endpoints

| Method | Path | Body |
|---|---|---|
| `POST` | `/v1/images/generations` | `application/json` |
| `POST` | `/v1/images/edits` | `multipart/form-data` (`image` / `image[]` files) or `application/json` (`images[].image_url`) |
| `GET` | `/healthz` | Returns `ok`. |

### Parameter mapping

The Codex built-in `image_gen` tool has only four parameters: `prompt`, `transparent_background`, `referenced_image_paths` and `num_last_images_to_include`. OpenAI parameters are handled as follows:

| OpenAI parameter | Handling |
|---|---|
| `prompt` | Required, at most 32,000 characters. Used verbatim as the `image_gen` prompt. |
| `n` | 1 to `limits.max_n`. The prompt asks Codex for `n` final images. Codex may call `image_gen` more often to replace drafts that miss the request. |
| `background` | `transparent` sets `transparent_background=true`. `opaque`, `auto` or omitted leaves it out. |
| `image` / `image[]` / `images[].image_url` | Saved to `input/` and attached with `codex exec -i`. The prompt refers to them only as `[Image #1]…[Image #K]`. Formats: PNG, JPEG, WebP, GIF. |
| `size` | Omitted or `auto`: no size constraint. `WxH`: converted to an aspect ratio and requested in the prompt (see below). An invalid format returns 400. |
| `quality`, `model`, `moderation`, `output_format`, `output_compression`, `input_fidelity`, `mask`, `style`, `response_format`, `stream`, `partial_images`, `user` | Accepted and ignored. The built-in tool doesn't support them. |
| `images[].file_id` | Not supported. Returns 400. |

**Size → aspect ratio.** The built-in tool can't set exact pixel dimensions, so `WxH` becomes an aspect ratio. If the ratio is within 5% of a common ratio (1:1, 5:4, 4:3, 3:2, 16:10, 16:9, 2:1, 21:9, or a portrait version of one), the closest common ratio is used. Otherwise the server uses the reduced ratio, or the closest ratio with both terms ≤ 25. Ratios beyond 3:1 or 1:3 are clamped to 3:1 or 1:3. The `size` in the response is the actual size of the generated image.

### Response

```json
{
  "created": 1760000000,
  "background": "opaque",
  "data": [{ "b64_json": "iVBORw0KGgo..." }],
  "output_format": "png",
  "size": "1536x1024"
}
```

`output_format` and `size` describe the first generated image. If Codex produces fewer than `n` images, the server returns the images it has and logs a warning.

### Errors

Errors use the OpenAI format `{"error":{"message","type","param","code"}}`:

| Situation | Status | `code` |
|---|---|---|
| Invalid parameter, malformed JSON, body too large, unsupported Content-Type | 400 | — |
| Unknown route / wrong method | 404 / 405 | — |
| Missing or wrong API key (with `WWW-Authenticate: Bearer`) | 401 | `invalid_api_key` |
| Codex failed or produced no image (the message includes Codex's last message) | 500 | `image_generation_failed` |
| Queue full or waited longer than `codex.queue_timeout` (with `Retry-After: 30`) | 503 | `server_busy` |
| Canceled because the server is shutting down (with `Retry-After: 5`) | 503 | `shutting_down` |
| Codex exceeded `codex.timeout` | 504 | `timeout` |
| Client disconnected | 499 | `canceled` |

## Operations

### Concurrency and queueing

At most `codex.max_concurrency` Codex processes run at once, and other requests wait in a queue. A request gets `503 server_busy` right away if `codex.max_queue` requests are already waiting, or after waiting `codex.queue_timeout` without getting a slot. `codex.timeout` starts counting once a request gets a slot.

### Request IDs and logs

Every response has an `X-Request-Id` header with 16 hex digits, e.g. `1a120aaf66a00b7c`. The first 11 digits are a Unix timestamp in milliseconds, the next 2 are a sequence number within that millisecond, and the last 3 are random. The timestamp and sequence always increase within a process, so IDs never collide and sort in time order. (After 256 IDs in one millisecond, the sequence rolls over into the next millisecond.) The same ID names the request's working directory and appears as `request_id` on every server and runner log line.

Clients can send `X-Client-Request-Id`. The server keeps only printable ASCII, cuts it to 128 bytes, and logs it as `client_request_id`.

### Graceful shutdown

On SIGINT/SIGTERM the server stops accepting connections and gives in-flight requests up to `server.shutdown_timeout`. Requests still running after that are canceled, which kills their Codex process groups and removes their temporary files from `$CODEX_HOME`. Then the server exits. A second Ctrl+C exits right away.

### Working directories

```text
<workspace.dir>/<YYYYMMDD>/<request-id>/   same as X-Request-Id
  prompt.txt          full prompt sent to Codex (on stdin)
  input/image_N.*     input images (edit requests only)
  output/image_N.*    collected output images
  codex.jsonl         `codex exec --json` event stream
  codex.stderr.log    Codex stderr
  last_message.txt    Codex's final message
```

`workspace.keep` controls how long working directories are kept:

- `false` or `0`: delete each directory as soon as its request finishes.
- `N` (≥ 1): keep for N days. At startup and every local midnight, delete `<YYYYMMDD>` directories dated before *today − N days*. For example, `1` keeps yesterday and today. Other files under `workspace.dir` are left alone. Dates use local time (set `TZ` in Docker).
- `true` or unset: keep forever.

Whatever the setting, a request rejected before Codex starts (validation error, `server_busy`) has its directory deleted.

### Image collection

The built-in `image_gen` tool saves each result to `$CODEX_HOME/generated_images/<thread_id>/<item_id>.png`. After Codex exits, the server:

1. Gets `thread_id` from the `thread.started` event in `codex.jsonl`.
2. Reads the session rollout `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread_id>.jsonl` and finds completed `image_gen.generation` items in order. For each one it loads `generated_images/<thread_id>/<item_id>.*`, or decodes the base64 `result` from the rollout if that file is missing. If there's no rollout, it reads the directory in modification-time order.
3. Keeps the images that Codex's final message lists as `FINAL_IMAGE: <file name>`, in that order and at most `n`. If none of the listed images match, it keeps the last `n`, because replaced drafts come first. It writes them to `output/image_N.<ext>`. Then it deletes `generated_images/<thread_id>/` and the rollout file, including after timeouts and failures. Codex's internal `state_5.sqlite` is left untouched.

> [!IMPORTANT]
> Don't pass `--ephemeral` to Codex (e.g. through `codex.extra_args`). Without a rollout on disk, collection can only fall back to modification-time order.

## Docker deployment notes

The [Dockerfile](Dockerfile) has two stages. `builder` compiles the server; it runs on the build platform and cross-compiles. `runner` installs the official Codex standalone package (`codex-package-<target>.tar.gz`). The full package is required because it includes `codex-code-mode-host` and other helpers that the built-in tools need; the bare `codex` binary can't start `image_gen`.

[compose.yaml](compose.yaml) mounts:

| Host (`.env`) | Container | Notes |
|---|---|---|
| `CODEX_HOME_DIR` (default `./codex-home`) | `/home/app/.codex` | Container `CODEX_HOME`: sessions, generated images, skills. Codex fills it on first run. |
| `CODEX_AUTH_FILE` | `/home/app/.codex/auth.json` | The host's ChatGPT login. Must be mounted read-write. |
| `WORKSPACE_DIR` (default `./workspace`) | `/data/workspace` | Per-request working directories. |
| `CONFIG_FILE` (default `./docker/config.yaml`) | `/etc/codex-image-api/config.yaml` | Server config (read-only). A custom file must keep `workspace.dir: /data/workspace` and `codex.sandbox: danger-full-access`. |

- **UID/GID.** Everything in the image belongs to root except a placeholder user `app` (1000:1000). The container starts as root. [docker/entrypoint.sh](docker/entrypoint.sh) changes `app` to `APP_UID`/`APP_GID`, gives that user the container's own `/home/app` and `/data`, then drops privileges with `setpriv`. So one prebuilt image works for any UID.
  - IDs must be plain decimal with no leading zeros (0–4294967294). `0` runs as root, which suits rootless Docker/Podman.
  - `APP_UID` must own `CODEX_AUTH_FILE` (check with `stat -c '%u %g' ~/.codex/auth.json`; the file is mode 0600). It must also own `CODEX_HOME_DIR` and `WORKSPACE_DIR`.
  - The entrypoint **never** chowns bind mounts. It exits with an error if their owner is wrong. After changing the UID, run `chown -R` on the host yourself.
- **auth.json.** Codex rewrites this file in place when it refreshes the token, and refresh tokens rotate. If the container can't write the new token back, the host's Codex gets logged out. The container doesn't read the host's `config.toml`, so set the model through `codex.model` or `codex.extra_args`.
- **Sandbox.** Under Docker's default seccomp/AppArmor profiles, Codex's bubblewrap sandbox can't create namespaces. Use `codex.sandbox: danger-full-access` and treat the container as the isolation boundary.
- **Network.** The container must reach `chatgpt.com`. If the host uses a proxy, set `HTTPS_PROXY`, using `host.docker.internal` for a proxy running on the host. If the host's path MTU is below 1500, set `NETWORK_MTU`; otherwise TLS handshakes hang and Codex reports `workspace routing discovery failed`.
- **Timezone.** The image includes `tzdata`. Set `TZ` to an IANA name such as `Asia/Shanghai` (default `UTC`). It affects log timestamps and the date directories for sessions and working directories.
- Other build and runtime options (Go version, `GOPROXY`, `CODEX_VERSION`, download mirror, bind address and port) are documented in [.env.example](.env.example).

### Prebuilt images

[.github/workflows/docker.yml](.github/workflows/docker.yml) runs `go vet` and `go test` on every push and pull request. Pushes that change only `*.md` files are skipped. For pushes, it then builds `linux/amd64` and `linux/arm64` images, pushes them to `ghcr.io/chendefine/codex-image-api`, and copies them unchanged to Docker Hub as `chendefine/codex-image-api`. The Docker Hub step needs the repository secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`, and is skipped without them.

| Tag | Source |
|---|---|
| `latest` | default branch (`main`) |
| `X.Y.Z`, `X.Y` | `vX.Y.Z` git tags |
| `<branch>` | any branch |
| `sha-<short>` | every build |

## Development

```bash
go vet ./...
go test ./... -count=1                                      # no real Codex needed
go test ./internal/server -run TestEditsMultipart -count=1  # one test
```

The tests replace `codex exec` with a shell script that writes the `thread.started` event, a rollout and `generated_images` the way Codex does. This covers the HTTP endpoints, auth, timeouts, queueing, cancellation and graceful shutdown end to end.

Project layout:

```text
cmd/codex-image-api/   entry point, signal handling, graceful shutdown
internal/server/       gin routes, middleware, request parsing, error mapping
internal/codex/        codex exec runner, prompt, aspect ratio, image collection, pruning
internal/config/       YAML config loading and defaults
internal/logctx/       request-scoped logger in context
docker/                image config and entrypoint
```

Contributor and AI-agent guidance is in [AGENTS.md](AGENTS.md).
