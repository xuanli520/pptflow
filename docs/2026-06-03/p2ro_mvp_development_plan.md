# p2ro MVP 开发方案

## 开发结论

p2ro MVP 采用“先 fork 独立产品仓，后抽共享内核”的节奏。第一阶段不强行拆 Go module，避免在产品尚未稳定时制造抽象成本；但所有新增生产逻辑必须放在 p2ro 专属目录，避免污染 p2r 质检端共享代码。第二阶段再把稳定共享能力抽成 `p2r_core` 或等价共享 module。

默认落地顺序：

```text
Fork hygiene -> 产品入口 -> 作业模型 -> Claude runtime spike -> Agent adapter -> P0/P1/P2/P3/P5/P9/P10 -> TUI -> 契约验证 -> 双仓同步机制
```

## Claude runtime 前置结论

已验证agent sdk可用

2026-06-09 调研结论：p2ro MVP 不能依赖未确认的 `claude app-server`。本机 Claude Code 2.1.143 没有暴露 Codex 风格 `app-server --listen stdio://`；官方 Anthropic 文档给出的可编程路径是 Claude Agent SDK 和 `claude -p` 的 headless/stream-json 模式。Remote Control 和 `claude mcp serve` 都不是 p2ro TUI 可直接消费的本地 agent 编排 API。

开发顺序必须先做 spike，再写 P0-P9：

1. 验证 Claude Agent SDK 能否满足 p2ro runtime contract。
2. 如果 SDK 不满足，验证 Claude CLI stream-json adapter。
3. 如果 Claude 两条路径都不满足，才允许把 Codex app-server 作为临时 producer 后端。

Claude runtime spike 的通过标准：

- 能启动独立会话并关联稳定 session ID。
- 能流式解析 message/tool/error/usage 事件。
- 能限制工具集合和权限模式。
- 能把 workspace 写入限定在作业目录内。
- 能拦截或记录安装依赖、联网、删除、git 破坏性操作。
- 能取消运行中的会话并落盘终止原因。
- 能把 stdout/stderr/raw event 写入 `.p2ro/runs/<run_id>/logs/`。
- 能在崩溃后恢复为 `manual_required` 或 `failed_blocked`，不丢失阶段状态。

## 目录策略

### MVP fork 内目录

```text
cmd/
  root.go
  operator.go
  new.go
  run.go
  tui.go

internal/operator/
  model.go
  store.go
  migrate.go
  lifecycle.go
  stage.go
  scheduler.go

internal/producer/
  stage_p0_intake.go
  stage_p1_scaffold.go
  stage_p2_test_freeze.go
  stage_p3_implement.go
  stage_p5_self_check.go
  stage_p9_package.go
  prompt_context.go
  package_manifest.go
  self_check.go

internal/agent/
  session.go
  runtime_probe.go
  claude_sdk_adapter.go
  claude_cli_adapter.go
  codex_appserver_adapter.go
  policy.go
  permission.go
  events.go
  diff.go

internal/workspace/
  layout.go
  scaffold.go
  snapshot.go
  cleanup.go
  manifest.go

internal/qaadapter/
  p2r_scan.go
  p2r_stage_a.go
  runtime_check.go

internal/tui/
  作业端页面、布局、按键、视图模型

assets/prompt_profiles/p2ro/
  intake.md
  scaffold.md
  test_freeze.md
  implement.md
  self_check.md
  package.md
```

### 暂不移动的共享目录

MVP 先直接继承 fork 中的现有目录，避免早期抽包：

```text
internal/config
internal/db
internal/executor
internal/docker
internal/browser
internal/codex
internal/preflight
internal/projectlayout
internal/scanner
assets/scripts
```

这些目录只能做兼容性修复，不能加入 p2ro 产品语义。

## 阶段 0：fork 和双仓基础

目标：

- 建立独立 `p2ro_tui` 仓库。
- 保留 p2r upstream 同步能力。
- 把产品层和共享层分开。

任务：

