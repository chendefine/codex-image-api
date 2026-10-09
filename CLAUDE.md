# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

把 `codex exec` + `$imagegen` skill 封装成 OpenAI 兼容的图片接口（`POST /v1/images/generations`、`POST /v1/images/edits`、`GET /healthz`）。服务本身不调用任何图片 API：每个请求启动一次 `codex exec`，由 codex 用**内置 `image_gen` 工具**（ChatGPT 登录额度）出图，服务再从 `$CODEX_HOME` 的本地存储中收集图片，以 `b64_json` 返回。参数映射、工作目录布局、图片收集流程的完整说明见 [README.md](README.md)，修改相关行为时需同步更新 README。

## 常用命令

```bash
go build -o bin/codex-image-api ./cmd/codex-image-api
./bin/codex-image-api -config config.yaml      # 先 cp config.example.yaml config.yaml
go test ./... -count=1                          # 全部测试（不需要真实 codex）
go test ./internal/server -run TestEditsMultipart -count=1   # 单个测试
```

运行前置条件：`codex` 已登录且启用 `image_generation` feature。

## 架构

三层，调用方向 `server → codex`，二者都依赖 `config`：

- **`internal/server`**：纯 gin，无其他 HTTP 框架。路由在 `server.go` 的 `Handler()` 中注册；全局中间件（`middleware.go`）依次为 `requestID` → `accessLog` → `recovery`；`/v1` 组额外挂 `auth`（`server.api_keys` 非空时比较 SHA-256 摘要校验 Bearer）。`requestID` 生成 `X-Request-Id`（同时用作工作目录名，经 `c.GetString(requestIDKey)` 取得），并把带 `request_id` 的 logger 放进 request context（`internal/logctx`）：server 用 `s.log(c)`、runner 用 `logctx.From(ctx, …)` 取，日志因此可按请求关联——新代码打日志也应从 ctx 取 logger。业务 handler 签名是 `func(*gin.Context) (T, error)`，经泛型 `handle(s, fn)` 适配：成功写 JSON，失败交给 `writeError`。handler 内用 `c.Request.Context()` 作为 ctx（**不要**直接把 `*gin.Context` 当 ctx 传，它不传播取消）。请求体在 handler 内手动解析（`request.go`：`decodeJSON` 带大小限制、`buildJob` 校验 prompt/n/background/size；edits 按 Content-Type 分 multipart / JSON 两条路径，归一成 `editInput`），然后 `NewWorkdir` → `execute`。
- **错误**：`errors.go` 的 `writeError` 是写错误响应的统一入口（handler、auth、NoRoute/NoMethod 都走它，负责 ≥500 的错误日志）；只有 recovery 因已记录 panic 而直接调底层的 `writeAPIError`。`toAPIError` 把 error 映射成 OpenAI 错误信封（`*APIError` 原样，`codex.ErrTimeout`→504，`ErrShuttingDown`/`codex.ErrBusy`→503 并带 `Retry-After`，`context.Canceled`→499，`*codex.RunError`→500 并带上 codex 最后一条消息，其余 500 且不暴露细节）。新增错误类型时在 `toAPIError` 加映射。
- **关闭**：`main.go` 的 `run(ctx, cfg, logger, ln)` 用 `BaseContext` 让所有请求 ctx 派生自一个可取消的 base ctx；ctx 结束（SIGINT/SIGTERM）后 `server.shutdown_timeout` 到期时以 `server.ErrShuttingDown` 为 cause 取消它，从而杀掉进行中的 codex 并完成清理。`main_test.go` 直接调用 `run` 测试这条链路。
- **`internal/codex`**：
  - `prompt.go` 的 `BuildPrompt(Job)` 生成经 stdin 传给 codex 的 prompt，必须以 `$imagegen` 开头；prompt 中**不出现任何文件路径**，输入图片只用 `[Image #k]` 指代（通过 `-i` 附加）。
  - `aspect.go` 把 `size` 的 `WxH` 换算成长宽比（就近吸附常见比例，限制在 1:3 ~ 3:1），只体现在 prompt 中，内置工具无法指定像素。
  - `runner.go`：信号量限制并发（`codex.max_concurrency`），等待槽位的请求最多 `codex.max_queue` 个、最长 `codex.queue_timeout`，否则返回 `ErrBusy`；ctx 被取消时返回 `context.Cause(ctx)`；工作目录名即请求 ID（`NewWorkdir(id)`，用 `Mkdir` 保证不复用）；`BuildArgs` 中 `-i` 必须放在最后（它是可变参数，会吞掉后续位置参数）；codex 在独立进程组中运行，超时/取消时整组 SIGKILL。结束后从 `codex.jsonl` 的 `thread.started` 取 `thread_id`，读 `$CODEX_HOME/sessions/.../rollout-*-<tid>.jsonl` 中已完成的 `image_gen.generation` 条目，到 `generated_images/<tid>/` 取图（缺失时解码 rollout 中的 base64；无 rollout 时按 mtime 兜底），写入 `output/`，最后删除该 thread 的 `generated_images` 目录和 rollout 文件。
- **`internal/config`**：YAML 配置加载与默认值；`workspace.keep` 是 `*bool`，未设置时默认保留工作目录（仅保留 codex 实际运行过的目录，见 `Runner.Cleanup`）。

## 约束与注意事项

- 不要给 codex 加 `--ephemeral`：rollout 不落盘会导致收集只能走 mtime 兜底。
- 内置 `image_gen` 只支持 `prompt`、`transparent_background`、`referenced_image_paths`、`num_last_images_to_include`；OpenAI 的其余参数要么折算进 prompt（`n`、`size`），要么接收后忽略，`file_id` 返回 400。
- `internal/server/server_test.go` 用一个 shell 脚本 `fakeCodex` 模拟 `codex exec`（写 `thread.started` 事件、rollout 和 `generated_images`），做端到端 HTTP 测试；改动 codex 输出格式的解析逻辑时要同步更新这个脚本。
