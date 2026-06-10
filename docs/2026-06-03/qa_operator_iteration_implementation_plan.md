# p2r Stage G 基线与 p2ro MVP 最终执行方案

## 目标

本方案以当前源码为基线，收敛 `docs/2026-06-03` 中 p2r Stage G、p2ro producer、Claude runtime、self-test attachment 上传四条线的最终执行计划。

最终方向：

```text
p2r 继续作为质检端
p2ro 作为独立 fork 作业端
p2r 已落地的 B/G/C runtime 能力作为 p2ro 兼容契约
p2ro 原生接管 quality-runner 当前承担的 self-test attachment 上传职责
```

## 当前源码事实

Stage G 已经在 p2r 中落地，不再作为 p2ro MVP 的前置开发项。

已存在的关键文件：

```text
internal/pipeline/model/stages.go
internal/pipeline/stage.go
internal/pipeline/stage_g.go
internal/pipeline/browser_action.go
internal/pipeline/browser_policy.go
internal/pipeline/browser_url.go
internal/pipeline/browser_codex_session.go
internal/pipeline/frontend_e2e_schema.go
internal/pipeline/repo_snapshot.go
internal/browser/runner.go
internal/browser/playwright_wrapper.go
internal/browser/observation.go
assets/prompt_profiles/frontend_e2e_browser.md
assets/prompt_profiles/frontend_e2e_browser_action_prompt.md
tests/internal/pipeline/stage_g_test.go
```

已确认行为：

- `model.StageG` 已定义，默认顺序为 `A,D,E,F,B,G,C`。
- G 是 runtime stage，日志名为 `G_frontend_e2e.log`。
- B 失败或无 runtime 时，`blockedDependents("B")` 阻断 G/C。
- G blocked 时会 materialize `frontend_e2e_summary.json`、`frontend_e2e_report.md`、`G_frontend_e2e.log`。
- G 使用 URL candidates、browser action validator、Playwright wrapper、summary schema、finding 映射和 repo snapshot。
- D/E/F 的 `CodexReviewStage` 仍是 read-only static reviewer，G 不复用该执行器。

当前还不存在 p2ro 产品层：

```text
internal/operator/
internal/producer/
internal/agent/
internal/workspace/
internal/operator/upload/selftest/
cmd/operator.go
cmd/new.go
```

## 架构决策

### ADR 1：p2ro 采用独立 fork 产品仓

Decision：

p2ro MVP 从当前 p2r 仓 fork 出独立仓库，保留 upstream 指向 p2r。p2ro 新增 producer 产品层，不把 P0-P10 塞进 p2r A-G 质检阶段。

Drivers：

- p2r 是质检端，p2ro 是作业端 producer，两者产品语义不同。
- Go `internal/` 不能作为跨仓稳定共享 API。
- 当前共享能力尚在演进，MVP 先 fork 比提前抽 module 更稳。

Consequences：

- 共享能力变更 upstream-first。
- p2ro 产品逻辑必须放在专属目录。
- 当同一 shared 文件频繁双向修改时，再抽 `p2r_core`。

### ADR 2：Stage G 作为共享 runtime 基线

Decision：

p2r Stage G 已作为当前源码基线，p2ro 不重新实现浏览器 E2E。p2ro 后续 P6 runtime-check 复用 p2r 的 B/G/C 或抽出的共享等价能力。

Drivers：

- G 已具备受控 Codex planning + p2r validated Playwright execution。
- B/G/C 的 artifact、finding、runtime evidence 是 p2r 和 p2ro 的兼容契约。
- 重写 G 会制造两套 runtime truth。

### ADR 3：P10 由 p2ro 原生接管上传

Decision：

P10 使用 p2ro `native_http` provider 直接调用平台 self-test attachment API，不等待 quality-runner upload bridge。quality-runner 只作为已验证 API 行为和历史产物命名的参考。

Drivers：

- 用户目标是让 p2ro 接管 quality-runner 上传职责。
- API 链路已经验证：exists/list/download_url、batch-presigned-url、PUT presigned URL、batch-confirm。
- P10 必须可在 p2ro 内闭环，不被 quality-runner 二进制能力暴露情况阻塞。

Security：

- p2ro 只读取自身进程显式配置的上传凭据，默认 env 名为 `API_KEY`。
- p2ro 不硬编码、不输出、不持久化 API key。
- p2ro 不从 quality-runner 二进制、日志、进程环境或内存中提取密钥。
- 日志和 manifest 不保存 presigned URL、Authorization、`X-Amz-Signature`、`X-Amz-Credential`。

## 最终系统分层

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
  events.go
  policy.go
  permission.go
  diff.go
  claude_sdk_adapter.go
  claude_cli_adapter.go
  codex_appserver_adapter.go