1. 从 p2r 当前仓 fork 新仓。
2. 添加 `upstream` 指向 p2r 主仓。
3. 修改 module path 和二进制名为 p2ro。
4. 将 CLI root 从 `p2r` 改为 `p2ro`。
5. 保留 p2r 的 Stage A/B/G/C 相关共享能力。
6. 建立 `docs/shared-boundary.md`，记录哪些目录允许同步、哪些目录产品分叉。
7. 新增同步脚本或文档化命令：列出 shared 目录 diff、cherry-pick upstream shared commits、运行契约测试。

验收：

- `go test ./...` 通过。
- `p2ro version` 输出 p2ro 名称。
- 仓库能从 p2r upstream 拉取 shared 修复而不冲突产品层。

## 阶段 1：作业端配置和数据模型

目标：

- 建立 p2ro 作业端状态，不复用 p2r 质检任务状态文案。

新增模型：

```go
type OperatorJob struct {
    ID string
    Prompt string
    ProjectType string
    Language string
    Framework string
    State string
    CurrentRunID string
    PackagePath string
    CreatedAt string
    UpdatedAt string
}

type OperatorRun struct {
    RunID string
    JobID string
    Status string
    ArtifactRoot string
    PackagePath string
}

type OperatorStageRecord struct {
    Stage string
    Status string
    LogPath string
    ArtifactPaths []string
    Findings []Finding
}
```

MVP 状态：

```text
queued
intaking
scaffolding
test_freezing
implementing
self_checking
packaging
ready_for_qa
manual_required
failed_blocked
cancelled
```

数据库：

- 可以先新增 p2ro 专属表，不改 p2r `tasks` 语义。
- 表名建议：`operator_jobs`、`operator_runs`、`operator_stages`、`operator_permissions`。
- 如果复用 p2r `runs/run_stages/findings`，必须用 adapter 隔离产品语义。

验收：

- 能创建作业。
- 能查询作业列表。
- 能持久化 run、stage、permission。
- 崩溃后能恢复到 terminal 或 manual_required 状态。

## 阶段 2：Claude runtime spike

目标：

- 验证 Claude Code 能否作为 p2ro producer runtime。
- 明确首选实现是 SDK、CLI stream-json 还是临时 Codex adapter。
- 在任何 P0-P9 stage 编码前锁定 runtime contract。

实现：

```text
internal/agent/runtime_probe.go
internal/agent/claude_sdk_adapter.go
internal/agent/claude_cli_adapter.go
internal/agent/events.go
internal/agent/policy.go
```

Spike 顺序：

1. 安装并验证 `@anthropic-ai/claude-agent-sdk` 或 `claude-agent-sdk`，确认 SDK 能启动会话、流式输出、限制工具、写 workspace、取消会话。
2. 如果 SDK 能力缺口影响 TUI 编排，验证 `claude -p --input-format stream-json --output-format stream-json`。
3. 如果 CLI 路径可用，定义 stdout JSONL parser、stdin event writer、session ID 持久化和进程取消语义。
4. 如果 Claude 路径不可用，保留 `CodexAppServerAdapter`，但禁止复用 p2r 静态审查的 read-only 配置。

本机已确认能力：

```text
claude --version = 2.1.143 (Claude Code)
claude --help exposes:
  --print
  --output-format stream-json
  --input-format stream-json
  --allowedTools / --disallowedTools / --tools
  --permission-mode
  --session-id / --continue / --resume
  --mcp-config / --strict-mcp-config
  --include-partial-messages
```

本机未确认能力：

```text
claude app-server --listen stdio://
JSON-RPC initialize/thread/start/turn/start/turn/steer
无需 claude.ai 登录的 Remote Control 本地编排
```

验收：

- 产出 `runtime_spike_report.md`，记录 SDK/CLI/Codex 三条路径的结论。
- 至少一个 Claude runtime 能完成“写入临时 workspace 文件 -> 输出 structured event -> 取消或正常结束”的最小闭环。
- 权限策略至少覆盖 read/write/test/install/network/delete/git 七类动作。
- runtime raw events、标准化 events、stderr/stdout 都能落盘。
- 无法验证 Claude 时，开发计划必须显式降级到 Codex 临时 adapter，不能继续引用 `claude app-server`。

## 阶段 3：Agent producer adapter

目标：

- 新增可写 workspace 的 producer agent session。
- 不复用 p2r D/E/F 的 read-only reviewer session。

