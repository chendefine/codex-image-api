# codex-image-api

把 `codex exec` + imagegen skill 封装成 OpenAI 标准图片接口：

- `POST /v1/images/generations` — 文生图（JSON）
- `POST /v1/images/edits` — 图片编辑（`multipart/form-data` 或 JSON）
- `GET /healthz`

服务本身不调用任何图片 API。每次请求都会在 workspace 下创建专属工作目录，启动 `codex exec` 并在 prompt 开头以 `$imagegen` 指定 skill，由 codex 使用**内置 `image_gen` 工具**（ChatGPT 登录额度，无需 `OPENAI_API_KEY`）生成图片，服务再从 codex 的本地存储中收集图片以 `b64_json` 返回。prompt 中不包含任何输出路径，codex 也不需要做任何文件操作。

## 运行

```bash
go build -o bin/codex-image-api ./cmd/codex-image-api
cp config.example.yaml config.yaml   # 按需修改
./bin/codex-image-api -config config.yaml
```

前置条件：`codex` 已登录且 `image_generation` feature 启用（`codex features list | grep image_generation`）。

## 示例

```bash
# 文生图
curl -s localhost:8080/v1/images/generations -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"a red apple on a white table","n":1}' \
  | jq -r '.data[0].b64_json' | base64 -d > apple.png

# 图片编辑（多图用 image[]，单图也可用 image）
curl -s localhost:8080/v1/images/edits -F 'prompt=make the apple green' -F 'image[]=@apple.png' \
  | jq -r '.data[0].b64_json' | base64 -d > green.png

# JSON 形式的编辑（images[].image_url 支持 data URL；http(s) URL 需开启 limits.allow_image_urls）
curl -s localhost:8080/v1/images/edits -H 'Content-Type: application/json' \
  -d "{\"prompt\":\"add a hat\",\"images\":[{\"image_url\":\"data:image/png;base64,$(base64 -w0 apple.png)\"}]}"
```

OpenAI SDK 只需把 `base_url` 指向 `http://<host>:8080/v1`。

## 参数映射

codex 内置 `image_gen` 工具只有 `prompt`、`transparent_background`、`referenced_image_paths`、`num_last_images_to_include` 四个参数，因此：

| OpenAI 参数 | 处理 |
|---|---|
| `prompt` | 原文作为 image_gen 的 prompt（必填，≤32000 字符） |
| `n` | 在 prompt 中要求 codex 分别调用 n 次 image_gen |
| `background` | `transparent` → `transparent_background=true`；`opaque`/`auto` 或未传：prompt 中不提 `transparent_background` |
| `image` / `image[]` / `images[].image_url` | 保存到 `input/`，通过 `codex exec -i` 附加到消息中；prompt 只以 `[Image #1]…[Image #K]` 指代，不写路径 |
| `size` | 未传或 `auto`：prompt 中不提尺寸。`WxH`：计算长宽比，与常见比例（1:1、5:4、4:3、3:2、16:10、16:9、2:1、21:9 及其竖版）误差 ≤5% 时取最接近的常见比例，否则取约简比值（项超过 25 时取两项都 ≤25 的最接近比值），长宽比限制在 1:3 ~ 3:1，超出时强制改为 3:1 或 1:3（图片模型无法生成更极端的比例），在 prompt 中要求输出该长宽比。内置工具不能指定精确像素，响应中的 `size` 为实际尺寸。格式错误返回 400 |
| `quality` `model` `moderation` `output_format` `output_compression` `input_fidelity` `mask` `style` `response_format` `stream` `partial_images` `user` | 内置工具不支持，接收后忽略 |
| `file_id` | 不支持，返回 400 |

响应：`{created, data:[{b64_json}], background, output_format, size}`，其中 `output_format`、`size` 为生成图片的实际值。错误（包括 404/405、鉴权失败和服务端 panic）统一使用 OpenAI 格式 `{"error":{"message","type","param","code"}}`：