internal/workspace/
  layout.go
  scaffold.go
  snapshot.go
  cleanup.go
  manifest.go

internal/operator/upload/selftest/
  client.go
  mapping.go
  runner.go
  manifest.go
  redaction.go

internal/qaadapter/
  p2r_scan.go
  p2r_stage_a.go
  runtime_check.go

internal/tui/
  operator board, detail, permission queue, artifact view
```

Shared 目录 MVP 先继承，不加 p2ro 产品语义：

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
internal/pipeline
assets/scripts
```

## 数据流

```text
prompt
  -> P0 intake
  -> package workspace
  -> P1 scaffold
  -> P2 test-freeze
  -> P3 implement
  -> P5 self-check
  -> P9 package
  -> P10 upload self-test attachments
  -> ready_for_qa

P9 package
  -> p2r scanner.Scan
  -> p2r Stage A
  -> optional B/G/C compatibility check
```

## 作业状态机

MVP 状态：

```text
queued
intaking
scaffolding
test_freezing
implementing
self_checking
packaging
uploading_self_test
ready_for_qa
manual_required
failed_blocked
cancelled
```

后续状态：

```text
contract_testing
runtime_checking
reviewing
repairing
qa_failed
qa_passed
```

默认成功路径：

```text
queued -> intaking -> scaffolding -> test_freezing -> implementing -> self_checking -> packaging -> uploading_self_test -> ready_for_qa
```

P10 上传完成不等于平台最终提交，MVP 默认终态仍是 `ready_for_qa`。

## 实现阶段

### M0：fork hygiene

范围：

- fork p2r 为 p2ro 仓。
- module path、二进制名、CLI root 改为 p2ro。
- 保留 upstream 指向 p2r。
- 建立 shared 目录清单。

验收：

- `go test ./...` 通过。
- `p2ro version` 输出 p2ro 名称。
- p2r A-G 质检逻辑未被产品文案污染。

### M1：operator 模型和存储

新增：

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

表：

```text
operator_jobs
operator_runs
operator_stages
operator_permissions
operator_artifacts
```

验收：

- 能创建、查询、恢复作业。
- 崩溃后 run 恢复为 terminal、`manual_required` 或 `failed_blocked`。
- 不复用 p2r `tasks` 的质检状态文案。

### M2：Agent runtime adapter

首选路径：

```text
Go TUI
  -> internal/agent RuntimeAdapter
  -> TypeScript sidecar JSONL protocol
  -> @anthropic-ai/claude-agent-sdk
```

接口：

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
session.started
message.delta
tool.call
tool.result
permission.request
permission.decision
file.changed
diff.updated
usage.updated
stage.completed
stage.failed
runtime.error
```

验收：

- SDK sidecar 能流式输出、写 workspace、生成 diff、落盘 raw event。
- `query.interrupt()` 用于软取消。
- 进程树 kill 用于 hard cancel。
- CLI adapter 只作为 fallback，不作为长期首选。

### M3：workspace 与交付包隔离

布局：

```text
work/<job_id>/
  package/TASK-xxx/
    docs/
    repo/
    original_sessions/
    metadata.json
  .p2ro/
    runs/<run_id>/
      run_manifest.json
      stage_status.json
      permissions.jsonl
      diff_summary.json
      logs/
```

验收：

- `.p2ro/` 不进入 P9 交付包。
- workspace path 防逃逸。
- snapshot 可检测 P3/P5/P9 文件变更。

### M4：P0/P1/P2/P3 producer stages

P0 intake：

- 原始 prompt 原样写入 `metadata.json.prompt`。
- 不确定事项写入 `docs/questions.md`。
- prompt 不作为 shell 指令执行。

P1 scaffold：

- 生成 docs、repo、original_sessions、metadata。
- 不写业务主体。
- 只允许最小启动骨架。

P2 test-freeze：

- 生成 `repo/unit_tests/`、`repo/API_tests/`、`repo/run_tests.sh`、可选 `repo/run_tests.ps1`。
- 测试必须可运行，允许因业务未实现失败。
- 不允许弱化需求。

P3 implement：

- 按冻结测试和文档实现真实业务。
- 每轮运行测试、记录日志、更新 diff summary。
- 不允许 fake logic、硬编码成功、静态页面冒充真实功能。

验收：

- 至少一个 fixture 可走完 P0-P3。
- P1 业务主体检测能产生 finding。
- P2 测试命令能启动并产生日志。
- P3 产生真实 diff 和实现报告。

### M5：P5 self-check 与 P9 package

P5 复用：

```text
assets/scripts/run_validate.py
assets/scripts/run_acceptance.py
assets/scripts/check_required_artifacts.py
assets/scripts/check_readme_alignment.py
assets/scripts/check_local_dependency.py
assets/scripts/check_fake_impl.py
assets/scripts/check_english_only.py
```

P9 输出：

```text
handoff_summary.md
p2ro_package_manifest.json
p2ro_run_manifest.json
ready-for-qa/<batch>/<task_id>/<task_id>/
```

验收：

- P5 Blocker/High 默认阻断 P9。
- P9 使用白名单复制。
- P9 包可被当前 `scanner.Scan` 识别。
- Stage A 对有效包无结构阻断，对故意缺陷包产生预期 finding。

### M6：P10 native self-test attachment upload

模块：

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

接口：

```go
type SelfTestAttachmentClient interface {
	Exists(ctx context.Context, req AttachmentExistsRequest) (AttachmentExistsResult, error)
	List(ctx context.Context, req AttachmentListRequest) (AttachmentListResult, error)
	BatchPresignedURL(ctx context.Context, req BatchPresignedURLRequest) (BatchPresignedURLResult, error)
	UploadBytes(ctx context.Context, req UploadBytesRequest) error
	BatchConfirm(ctx context.Context, req BatchConfirmRequest) (BatchConfirmResult, error)
}
```

provider 选择：

```text
if native_http credentials configured and human confirmed:
  use NativeHTTPProvider