实现：

```text
internal/agent/session.go
internal/agent/claude_sdk_adapter.go
internal/agent/claude_cli_adapter.go
internal/agent/codex_appserver_adapter.go
internal/agent/policy.go
internal/agent/permission.go
internal/agent/events.go
internal/agent/diff.go
```

核心接口：

```go
type RuntimeAdapter interface {
    StartSession(context.Context, SessionRequest) (Session, error)
}

type Session interface {
    Send(context.Context, AgentInput) error
    Events() <-chan Event
    Wait(context.Context) (SessionResult, error)
    Cancel(context.Context) error
}
```

事件：

```text
message.delta
tool.call
tool.result
permission.request
permission.decision
file.changed
diff.updated
stage.completed
stage.failed
```

权限策略：

- `auto allow`
- `require human`
- `deny`

验收：

- agent 输出能流式进入 scheduler/TUI。
- 写 workspace 内文件被允许。
- 访问 workspace 外路径被拒绝。
- 安装依赖、联网、删除大范围文件进入权限队列。
- 每个 permission 事件写入 `permissions.jsonl`。
- adapter 可以替换 runtime 后端，不影响 P0-P9 stage 代码。

## 阶段 4：Workspace 和 package skeleton

目标：

- p2ro 控制产物和交付包彻底分离。

workspace 布局：

```text
<scan_path>/work/<job_id>/
  package/TASK-xxx/
    docs/
    repo/
    original_sessions/
    metadata.json
  .p2ro/
    runs/<run_id>/
```

最终输出：

```text
<scan_path>/ready-for-qa/<batch>/<task_id>/<task_id>/
  docs/
  repo/
  original_sessions/
  metadata.json
```

实现：

- `internal/workspace/layout.go`
- `internal/workspace/scaffold.go`
- `internal/workspace/snapshot.go`
- `internal/workspace/cleanup.go`
- `internal/workspace/manifest.go`

验收：

- scaffold 后包根结构符合 `projectlayout.ValidatePackageRoot`。
- 控制目录不进入最终交付包。
- snapshot 能检测 P3/P5/P9 文件变更。

## 阶段 5：P0 intake

目标：

- 把自由 prompt 转成结构化作业计划。

输入：

- prompt 文本
- project type
- 可选语言/框架

输出：

```text
p2ro_intake.json
metadata.json
docs/questions.md
logs/P0_intake.log
```

实现要点：

- prompt 原文原样写入 `metadata.json.prompt`。
- 语言不确定时不瞎猜为硬约束。
- 不明确需求写入 `docs/questions.md`。
- 生成 P1/P2/P3 所需 context。

验收：

- 空 prompt 拒绝。
- project type 非法拒绝。
- prompt 注入 shell 命令不会执行。
- intake 结果可被 P1 使用。

## 阶段 6：P1 scaffold

目标：

- 按初始化提示词搭建框架和文档，不写业务代码。

prompt profile：

```text
assets/prompt_profiles/p2ro/scaffold.md
```

输出：

```text
docs/design.md
docs/api-spec.md
docs/questions.md
docs/test-plan.md
repo/README.md
original_sessions/P1_scaffold.md
```

实现要点：

- P1 agent policy 禁止大规模业务实现。
- repo 允许最小入口和目录结构。
- P1 后执行 package layout check。
- 如果检测到完整业务实现，标记 Medium finding，要求人工确认或回滚阶段。

验收：

- `docs/`、`repo/`、`original_sessions/`、`metadata.json` 存在。
- `metadata.json.prompt` 存在。
- `docs/design.md` 和 `docs/test-plan.md` 存在。
- 不出现明显业务代码主体。

## 阶段 7：P2 test-freeze

目标：

- 按测试冻结提示词生成可运行测试。

prompt profile：

```text
assets/prompt_profiles/p2ro/test_freeze.md
```

输出：

```text
repo/unit_tests/
repo/API_tests/
repo/run_tests.sh
repo/run_tests.ps1
p2ro_test_freeze_report.md
logs/P2_test_freeze.log
```

实现要点：