| 情况 | 状态码 | `code` |
|---|---|---|
| 参数错误 / 请求体过大 / JSON 非法 | 400 | — |
| 缺少或错误的 API key（带 `WWW-Authenticate: Bearer`） | 401 | `invalid_api_key` |
| codex 失败或未产出图片（message 带 codex 最后一条消息） | 500 | `image_generation_failed` |
| 排队已满或等待超过 `codex.queue_timeout`（带 `Retry-After`） | 503 | `server_busy` |
| 服务关闭时被取消（带 `Retry-After`） | 503 | `shutting_down` |
| codex 超过 `codex.timeout` | 504 | `timeout` |
| 客户端断开 | 499 | `canceled` |

## 并发与排队

同时运行的 codex 最多 `codex.max_concurrency` 个（默认 2）。其余请求排队等待：排队数超过 `codex.max_queue`（默认 16）时立即返回 503，排队超过 `codex.queue_timeout`（默认 5m）也返回 503；`codex.timeout` 从拿到执行槽位后开始计时。

## 请求 ID 与日志

每个响应都带 `X-Request-Id` 头，值为 16 位十六进制（如 `1a120aaf66a00b7c`）：前 11 位是毫秒级 Unix 时间戳，中间 2 位是同一毫秒内的序号，最后 3 位是随机数；时间戳加序号在同一进程内严格递增、不会重复（单毫秒超过 256 个时顺延到下一毫秒），因此工作目录和日志中的 ID 按字典序即按时间排序。它同时是该请求的工作目录名，以及服务端和 codex runner 所有日志行中的 `request_id` 字段。客户端可以发送 `X-Client-Request-Id`，其值（仅保留可打印 ASCII，最长 128 字节）会作为 `client_request_id` 记入同一请求的日志。

## 优雅关闭

收到 SIGINT/SIGTERM 后服务停止接受新连接，进行中的请求最多再运行 `server.shutdown_timeout`（默认 30s）；超时后取消剩余请求，对应的 codex 进程组被 SIGKILL 并照常清理 `$CODEX_HOME` 中的临时文件，然后进程退出。

## 工作目录

```
<workspace.dir>/<YYYYMMDD>/<request-id>/   与 X-Request-Id 相同
  prompt.txt          发给 codex 的完整 prompt（经 stdin 传入）
  input/image_N.*     编辑请求的输入图片（仅编辑请求创建 input/）
  output/image_N.png  服务收集到的最终图片
  codex.jsonl         codex exec --json 事件流
  codex.stderr.log
  last_message.txt    codex 最后一条消息
```

### 图片收集

内置 `image_gen` 每次调用都会把结果自动保存到 `$CODEX_HOME/generated_images/<thread_id>/<item_id>.png`。codex 进程结束后，服务：

1. 从 `codex.jsonl` 的 `thread.started` 事件取得 `thread_id`；
2. 读取会话记录 `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread_id>.jsonl` 中 `kind=image_gen.generation` 且 `status=completed` 的条目，按完成顺序从 `generated_images/<thread_id>/<item_id>.*` 读取图片（文件缺失时解码条目里的 base64 `result`）；没有会话记录时按修改时间读取该目录；
3. 写入工作目录 `output/image_N.<ext>`，并删除本次会话在 `$CODEX_HOME` 下留下的 `generated_images/<thread_id>/` 和会话记录 `rollout-*-<thread_id>.jsonl`（超时或失败时也会删除）。codex 内部数据库 `state_5.sqlite` 中的 thread 记录不做修改。

因此不要给 codex 加 `--ephemeral`（会话记录不会落盘，只能走按修改时间排序的兜底）。`workspace.keep: false` 时请求结束后删除工作目录；即使 `keep: true`，在 codex 启动前就被拒绝的请求（如参数校验失败、`server_busy`）也会删除其工作目录。

## 测试

```bash
go test ./... -count=1   # 使用假的 codex 脚本做 HTTP 集成测试和优雅关闭测试
```
