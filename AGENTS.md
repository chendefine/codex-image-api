# AGENTS.md

## 项目概述

把 `codex exec` + `$imagegen` skill 封装成 OpenAI 兼容的图片接口：

- `POST /v1/images/generations`：文生图（JSON）
- `POST /v1/images/edits`：图片编辑（`multipart/form-data` 或 JSON）
- `GET /healthz`：返回 `ok`，不鉴权

服务本身不调用任何图片 API。每个请求启动一次 `codex exec`，codex 用**内置 `image_gen` 工具**（ChatGPT 登录额度）出图，服务再从 `$CODEX_HOME` 的本地存储收集图片，以 `b64_json` 返回。

面向用户的说明有英文 [README.md](README.md) 和中文 [README.zh-CN.md](README.zh-CN.md) 两份，内容保持一致。修改参数映射、错误码、配置项、工作目录布局、图片收集流程或 Docker 行为时，**两份 README 都要同步更新**；新增配置项还要同步更新 `config.example.yaml` 和 `docker/config.yaml`。

## 常用命令

```bash
go build -o bin/codex-image-api ./cmd/codex-image-api
cp config.example.yaml config.yaml && ./bin/codex-image-api -config config.yaml
go vet ./...
go test ./... -count=1                                       # 全部测试，不需要真实 codex
go test ./internal/server -run TestEditsMultipart -count=1   # 单个测试
docker compose up -d --build                                 # 本地构建镜像（需在 .env 中把 IMAGE 改成本地名字）
```

本地运行前需要 `codex` 已登录并启用 `image_generation` feature。CI 会对每次 push 和 PR 执行 `go vet` 与 `go test`。

## 目录结构

```
cmd/codex-image-api/   main.go：加载配置、监听、信号处理、优雅关闭（run 函数）
internal/server/       gin 路由、中间件、请求解析、错误映射
internal/codex/        codex exec 调用、prompt 构造、长宽比换算、图片收集、工作目录清理
internal/config/       YAML 配置加载、默认值、校验
internal/logctx/       在 context 中携带请求级 *slog.Logger
docker/                镜像内置配置 config.yaml、入口脚本 entrypoint.sh
.github/workflows/     测试 → 构建多架构镜像推送 GHCR → 复制到 Docker Hub
```

## 架构

调用方向为 `main → server → codex`，三者都依赖 `config`，日志通过 `logctx` 传递。

### 请求链路

1. 全局中间件（`middleware.go`）依次为 `requestID` → `accessLog` → `recovery`；`/v1` 组额外挂 `auth`。
2. `requestID` 生成 16 位十六进制 ID（毫秒时间戳 + 序号 + 随机数，进程内严格递增），写入 `X-Request-Id` 响应头，并把带 `request_id`（以及可选的 `client_request_id`）字段的 logger 放进 request context。这个 ID 也用作工作目录名，用 `c.GetString(requestIDKey)` 取得。
3. `auth` 只在 `server.api_keys` 非空时生效，用 SHA-256 摘要做常量时间比较来校验 Bearer token。
4. 业务 handler 的签名是 `func(*gin.Context) (T, error)`，经泛型 `handle(s, fn)` 适配：成功时写 JSON，失败时交给 `writeError`。
5. handler 内手动解析请求体（`request.go`）：`decodeJSON` 带大小限制并拒绝尾随数据；`buildJob` 校验 `prompt`、`n`、`background`、`size`。edits 按 Content-Type 分 multipart 和 JSON 两条路径（`edits.go`），统一成 `editInput` 后由 `saveInputs` 写入 `input/`。
6. 依次调用 `runner.NewWorkdir(id)` → `defer runner.Cleanup` → `s.execute` → `runner.Run`。

### 错误

- `errors.go` 的 `writeError` 是写错误响应的统一入口，handler、auth、NoRoute/NoMethod 都经过它，状态码 ≥500 的错误也由它记日志。只有 `recovery` 直接调用底层的 `writeAPIError`，因为 panic 已经记过日志。
- `toAPIError` 把 error 映射成 OpenAI 错误信封：`*APIError` 原样返回；`codex.ErrTimeout`→504 `timeout`；`ErrShuttingDown`→503 `shutting_down`（`Retry-After: 5`）；`codex.ErrBusy`→503 `server_busy`（`Retry-After: 30`）；`context.Canceled`→499 `canceled`；`*codex.RunError`→500 `image_generation_failed`，并附上 codex 最后一条消息；其余错误返回 500，不暴露细节。
- 新增错误类型时要在 `toAPIError` 中加映射，并同步更新 README 的错误表。参数错误用 `invalidRequest(param, ...)` 构造。

### 优雅关闭

`main.go` 的 `run(ctx, cfg, logger, ln)` 通过 `http.Server.BaseContext` 让所有请求 ctx 派生自一个可取消的 base ctx。收到 SIGINT/SIGTERM 后先停止接受新连接，等待 `server.shutdown_timeout`；到期后以 `server.ErrShuttingDown` 为 cause 取消 base ctx，从而杀掉进行中的 codex 并完成清理，额外的清理宽限期为 `cleanupGrace`（10s）。第二次 Ctrl+C 直接退出。`main_test.go` 直接调用 `run` 测试这条链路。

### internal/codex