- 生成测试后运行一次测试命令。
- 允许业务未实现导致测试失败。
- 失败必须能被日志解释。
- 不允许删除或弱化 P1 文档中的核心需求。

验收：

- `run_tests.sh` 存在且可执行。
- Windows 下可选生成 `run_tests.ps1`。
- 测试命令能启动并产生日志。
- 测试覆盖核心 happy path 和关键失败路径。

## 阶段 8：P3 implement

目标：

- 按冻结文档和测试完成业务实现。

prompt profile：

```text
assets/prompt_profiles/p2ro/implement.md
```

循环：

```text
read docs -> inspect tests -> implement -> run tests -> fix -> repeat
```

停止条件：

- 测试通过。
- 达到最大轮次。
- 同类失败重复超过阈值。
- 需要人工权限。
- prompt 不可实现。

输出：

```text
p2ro_implementation_report.md
diff_summary.json
logs/P3_implement.log
```

实现要点：

- 每轮前后做 workspace snapshot。
- diff summary 包含新增、修改、删除、风险文件。
- 运行测试必须走 policy，不能让 agent 直接自由执行任意命令。
- 失败分类进入 finding。

验收：

- 对至少一个 fixture prompt 能生成真实业务实现。
- 不出现 fake success、硬编码通过、静态冒充。
- diff summary 能在 TUI 展示。

## 阶段 9：P5 self-check

目标：

- 复用 p2r 交付红线检查。

执行项：

```text
run_validate.py
run_acceptance.py
check_required_artifacts.py
check_readme_alignment.py
check_local_dependency.py
check_fake_impl.py
check_english_only.py
```

输出：

```text
p2ro_self_check_summary.json
p2ro_self_check_report.md
logs/P5_self_check.log
```

实现要点：

- 检查脚本从共享 `assets/scripts` 来。
- findings 映射到 operator stage record。
- Blocker/High 默认阻断 P9，除非人工确认进入 manual_required。

验收：

- 缺失 `metadata.json.prompt` 能被阻断。
- 缺失 `run_tests` 能被阻断。
- fake implementation 能产生 finding。
- README 与启动命令不一致能产生 finding。

## 阶段 10：P9 package

目标：

- 清理并生成待质检交付包。

输出：

```text
handoff_summary.md
p2ro_package_manifest.json
p2ro_run_manifest.json
ready-for-qa/<batch>/<task_id>/<task_id>/
```

实现要点：

- 清理 `node_modules`、`.venv`、`.codex`、`.vscode`、缓存、数据库文件。
- 保留必要 `original_sessions`。
- 复制时使用白名单。
- 最后调用 p2r `projectlayout.ValidatePackageRoot` 或共享等价能力。

验收：

- 待质检包能被 p2r `scanner.Scan` 识别。
- 控制目录不进入包。
- package manifest 列出所有文件、大小和 hash。

作业员自测附件上传不能耦合在 P9 package 内。P9 只产出本地 artifact 清单；平台侧上传由 P10 self-test attachment adapter 消费该清单。P10 由 p2ro 原生实现接管 quality-runner 当前承担的自测附件上传职责。

## 阶段 10.5：P10 upload-self-test-attachments

目标：

- 上传 report/log/summary 类 `ai_test_report` 附件到作业员平台。

输入：

```text
p2ro_package_manifest.json
p2ro_run_manifest.json
artifact mapping
```

输出：

```text
p2ro_upload_manifest.json
logs/P10_upload.log
```

**实际部署环境变量：** 云电脑已在 `/etc/profile.d/devenv.sh` 中显式配置 `export API_KEY=sk-Ptn4SLGzkPGdT2wEZDNEu4iCqGTYWprneaIb6RAzrnZwVUfy`。这是平台的官方部署配置，读取此环境变量不是”从二进制中提取密钥”，而是使用平台显式提供的凭据注入。p2ro 的 `native_http` provider 可直接使用此环境变量。

实现要点：

