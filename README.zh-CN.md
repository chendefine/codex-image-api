# codex-image-api

[![CI](https://github.com/chendefine/codex-image-api/actions/workflows/docker.yml/badge.svg)](https://github.com/chendefine/codex-image-api/actions/workflows/docker.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/chendefine/codex-image-api)](go.mod)
[![Docker Hub](https://img.shields.io/badge/docker-chendefine%2Fcodex--image--api-blue?logo=docker)](https://hub.docker.com/r/chendefine/codex-image-api)

[English](README.md) | 简体中文

基于 [Codex CLI](https://github.com/openai/codex) 的 OpenAI 兼容图片接口。每个请求启动一次带 `$imagegen` skill 的 `codex exec`，由 Codex 的**内置 `image_gen` 工具**使用你的 ChatGPT 登录额度出图，无需 `OPENAI_API_KEY`。

## 目录

- [特性](#特性)
- [工作原理](#工作原理)
- [快速开始](#快速开始)
- [使用示例](#使用示例)
- [配置](#配置)
- [API 参考](#api-参考)
- [运维](#运维)
- [Docker 部署说明](#docker-部署说明)
- [开发](#开发)

## 特性

- **OpenAI 兼容接口**：`POST /v1/images/generations`、`POST /v1/images/edits`（`multipart/form-data` 或 JSON）。把任意 OpenAI SDK 指向本服务即可使用。
- **使用 ChatGPT 登录额度**：服务本身不调用任何图片 API，图片由 Codex 内置 `image_gen` 工具生成。
- **并发与排队**：可以设置同时运行的 Codex 进程数。等待的请求进入有界队列，超出时返回 `503` 并带 `Retry-After`。
- **OpenAI 错误格式**：所有错误（包括 404/405、鉴权失败和 panic）都使用 OpenAI 错误信封。
- **便于排查**：每个请求有独立的工作目录，保存 prompt、输入图片、输出图片和 Codex 日志，保留时长可配置。
- **优雅关闭**：进行中的请求可以先完成，宽限期过后仍未完成的会被终止并清理。
- **现成的 Docker 镜像**：支持 `linux/amd64` 和 `linux/arm64`，可以用任意 UID/GID 运行。

## 工作原理

```text
客户端 ──HTTP──▶ codex-image-api ──stdin prompt──▶ codex exec（$imagegen skill）
                       ▲                                   │ 内置 image_gen
                       │                                   ▼
                       └──── b64_json ◀── 收集 ── $CODEX_HOME/generated_images/<thread_id>/
```

1. 服务为请求创建工作目录，并把输入图片（如有）写入其中。
2. 启动 `codex exec`，prompt 经 stdin 传入。prompt 以 `$imagegen` 开头，不包含任何文件路径；输入图片通过 `-i` 附加，在 prompt 中以 `[Image #1]…[Image #K]` 指代。
3. Codex 退出后，服务从会话记录（rollout）中找出已完成的 `image_gen` 调用，到 `$CODEX_HOME` 中取出对应图片，复制到工作目录，再以 `b64_json` 返回。
4. 最后删除本次会话在 `$CODEX_HOME` 中留下的文件。

## 快速开始

### Docker Compose（推荐）

前置条件：安装了 Docker 和 Compose；宿主机上的 Codex 已用 ChatGPT 登录（`codex login`），容器会使用它的 `auth.json`。

```bash
git clone https://github.com/chendefine/codex-image-api.git
cd codex-image-api
cp .env.example .env          # 至少设置 APP_UID、APP_GID 和 CODEX_AUTH_FILE
mkdir -p codex-home workspace # 须以 APP_UID 身份创建，否则 Docker 会把它们创建为 root 属主
docker compose up -d
curl -s localhost:8080/healthz
```

`compose.yaml` 默认从 Docker Hub 拉取预构建镜像 `chendefine/codex-image-api:latest`，同一镜像也发布在 `ghcr.io/chendefine/codex-image-api`。更新时执行 `docker compose pull && docker compose up -d`。如果要从源码构建，在 `.env` 中把 `IMAGE` 改成本地名字（如 `codex-image-api:latest`），再执行 `docker compose up -d --build`。

UID/GID、网络和沙箱的要求见 [Docker 部署说明](#docker-部署说明)。

### 从源码运行

前置条件：Go（版本见 [go.mod](go.mod)）；`codex` 已登录并启用 `image_generation` feature，可以用 `codex features list | grep image_generation` 确认。

```bash
go build -o bin/codex-image-api ./cmd/codex-image-api
cp config.example.yaml config.yaml   # 按需修改；workspace.dir 必填
./bin/codex-image-api -config config.yaml
```

## 使用示例

### curl

```bash
# 文生图
curl -s localhost:8080/v1/images/generations -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"a red apple on a white table","size":"1536x1024"}' \
  | jq -r '.data[0].b64_json' | base64 -d > apple.png

# 图片编辑（multipart）。多张图用 image[]，单张图也可以用 image
curl -s localhost:8080/v1/images/edits -F 'prompt=make the apple green' -F 'image[]=@apple.png' \
  | jq -r '.data[0].b64_json' | base64 -d > green.png

# 图片编辑（JSON）。images[].image_url 支持 data URL；http(s) URL 需开启 limits.allow_image_urls
curl -s localhost:8080/v1/images/edits -H 'Content-Type: application/json' \
  -d "{\"prompt\":\"add a hat\",\"images\":[{\"image_url\":\"data:image/png;base64,$(base64 -w0 apple.png)\"}]}" \
  | jq -r '.data[0].b64_json' | base64 -d > hat.png
```

配置了 `server.api_keys` 时，需要加上 `-H 'Authorization: Bearer <key>'`。

### OpenAI SDK

把 `base_url` 设为 `http://<host>:8080/v1`：

```python
import base64
from openai import OpenAI

# api_key 须与 server.api_keys 中的某一项一致；api_keys 为空时可填任意值
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-local")

result = client.images.generate(model="gpt-image-2", prompt="a watercolor fox", size="1024x1536")
with open("fox.png", "wb") as f:
    f.write(base64.b64decode(result.data[0].b64_json))
```

一个请求要跑完一整个 Codex agent 会话，可能需要几分钟，客户端超时应设得足够长。

## 配置

服务读取 `-config` 指定的 YAML 文件（默认 `config.yaml`），带注释的完整示例见 [config.example.yaml](config.example.yaml)。Docker 镜像内置 [docker/config.yaml](docker/config.yaml)。

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `server.listen` | `:8080` | 监听地址。 |
| `server.api_keys` | `[]` | 允许的 Bearer token，为空时不鉴权。`/healthz` 始终不鉴权。 |
| `server.shutdown_timeout` | `30s` | 收到 SIGINT/SIGTERM 后，进行中的请求最多还能运行多久。 |
| `workspace.dir` | —（必填） | 请求工作目录的根目录。 |
| `workspace.keep` | `true` | 工作目录保留策略：`false`/`0`、`N` 天或 `true`（永久），见[工作目录](#工作目录)。 |
| `codex.bin` | `codex` | Codex 可执行文件路径。 |
| `codex.model` | `""` | 作为 `codex exec -m` 传入；为空时使用 Codex `config.toml` 中的模型。 |
| `codex.profile` | `""` | 作为 `codex exec -p` 传入。 |
| `codex.sandbox` | `workspace-write` | `read-only`、`workspace-write` 或 `danger-full-access`；Docker 中使用 `danger-full-access`。 |
| `codex.codex_home` | `$CODEX_HOME` 或 `~/.codex` | Codex 存放会话和生成图片的目录。 |
| `codex.timeout` | `15m` | 单次 Codex 运行的时限，从请求拿到执行槽位时开始计时。 |
| `codex.max_concurrency` | `2` | 同时运行的 Codex 进程数上限。 |
| `codex.max_queue` | `16` | 等待槽位的请求数上限。 |
| `codex.queue_timeout` | `5m` | 请求等待槽位的最长时间。 |
| `codex.extra_args` | `[]` | 追加给 `codex exec` 的参数，如 `["-c", "model_reasoning_effort=\"low\""]`。 |
| `codex.env` | `{}` | 传给 Codex 的额外环境变量。 |
| `limits.max_image_bytes` | `52428800`（50 MiB） | 单张输入图片的大小上限。 |
| `limits.max_images` | `5` | 每个编辑请求的输入图片数上限。超过 5 按 5 处理（内置 `image_gen` 工具的上限）。 |
| `limits.max_n` | `10` | `n` 的上限，超过 10 时按 10 处理。 |
| `limits.allow_image_urls` | `false` | 允许 JSON 编辑请求使用 `http(s)` 图片 URL（由服务端下载）。 |

上表是配置项缺省时的默认值。两份示例配置中写的是 `max_concurrency: 4`、`max_queue: 32`。

## API 参考

### 接口

| 方法 | 路径 | 请求体 |
|---|---|---|
| `POST` | `/v1/images/generations` | `application/json` |
| `POST` | `/v1/images/edits` | `multipart/form-data`（文件字段 `image` / `image[]`）或 `application/json`（`images[].image_url`） |
| `GET` | `/healthz` | 返回 `ok`。 |

### 参数映射

Codex 内置 `image_gen` 工具只有 `prompt`、`transparent_background`、`referenced_image_paths`、`num_last_images_to_include` 四个参数，OpenAI 参数的处理方式如下：

| OpenAI 参数 | 处理方式 |
|---|---|
| `prompt` | 必填，最多 32000 字符，原样作为 `image_gen` 的 prompt。 |
| `n` | 取值 1 ~ `limits.max_n`。在 prompt 中要求 Codex 交付 `n` 张最终图片；结果不符合要求时，Codex 可以多次调用 `image_gen` 替换草稿。 |
| `background` | `transparent` 时设置 `transparent_background=true`；`opaque`、`auto` 或不传时不设置。 |
| `image` / `image[]` / `images[].image_url` | 保存到 `input/`，通过 `codex exec -i` 附加；prompt 中只以 `[Image #1]…[Image #K]` 指代。支持 PNG、JPEG、WebP、GIF。 |
| `size` | 不传或 `auto`：不限制尺寸。`WxH`：换算成长宽比写进 prompt（见下文）。格式错误返回 400。 |
| `quality`、`model`、`moderation`、`output_format`、`output_compression`、`input_fidelity`、`mask`、`style`、`response_format`、`stream`、`partial_images`、`user` | 内置工具不支持，接收后忽略。 |
| `images[].file_id` | 不支持，返回 400。 |

**尺寸 → 长宽比**：内置工具不能指定精确像素，所以 `WxH` 会换算成长宽比。与常见比例（1:1、5:4、4:3、3:2、16:10、16:9、2:1、21:9 及其竖版）误差在 5% 以内时，取最接近的常见比例；否则取约简比值，约简后某项超过 25 时，取两项都不超过 25 的最接近比值。超出 3:1 或 1:3 时强制改为 3:1 或 1:3。响应中的 `size` 是生成图片的实际尺寸。

### 响应

```json
{
  "created": 1760000000,
  "background": "opaque",
  "data": [{ "b64_json": "iVBORw0KGgo..." }],
  "output_format": "png",
  "size": "1536x1024"
}
```

`output_format` 和 `size` 取自第一张生成的图片。Codex 生成的图片少于 `n` 张时，返回已有的图片，并在日志中记录警告。

### 错误

错误统一使用 OpenAI 格式 `{"error":{"message","type","param","code"}}`：

| 情况 | 状态码 | `code` |
|---|---|---|
| 参数错误、JSON 非法、请求体过大、不支持的 Content-Type | 400 | — |
| 未知路由 / 方法不允许 | 404 / 405 | — |
| 缺少或错误的 API key（带 `WWW-Authenticate: Bearer`） | 401 | `invalid_api_key` |
| Codex 失败或未产出图片（message 中带 Codex 最后一条消息） | 500 | `image_generation_failed` |
| 排队已满，或等待超过 `codex.queue_timeout`（带 `Retry-After: 30`） | 503 | `server_busy` |
| 服务关闭时被取消（带 `Retry-After: 5`） | 503 | `shutting_down` |
| Codex 运行超过 `codex.timeout` | 504 | `timeout` |
| 客户端断开 | 499 | `canceled` |

## 运维

### 并发与排队

同时运行的 Codex 进程最多 `codex.max_concurrency` 个，其余请求排队等待。已有 `codex.max_queue` 个请求在等待时，新请求立即返回 `503 server_busy`；等待超过 `codex.queue_timeout` 仍未拿到槽位，也返回 `503 server_busy`。`codex.timeout` 从拿到槽位后开始计时。

### 请求 ID 与日志

每个响应都带 `X-Request-Id` 头，值为 16 位十六进制，如 `1a120aaf66a00b7c`：前 11 位是毫秒级 Unix 时间戳，中间 2 位是同一毫秒内的序号，最后 3 位是随机数。时间戳加序号在进程内严格递增，因此 ID 不会重复，并且按字典序排列即按时间排列（同一毫秒超过 256 个时顺延到下一毫秒）。同一个 ID 也是该请求的工作目录名，并作为 `request_id` 字段出现在服务端和 runner 的所有日志行中。

客户端可以发送 `X-Client-Request-Id`，服务端只保留其中的可打印 ASCII 字符，截断到 128 字节，作为 `client_request_id` 记入日志。

### 优雅关闭

收到 SIGINT/SIGTERM 后，服务停止接受新连接，进行中的请求最多再运行 `server.shutdown_timeout`。到期后仍在运行的请求会被取消：对应的 Codex 进程组被杀掉，`$CODEX_HOME` 中的临时文件照常清理，然后进程退出。第二次 Ctrl+C 会立即退出。

### 工作目录

```text
<workspace.dir>/<YYYYMMDD>/<request-id>/   与 X-Request-Id 相同
  prompt.txt          发给 Codex 的完整 prompt（经 stdin 传入）
  input/image_N.*     输入图片（仅编辑请求）
  output/image_N.*    收集到的输出图片
  codex.jsonl         `codex exec --json` 事件流
  codex.stderr.log    Codex 的 stderr
  last_message.txt    Codex 的最后一条消息
```

`workspace.keep` 控制工作目录保留多久：

- `false` 或 `0`：请求结束后立即删除其工作目录。
- `N`（≥ 1）：保留 N 天。服务启动时以及之后每天本地时间 0 点，删除日期早于"今天 − N 天"的 `<YYYYMMDD>` 目录。例如 `1` 保留昨天和今天。`workspace.dir` 下的其他文件不受影响。日期按本地时间计算（Docker 中由 `TZ` 决定）。
- `true` 或不设置：永久保留。

无论哪种设置，在 Codex 启动前就被拒绝的请求（参数校验失败、`server_busy` 等）都会删除其工作目录。

### 图片收集

内置 `image_gen` 会把每次的结果保存到 `$CODEX_HOME/generated_images/<thread_id>/<item_id>.png`。Codex 退出后，服务：

1. 从 `codex.jsonl` 的 `thread.started` 事件取得 `thread_id`。
2. 读取会话记录 `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread_id>.jsonl`，按顺序找出已完成的 `image_gen.generation` 条目。每个条目从 `generated_images/<thread_id>/<item_id>.*` 读取图片，文件缺失时解码条目中的 base64 `result`。没有会话记录时，按修改时间顺序读取该目录。
3. 保留 Codex 最后一条消息中以 `FINAL_IMAGE: <文件名>` 列出的图片（按列出顺序，最多 `n` 张）；一张都对不上时保留最后 `n` 张（被替换的草稿在前）。把它们写入 `output/image_N.<ext>`，然后删除 `generated_images/<thread_id>/` 和会话记录文件（超时或失败时也会删除）。Codex 内部数据库 `state_5.sqlite` 不做修改。

> [!IMPORTANT]
> 不要给 Codex 加 `--ephemeral`（例如通过 `codex.extra_args`）。会话记录不落盘时，图片收集只能退回到按修改时间排序。

## Docker 部署说明

[Dockerfile](Dockerfile) 分两个阶段。`builder` 编译服务，在构建平台上运行并交叉编译。`runner` 安装 Codex 官方 standalone 包（`codex-package-<target>.tar.gz`）。必须安装完整包，因为其中包含内置工具依赖的 `codex-code-mode-host` 等组件，只装裸 `codex` 二进制无法启动 `image_gen`。

[compose.yaml](compose.yaml) 的挂载：

| 宿主机（`.env`） | 容器内 | 说明 |
|---|---|---|
| `CODEX_HOME_DIR`（默认 `./codex-home`） | `/home/app/.codex` | 容器内的 `CODEX_HOME`：会话、生成的图片、skill 等，Codex 首次运行时自动生成。 |
| `CODEX_AUTH_FILE` | `/home/app/.codex/auth.json` | 宿主机的 ChatGPT 登录凭据，必须可读写挂载。 |
| `WORKSPACE_DIR`（默认 `./workspace`） | `/data/workspace` | 请求工作目录。 |
| `CONFIG_FILE`（默认 `./docker/config.yaml`） | `/etc/codex-image-api/config.yaml` | 服务配置（只读）。自定义配置须保留 `workspace.dir: /data/workspace` 和 `codex.sandbox: danger-full-access`。 |

- **UID/GID**：镜像内的文件都归 root，只预留一个占位用户 `app`（1000:1000）。容器以 root 启动，[docker/entrypoint.sh](docker/entrypoint.sh) 把 `app` 改成 `APP_UID`/`APP_GID`，把容器自身的 `/home/app`、`/data` 交给该用户，再用 `setpriv` 降权运行。因此同一个预构建镜像适用于任意 UID。
  - ID 须为不带前导零的十进制数（0 ~ 4294967294）。`0` 表示以 root 运行，适用于 rootless Docker/Podman。
  - `APP_UID` 必须是 `CODEX_AUTH_FILE` 的属主（用 `stat -c '%u %g' ~/.codex/auth.json` 查看，文件权限为 0600），也必须是 `CODEX_HOME_DIR` 和 `WORKSPACE_DIR` 的属主。
  - 入口脚本**不会** chown 挂载的目录，属主不符时直接报错退出。修改 UID 后需要自己在宿主机上 `chown -R`。
- **auth.json**：Codex 刷新 token 时会原地改写这个文件，而刷新令牌会轮换。如果容器写不回新令牌，宿主机上的 Codex 就会掉线。容器不读宿主机的 `config.toml`，模型等设置请用 `codex.model` 或 `codex.extra_args` 指定。
- **沙箱**：在 Docker 默认的 seccomp/AppArmor 配置下，Codex 的 bubblewrap 沙箱无法创建命名空间。请使用 `codex.sandbox: danger-full-access`，以容器作为隔离边界。
- **网络**：容器需要能访问 `chatgpt.com`。宿主机经代理上网时设置 `HTTPS_PROXY`，代理在宿主机上时用 `host.docker.internal` 作为地址。宿主机路径 MTU 小于 1500 时设置 `NETWORK_MTU`，否则 TLS 握手会卡住，Codex 报 `workspace routing discovery failed`。
- **时区**：镜像内置 `tzdata`。用 `TZ` 指定 IANA 时区名，如 `Asia/Shanghai`（默认 `UTC`），它影响日志时间戳以及会话和工作目录的日期目录。
- 其余构建和运行参数（Go 版本、`GOPROXY`、`CODEX_VERSION`、下载镜像、监听地址和端口等）见 [.env.example](.env.example)。

### 预构建镜像

[.github/workflows/docker.yml](.github/workflows/docker.yml) 在每次 push 和 PR 时运行 `go vet` 和 `go test`，只改动 `*.md` 的 push 会被跳过。对于 push，接着构建 `linux/amd64`、`linux/arm64` 镜像推送到 `ghcr.io/chendefine/codex-image-api`，再原样复制到 Docker Hub 的 `chendefine/codex-image-api`。推送 Docker Hub 需要在仓库中配置 secrets `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`，未配置时跳过这一步。

| 标签 | 来源 |
|---|---|
| `latest` | 默认分支（`main`） |
| `X.Y.Z`、`X.Y` | `vX.Y.Z` git tag |
| `<branch>` | 任意分支 |
| `sha-<短 hash>` | 每次构建 |

## 开发

```bash
go vet ./...
go test ./... -count=1                                      # 不需要真实的 Codex
go test ./internal/server -run TestEditsMultipart -count=1  # 运行单个测试
```

测试用一个 shell 脚本代替 `codex exec`，它会像 Codex 一样写出 `thread.started` 事件、会话记录和 `generated_images`。HTTP 接口、鉴权、超时、排队、取消和优雅关闭都有端到端测试。

项目结构：

```text
cmd/codex-image-api/   程序入口、信号处理、优雅关闭
internal/server/       gin 路由、中间件、请求解析、错误映射
internal/codex/        codex exec 调用、prompt、长宽比、图片收集、目录清理
internal/config/       YAML 配置加载与默认值
internal/logctx/       在 context 中携带请求级 logger
docker/                镜像内置配置和入口脚本
```

贡献者和 AI agent 的开发指南见 [AGENTS.md](AGENTS.md)。