else:
  manual_required
```

上传顺序：

```text
1. read P9 manifest
2. materialize artifact mapping
3. filter missing optional files
4. compute file_size / file_type / sha256
5. exists/list before upload
6. batch-presigned-url
7. PUT file bytes to presigned upload_url
8. batch-confirm
9. exists/list after upload
10. write p2ro_upload_manifest.json
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

验收：

- `codex_report.md` 上传并确认成功。
- `logs/B_docker.log` 上传为 `docker_startup.log`。
- `logs/C_tests.log` 上传为 `run_tests.log`。
- `frontend_e2e_report.md` 和 `frontend_e2e_summary.json` 可作为 `ai_test_report` 上传。
- 上传失败不删除本地 artifact，不改变 P9 package。
- manifest 不保存 API key、Authorization、download_url、upload_url。

### M7：operator TUI

主视图：

```text
待生产 | 生产中/待人工 | 待质检/已完成
```

详情区：

- prompt 摘要
- 当前 P 阶段
- 流式日志
- diff 摘要
- 权限队列
- findings
- artifact 列表
- package path
- P10 上传状态

验收：

- 能新建、启动、取消作业。
- 能看到实时 P 阶段输出。
- 能批准或拒绝 require human 权限。
- 能查看 diff summary、package path、upload manifest。

### M8：兼容验证和 hardening

验证路径：

```text
p2ro P9 package -> p2r scan -> p2r run --stage A -> p2r run --from B
```

fixtures：

```text
fullstack valid package
pure_backend valid package
pure_frontend valid package
missing metadata.prompt package
fake implementation package
frontend blank page package
```

验收：

- p2r scanner 能识别 p2ro P9 包。
- Stage A 对有效包通过，对缺陷包产生预期 finding。
- B/G/C 可消费 p2ro fullstack 或 pure_frontend 包。
- G finding 可进入 p2ro 后续 repair brief。

## 并行开发分工

| Lane | 模块 | 依赖 |
| --- | --- | --- |
| A | fork hygiene, cmd, config | 无 |
| B | operator model, store, lifecycle | A |
| C | agent adapter, policy, permissions | A |
| D | workspace, package skeleton, snapshot | A |
| E | P0/P1/P2/P3 stages | B, C, D |
| F | P5/P9 self-check and package | D, E |
| G | P10 native upload | B, F |
| H | operator TUI | B, C, F, G |
| I | compatibility fixtures | F, G |

执行顺序：

```text
A
  -> B + C + D
  -> E
  -> F + G
  -> H + I
```

## 不在 MVP 范围

- 自动平台最终提交。
- 自动质检通过或打回。
- 质检员题目管理系统网页自动化。
- 多模型调度。
- 多人协作。
- P4/P6/P7/P8 默认闭环。
- Playwright trace/video 强制产出。
- `p2r_core` 抽包。

## 最终完成标准

- p2ro 独立仓可构建独立二进制。
- P0/P1/P2/P3/P5/P9/P10 可串行完成。
- P1 不写业务主体，P2 冻结可运行测试，P3 产生真实实现。
- P5 能执行 prompt2repo 交付红线。
- P9 产物能被 p2r `scanner.Scan` 和 Stage A 识别。
- P10 原生上传 `ai_test_report` 附件并生成脱敏 manifest。
- 权限策略覆盖 read/write/test/install/network/delete/git。
- TUI 使用作业端语义，不复用质检端三栏文案。
- 日志、manifest、stdout/stderr 不出现 API key、Authorization、presigned URL 明文。
- 至少 3 个 fixture 覆盖 fullstack、pure_backend、pure_frontend。