- P10 默认使用 p2ro `native_http` provider 直接调用平台 self-test attachment API。
- p2ro 只读取显式配置的上传凭据环境变量，默认 `API_KEY`。
- p2ro 不从 quality-runner 二进制、日志、进程环境或内存中提取凭据。
- p2ro 不打印、不持久化上传凭据。
- 按 artifact mapping 过滤存在的本地产物，不生成空 sample 文件。
- 每个文件上传前计算 `file_size`、`file_type`、sha256。
- 调用 `batch-presigned-url` 获取短时上传地址。
- 使用返回的 presigned URL PUT 文件 bytes。
- 调用 `batch-confirm` 确认 attachment ID。
- 上传前后执行 exists/list 对比。
- manifest 只保存 `attachment_id`、`object_path`、sha256、file_name、dimension_type，不保存 presigned URL。
- 日志脱敏 API key、Authorization、presigned URL、`X-Amz-Signature`、`X-Amz-Credential`。

验收：

- `codex_report.md` 可上传并确认成功。
- `logs/B_docker.log` 上传为 `docker_startup.log`。
- `logs/C_tests.log` 上传为 `run_tests.log`。
- HTTP 200 但业务码失败会记录 adapter finding。
- 上传失败不删除本地 artifact，不改变 P9 package。
- 上传凭据未配置、不可见或未授权时阶段进入 `manual_required`。

### P10 上传契约

模块边界：

```text
internal/operator/upload/selftest
```

配置：

```json
{
  "schema_version": "p2ro.self_test_upload.config.v1",
  "enabled": true,
  "provider": "native_http",
  "base_url": "",
  "read_api_key_env": "API_KEY",
  "upload_api_key_env": "API_KEY",
  "timeout_seconds": 60,
  "retry_presigned_once": true,
  "require_human_confirm": true
}
```

凭据策略：

```text
1. native_http
   MVP 默认且唯一必需 provider。
   p2ro 读取自身进程显式可见的上传凭据环境变量，默认 API_KEY。
   p2ro 直接调用 exists/list/batch-presigned-url/PUT/batch-confirm。

2. quality_runner_observation
   quality-runner.exe / run_quality.py 只作为已验证 API 行为和历史产物命名的参考。
   p2ro 不调用 quality-runner 完成上传，不等待 runner 暴露独立上传命令，不从 runner 中抽取凭据。

3. unavailable
   上传凭据未配置、不可见或未授权时，P10 进入 manual_required。
```

当前 `quality-runner.exe` 验证结果：

```text
quality-runner.exe --help
quality-runner.exe help
quality-runner.exe upload-self-test-attachments --help
quality-runner.exe upload --help
```

均只返回：

```text
Usage: quality-runner.exe run [flags]
```

`quality-runner.exe run --help` 只暴露 `--path`、`--task-id`、`--timeout`、`--report-name`、`--quiet`、`--verbose`、`--skip-ui`、`--skip-ui-interaction`、`--model`。`--skip-upload` 不存在。二进制字符串能看到 `BatchPresignedURL`、`UploadBytes`、`BatchConfirm`，说明上传能力在 runner 内部，但当前没有公开独立上传命令。

该事实不再阻塞 P10。最终口径是 p2ro 接管上传：P10 通过平台正式 self-test attachment API 原生上传 report/log/summary 类产物，quality-runner 只保留为 API 行为对照和兼容验收对象。

凭据边界：

- p2ro 只读取自身进程显式配置的上传凭据，默认 env 名为 `API_KEY`。
- p2ro 不硬编码 API key，不把 API key 写入 config、DB、manifest、日志或 stdout/stderr。
- p2ro 不通过反编译、日志抓取、进程环境扫描、内存读取等方式从 quality-runner 中提取密钥。
- p2ro 日志必须脱敏 API key、Authorization、download_url、upload_url、`X-Amz-Signature`、`X-Amz-Credential`。
- quality-runner 内部只读任务列表 key 不是上传凭据，p2ro 不依赖、不抽取、不复用。

P10 原生上传 API contract：

```text
p2ro P10
  -> read upload config
  -> build artifact mapping from P9 manifest
  -> exists/list before upload
  -> batch-presigned-url
  -> PUT bytes to presigned URL
  -> batch-confirm
  -> exists/list after upload
  -> write p2ro_upload_manifest.json
```

artifact mapping：