- **`prompt.go`**：`BuildPrompt(Job)` 生成经 stdin 传给 codex 的 prompt，必须以 `$imagegen` 开头。prompt 中**不出现任何文件路径**，输入图片只用 `[Image #k]` 指代（通过 `-i` 附加）。`n` 体现为"交付 n 张最终图片"：允许 codex 检查结果并再次调用 image_gen 替换不合格的草稿，最后一条消息用 `FINAL_IMAGE: <文件名>` 逐行列出交付的图片；prompt 还要求 codex 不复制、移动或保存任何图片。透明背景体现为 `transparent_background=true`。
- **`aspect.go`**：`AspectRatio(w, h)` 把 `size` 换算成长宽比：误差 ≤5% 时吸附到常见比例，否则取约简比值（各项 ≤25），并限制在 1:3 ~ 3:1。结果只写进 prompt，因为内置工具无法指定像素。
- **`runner.go`**：
  - 并发：信号量限制并发数为 `codex.max_concurrency`；最多 `codex.max_queue` 个请求等待槽位，最长等待 `codex.queue_timeout`，否则返回 `ErrBusy`。`codex.timeout` 从拿到槽位后开始计时。ctx 被取消时返回 `context.Cause(ctx)`。
  - 工作目录：`NewWorkdir(id)` 创建 `<workspace.dir>/<YYYYMMDD>/<id>/output/`，用 `Mkdir` 保证目录不被复用；`input/` 只在编辑请求中创建。
  - 进程：`BuildArgs` 中的 `-i` 必须放在最后（它是可变参数，会吞掉后续的位置参数）。codex 在独立进程组中运行，超时或取消时整组 SIGKILL。
  - 收集：codex 结束后，从 `codex.jsonl` 的 `thread.started` 事件取 `thread_id`，读取 `$CODEX_HOME/sessions/*/*/*/rollout-*-<tid>.jsonl` 中已完成的 `image_gen.generation` 条目，按顺序到 `generated_images/<tid>/<item_id>.*` 取图；文件缺失时解码 rollout 中的 base64，没有 rollout 时按 mtime 兜底。再用 `selectFinal` 选图：按 `last_message.txt` 中 `FINAL_IMAGE:` 列出的 id（文件名去扩展名）依次选取，最多 `n` 张；一张都对不上时取最后 `n` 张。图片写入 `output/`。最后（包括失败时）用 `cleanupThread` 删除该 thread 的 `generated_images` 目录和 rollout 文件。从 codex 输出读到的 id 在拼路径前都要经过 `validID` 校验。
  - 清理：`Cleanup` 只在 `workspace.keep` 不为 0 **且 codex 实际运行过**时保留工作目录（`wd.ran`）；在 codex 启动前就被拒绝的请求一律删除工作目录。
- **`prune.go`**：`workspace.keep = N` 时，`RunPruner` 在启动时和每天本地 0 点删除早于"今天 − N 天"的 `<YYYYMMDD>` 目录，由 `main.go` 的 `run` 启动。

### internal/config

负责 YAML 加载、默认值（`normalize`）和校验。`workspace.dir` 必填，会被转成绝对路径；`codex.sandbox` 只允许 `read-only`、`workspace-write`、`danger-full-access`；`limits.max_n` 上限为 10；`limits.max_images` 上限为 5（内置 `image_gen` 最多接受 5 张参考图）。`workspace.keep` 是 `*Keep` 类型，YAML 中可以写 bool 或天数：`false`/`0` 表示请求结束即删，`N` 表示保留 N 天，`true` 或不设置表示永久保留。用 `KeepWorkdirs()`、`RetentionDays()` 读取，构造用 `KeepDays(n)` 或 `KeepForever()`。

## 编码约定

- 只用 gin，不引入其他 HTTP 框架。
- handler 内用 `c.Request.Context()` 作为 ctx，**不要**把 `*gin.Context` 直接当 ctx 传，因为它不传播取消。
- 打日志时从 ctx 取 logger：server 中用 `s.log(c)`，codex 中用 `logctx.From(ctx, …)` 或 `r.log(ctx)`，这样日志可以按 `request_id` 关联。
- 代码注释和标识符用英文；commit message 用中文。

## 约束与注意事项

- 不要给 codex 加 `--ephemeral`：rollout 不落盘时，图片收集只能走 mtime 兜底。
- 内置 `image_gen` 只支持 `prompt`、`transparent_background`、`referenced_image_paths`、`num_last_images_to_include` 四个参数。OpenAI 的其余参数要么折算进 prompt（`n`、`size`），要么接收后忽略（`mask`、`quality`、`model` 等）；`file_id` 返回 400。
- 输入图片支持 png/jpeg/webp/gif（按内容嗅探）；输出图片只识别 png/jpeg/webp。
- 测试用 shell 脚本 `fakeCodex` 模拟 `codex exec`：`internal/server/server_test.go` 中的版本会写 `thread.started` 事件、rollout 和 `generated_images`，用于端到端 HTTP 测试；`cmd/codex-image-api/main_test.go` 中的版本用于优雅关闭测试。改动 codex 输出格式的解析逻辑时，要同步更新这两个脚本。
- Docker：容器内必须使用 `codex.sandbox: danger-full-access`（bubblewrap 在默认 seccomp/AppArmor 下无法创建命名空间），`workspace.dir` 为 `/data/workspace`。`docker/entrypoint.sh` 会把 `app` 用户重映射为 `APP_UID`/`APP_GID` 后降权运行；它**绝不 chown 挂载的宿主机目录**，只校验属主。修改入口脚本时要保持这一点，并保证脚本幂等。