```json
{
  "schema_version": "p2ro.self_test_upload.mapping.v1",
  "dimension_type": "ai_test_report",
  "files": [
    {
      "local_path": "codex_report.md",
      "file_name": "codex_report.md",
      "file_type": "text/markdown",
      "required": false
    },
    {
      "local_path": "logs/B_docker.log",
      "file_name": "docker_startup.log",
      "file_type": "text/plain",
      "required": false
    },
    {
      "local_path": "logs/C_tests.log",
      "file_name": "run_tests.log",
      "file_type": "text/plain",
      "required": false
    },
    {
      "local_path": "test_runtime_summary.json",
      "file_name": "test_runtime_summary.json",
      "file_type": "application/json",
      "required": false
    }
  ]
}
```

adapter interface：

```go
type SelfTestAttachmentClient interface {
	Exists(ctx context.Context, req AttachmentExistsRequest) (AttachmentExistsResult, error)
	List(ctx context.Context, req AttachmentListRequest) (AttachmentListResult, error)
	BatchPresignedURL(ctx context.Context, req BatchPresignedURLRequest) (BatchPresignedURLResult, error)
	UploadBytes(ctx context.Context, req UploadBytesRequest) error
	BatchConfirm(ctx context.Context, req BatchConfirmRequest) (BatchConfirmResult, error)
}
```

provider interface：

```go
type SelfTestUploadProvider interface {
	Name() string
	Available(ctx context.Context) ProviderStatus
	Upload(ctx context.Context, req UploadRunRequest) (UploadManifest, []Finding)
}
```

provider 选择：

```text
if native_http credentials configured and human confirmed:
  use NativeHTTPProvider
else:
  manual_required
```

P10 runner contract：

```go
type SelfTestUploadRunner interface {
	Plan(ctx context.Context, artifactRoot string, mapping ArtifactMapping) (UploadPlan, error)
	Run(ctx context.Context, req UploadRunRequest) (UploadManifest, []Finding)
}
```

上传请求顺序：

```text
1. read P9 manifest
2. materialize artifact mapping
3. filter missing optional files
4. compute file_size / file_type / sha256
5. exists before upload
6. batch-presigned-url
7. PUT file bytes to presigned upload_url
8. batch-confirm
9. exists/list after upload
10. write p2ro_upload_manifest.json
```

`exists` request：

```json
{
  "external_task_id": "TASK-...",
  "dimension_type": "ai_test_report",
  "file_name": "codex_report.md"
}
```

`exists` success response：

```json
{
  "code": 200,
  "message": "查询成功",
  "data": {
    "external_task_id": "TASK-...",
    "dimension_type": "ai_test_report",
    "file_name": "codex_report.md",
    "exists": true,
    "attachment_id": "<attachment_id>",
    "object_path": "TASK_DISPATCH/attachments/.../codex_report.md",
    "file_type": "text/markdown",
    "file_size": 54690,
    "download_url": "<redacted_presigned_download_url>",
    "expires_in": 600,
    "uploaded_at": "2026-06-09T21:15:19.311250"
  }
}
```

`batch-presigned-url` request：

```json
{
  "external_task_id": "TASK-...",
  "files": [
    {
      "dimension_type": "ai_test_report",
      "file_name": "codex_report.md",
      "file_size": 54690,
      "file_type": "text/markdown"
    }
  ]
}
```

`batch-presigned-url` success response：

```json
{
  "code": 200,
  "message": "获取上传URL成功",
  "data": {
    "items": [
      {
        "dimension_type": "ai_test_report",
        "attachment_id": "<attachment_id>",
        "upload_url": "<redacted_presigned_upload_url>",
        "object_path": "TASK_DISPATCH/attachments/.../codex_report.md",
        "expires_in": 600
      }
    ]
  }
}
```

PUT upload：

```text
method: PUT
url: response.data.items[i].upload_url
headers:
  Content-Type: same as requested file_type
body:
  raw file bytes
auth:
  no X-API-Key header; auth is embedded in the presigned URL
```

`batch-confirm` request：

```json
{
  "attachment_ids": ["<attachment_id>"]
}
```

`batch-confirm` success response：

```json
{
  "code": 200,
  "message": "批量确认完成",
  "data": {
    "success_ids": ["<attachment_id>"],
    "failed_items": []
  }
}
```

`p2ro_upload_manifest.json`：

```json
{
  "schema_version": "p2ro.self_test_upload.manifest.v1",
  "external_task_id": "TASK-...",
  "status": "uploaded",
  "started_at": "2026-06-09T21:15:18+08:00",
  "finished_at": "2026-06-09T21:15:19+08:00",
  "items": [
    {
      "local_path": "codex_report.md",
      "file_name": "codex_report.md",
      "dimension_type": "ai_test_report",
      "file_type": "text/markdown",
      "file_size": 54690,
      "sha256": "<sha256>",
      "exists_before": false,
      "attachment_id": "<attachment_id>",
      "object_path": "TASK_DISPATCH/attachments/.../codex_report.md",
      "confirmed": true,
      "exists_after": true
    }
  ],
  "redactions": [
    "api_key",
    "authorization",
    "upload_url",
    "download_url",
    "X-Amz-Signature",
    "X-Amz-Credential"
  ]
}
```

错误分类：

| 类型 | 判定 | 阶段结果 |
| --- | --- | --- |
| `config_missing` | 上传凭据未配置、不可见或未授权 | `manual_required` |
| `artifact_missing` | required file 不存在 | `failed` |
| `artifact_optional_missing` | optional file 不存在 | `passed_with_warnings` |
| `http_error` | HTTP status 非 2xx | `failed` |
| `business_error` | HTTP 200 但 `code != 200` | `failed` |
| `presigned_expired` | PUT 返回签名过期或 403 | retry once |
| `confirm_failed` | `failed_items` 非空或 attachment ID 不在 `success_ids` | `failed` |
| `verify_failed` | confirm 后 exists/list 不匹配 | `failed` |

接入方法：

```text
P9 package completed
  -> P10 Plan
  -> permission request: self-test attachment upload/confirm
  -> P10 Run
  -> write upload manifest
  -> keep job ready_for_qa when P9 is valid and P10 is uploaded or explicitly skipped by human
```

TUI 行为：

- P10 展示待上传文件、平台 file_name、dimension_type、file_size。
- 上传前要求人工确认目标 task ID 和文件列表。
- 上传中隐藏所有 presigned URL。
- 上传后展示 `attachment_id`、file_name、confirmed、exists_after。
- P10 failed 时保留重试按钮，重试重新申请 presigned URL。

## 阶段 11：TUI 改造

目标：

- 作业端 TUI 不再使用质检端三栏文案。

主视图：

```text
待生产 | 生产中/待人工 | 待质检/已完成
```

详情区域：

- prompt 摘要
- 当前阶段
- 流式日志
- diff 摘要
- 权限队列
- findings
- artifact 列表
- package path

实现建议：

- 可以复用 layout、viewport、scheduler snapshot 思路。
- 不直接复用 `TaskBoardModel` 的质检文案。
- 新增 `OperatorBoardModel`、`OperatorDetailViewModel`。
- pipeline bar 改成 operator job bar。

验收：

- 能新建作业。
- 能启动/取消作业。
- 能看到实时 P 阶段输出。
- 能批准或拒绝 require human 权限。
- 能查看 diff summary 和 package path。

## 阶段 12：p2r 兼容验证

目标：

- p2ro 产物必须被 p2r 识别。

验证路径：

```text
p2ro P9 package -> p2r scan -> p2r run --stage A -> p2r run --from B
```

MVP 最小验证：

- `scanner.Scan` 识别。
- Stage A 结构检查能运行。
- fullstack/backend fixture 可进入 B/G/C。

后续：

- P6 runtime-check 直接接入 B/G/C。
- P7 review 接入 D/E/F 或 p2ro 专用审查。
- P8 repair 接入循环修复。

验收：

- p2ro fixture 包在 p2r 中可扫描。
- Stage A 对有效包无结构阻断。
- Stage A 对故意缺陷包产生预期 finding。

## 双仓同步机制

### MVP 同步

保留 shared 目录清单：

```text
internal/config
internal/executor
internal/docker
internal/browser
internal/preflight
internal/projectlayout
internal/scanner
assets/scripts
```

每次同步：

1. 拉取 p2r upstream。
2. 查看 shared 目录 diff。
3. cherry-pick 或手工移植 shared 修复。
4. 运行 p2ro 单测。
5. 运行 p2r 兼容 fixture。
6. 记录同步结果。

### 中期抽包

触发条件：

- 同一个 shared 文件一个月内被两仓修改两次以上。
- p2ro 需要 p2r Stage G 或 Docker 修复但 cherry-pick 冲突频繁。
- schema 和 artifact contract 开始被两边共同扩展。

抽包目标：

```text
p2r_core/
  model
  artifact
  projectlayout
  executor
  docker
  browser
  schemas
  scripts
```

p2r 和 p2ro 都依赖 tagged release，不依赖对方 main。

## 测试策略

MVP 不为每个小改动写测试，但以下必须覆盖：

- 作业状态机转换
- 权限策略判定
- workspace path 防逃逸
- P1 不写业务代码的检测
- P2 run_tests 可运行性
- P5 交付红线映射
- P9 package whitelist
- p2r scanner 兼容
- p2r Stage A 兼容 fixture
- TUI 权限队列和 stage stream 基础展示
- self-test attachment adapter contract fixture

## 开发里程碑

### M0：fork 可运行

完成：

- p2ro 仓库创建
- module path 和 CLI 名称调整
- 共享目录不动
- `go test ./...` 通过

### M1：本地作业模型

完成：

- operator DB
- job/run/stage/permission 模型
- CLI 新建作业
- run lifecycle skeleton

### M2：Claude runtime spike

完成：

- `runtime_spike_report.md`
- Claude SDK 可行性结论
- Claude CLI stream-json 可行性结论
- runtime event contract
- 最小 workspace 写入闭环
- 取消和日志落盘

### M3：agent 和权限

完成：

- producer adapter
- policy engine
- permissions.jsonl
- stream events
- diff summary

### M4：P0/P1/P2

完成：

- intake
- scaffold
- test-freeze
- package skeleton fixture

### M5：P3/P5

完成：

- implement loop
- self-check
- findings 映射

### M6：P9 和 TUI

完成：

- package
- 作业端三栏 TUI
- 权限队列
- p2r scan 兼容验证

### M7：MVP hardening

完成：

- fixtures
- 崩溃恢复
- 取消作业
- 文档和同步清单
- release binary

### M8：作业员自测附件 API

完成：

- self-test attachment adapter
- `ai_test_report/codex_report.md` exists/list/download_url 只读验证
- `batch-presigned-url -> PUT presigned URL -> batch-confirm` 上传链路
- report/log/summary artifact mapping
- 上传前后 exists/list 对比
- 预签名 URL 和凭据日志脱敏

## 主要风险处理

| 风险 | 处理 |
| --- | --- |
| 双仓漂移 | shared 清单、contract fixtures、upstream-first |
| 误依赖 Claude app-server | runtime spike 先行，SDK/CLI/Codex 三路径显式结论 |
| p2ro 私改质检规则 | P5/P9 复用 shared 校验 |
| agent 越权 | policy engine、workspace root、防逃逸 |
| P1 过早实现业务 | P1 后检测业务代码和 diff |
| P2 测试弱化 | 测试冻结报告和 core requirement mapping |
| P3 假实现 | fake impl 检查、README 对齐、P5 阻断 |
| TUI 复用质检心智 | 新 OperatorBoard 和作业文案 |
| 控制产物污染交付包 | P9 whitelist copy 和 manifest hash |

## 最终 MVP 完成标准

- p2ro 独立仓可构建。
- Claude runtime spike 有明确通过或降级结论。
- TUI 可创建、运行、取消作业。
- P0/P1/P2/P3/P5/P9/P10 可串行完成。
- 权限请求不会进入自由 yes/no。
- `ready-for-qa` 产物能被 p2r `scan` 识别。
- report/log/summary 类 `ai_test_report` 附件能通过平台 API 上传并生成脱敏 manifest。
- 至少 3 个 fixture 覆盖 fullstack、pure_backend、pure_frontend。
- shared 同步清单和契约验证存在。
- 不修改 p2r 质检产品语义即可维护 p2ro 生产逻辑。
