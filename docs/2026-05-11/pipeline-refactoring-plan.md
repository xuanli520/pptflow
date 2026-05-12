# Pipeline 包重构方案（核对修订版）

> 日期：2026-05-11
> 核对范围：`internal/pipeline/` 顶层 21 个 Go 文件，`wc -l` 为 6768 行；含 `internal/pipeline/model/` 则为 23 个 Go 文件，6945 行。
> 核对依据：`wc -l`、`rg`、逐文件阅读 `pipeline.go`、`run_lifecycle.go`、`stage_*`、`codex_session.go`、`codex_app_server_session.go`、`cleanup.go`、`recovery.go`、`testhooks.go` 以及相关测试。

---

## 一、核对结论

原方案的主判断成立：`Runner` 过载、`codex_app_server_session.go` 放错包、`runState` 状态过大、D/E/F 静态审查逻辑重复、`pipeline.go` 职责混杂，都是当前代码中的真实问题。

但原方案也有几处需要修正，否则后续实现会踩坑：

| 原方案问题 | 修订结论 |
|---|---|
| 行数整体比当前 `wc -l` 多 1 行左右 | 已按当前工作区实际行数校准 |
| `runState` 写成 22 字段 | 当前实际约 27 个字段，问题比原文更重 |
| Stage 接口前后签名不一致，一处返回 `(StageRecord, error)`，一处只返回 `StageRecord` | 统一为 `StageOutcome`；Stage 内部失败用 `StageRecord` 表达，生命周期/持久化错误仍由 lifecycle 返回 error |
| D/E 配置示例把当前 Stage D 初始输出写反 | 当前 D 初始主输出是 `tests_coverage_report.md`，兼容输出是 `4_测试有效性报告_api端点真实性.md` |
| Stage F 设计成“覆盖 buildContext” | Go 没有虚方法覆盖；应使用 `ContextBuilder`/`BeforeReview` 函数注入或小接口组合 |
| “Phase 2+4 并行”会同时改 `executeStage`、stage helper 和 `pipeline.go` | 改为顺序执行，避免同一写集冲突 |
| 问题 7 提到了运行时证据和清理所有权，但实施计划没有真正解决 | 新增 `RuntimeState`/`StageOutcome`，让 B→C→cleanup 的依赖从磁盘隐式读取变成显式状态 |
| 测试命令只写 `go test ./internal/pipeline/... ./internal/codex/...` | 当前主要测试在 `tests/internal/...`，必须以 `go test ./...` 或显式包含 `./tests/internal/...` 为准 |

---

## 二、现状诊断

### 2.1 文件职责速览

| 文件 | 当前行数 | 当前职责 | 核心问题 |
|---|---:|---|---|
| `pipeline.go` | 851 | 公开类型、接口、Runner 构造、Run 入口、Stage 路由、Stage helper、崩溃/中止收尾、清单持久化、路径计算 | 杂货铺，一个文件承担多类职责 |
| `run_lifecycle.go` | 387 | `runState`、prepare、初始 artifact、preflight、stage loop、finish/abort 协调 | `runState` 是 27 字段可变状态袋，且持有 `Runner` 形成环 |
| `stage_a.go` | 513 | Stage A 结构验证、包快照、Python/uv 脚本执行、acceptance 解析 | 职责基本内聚，但强耦合 Runner/config/exec |
| `stage_b.go` | 283 | Stage B Docker/Compose 编排、端口采集、健康探测、失败证据写入 | 职责基本内聚，但创建运行时资源后的清理所有权分散 |
| `stage_c.go` | 163 | Stage C host `run_tests.sh` 执行、运行时 env 注入、截图/summary | 从磁盘读取 B 的 `port_map.json`，B→C 依赖是隐式文件契约 |
| `stage_codex.go` | 600 | Stage D/E 共享静态审查实现、prompt 构建、context 组装、静态报告收尾 helper | D/E 已参数化，但文件过长，并与 F 重复 Codex review harness |
| `stage_f.go` | 196 | Stage F 修复审查、prior findings context、repair summary、short comment | 与 `stage_codex.go` 大量重复，但又有 F 专属 artifact |
| `codex_session.go` | 264 | Codex app-server 请求/结果/接口类型、工厂、引导调度、guidance 日志 | session 协议类型和 pipeline 引导策略混装 |
| `codex_app_server_session.go` | 1257 | Codex app-server JSON-RPC 客户端、进程管理、stdout/stderr 流、delta 聚合、activity preview、日志压缩 | 最严重职责错位；应迁出 pipeline 包 |
| `finding.go` | 474 | 静态审查 JSON 合约解析、finding ID 分配、severity 排序、report normalize/truncate | 不只是解析，混入 finding 编号/排序和报告裁剪 helper |
| `artifact_io.go` | 261 | ArtifactWriter、required/best-effort 写入、artifact warning、包快照、底层文件 I/O | 基本合理，但 `copyPackageSnapshot` 可视为 Stage A 支撑能力 |
| `artifact_names.go` | 42 | QA artifact 命名规则 | 职责清晰 |
| `compose.go` | 298 | Compose 文件查找、README compose 命令解析、端口/探测辅助 | 职责清晰 |
| `runtime_evidence.go` | 180 | `port_map.json` 读取、Stage C 环境变量构建 | 工具函数清晰，但承载了隐式 B→C 文件契约 |
| `cleanup.go` | 275 | task lock、stale/current Docker 清理、cleanup summary、运行时清理点判断 | 锁、清理、stage 时序策略混在一起 |
| `recovery.go` | 177 | stale running run 恢复、stage 补失败、crash summary、lock 删除 | 包级函数绕回创建 Runner，锁释放逻辑重复 |
| `render.go` | 123 | 终端日志渲染为 PNG 截图 | 职责清晰 |
| `text_helpers.go` | 32 | 字符串截断和 `firstNonEmpty` | 职责清晰，但 `firstNonEmpty` 与 `internal/codex` 中同名 helper 重复 |
| `testhooks.go` | 368 | 测试导出、Stage/Session 探针 | session 探针暴露 app-server 内部状态机，应随 appserver 包迁移 |
| `process_unix.go` / `process_windows.go` | 24 | 平台进程存活检测 | 职责清晰 |
| `model/` | 177 | `RunRecord`、`StageRecord`、`Finding`、`ArtifactWarning`、Stage 元数据 | 职责清晰 |

### 2.2 十个主要问题

#### 问题 1：`Runner` 是 God Object

`Runner` 当前只有 3 个字段，但拥有 30+ 个方法，覆盖编排、生命周期、stage 实现、Codex 审查、Docker 清理、锁管理、artifact 持久化、恢复、路径计算、超时策略等。

每新增一个 Stage 或行为，`Runner` 都会继续变胖；更糟的是，测试往往必须构造完整 `Runner`，很难只测一个 stage 的小切面。

#### 问题 2：`codex_app_server_session.go` 不属于 pipeline

`appServerCodexReviewSession` 的主体是一个 Codex app-server JSON-RPC 客户端：

```text
appServerCodexReviewSession
├── JSON-RPC: initialize / thread/start / turn/start / turn/steer / unsupported response
├── 进程管理: exec.CommandContext / stdin / stdout / stderr / waitProcess / timeout
├── 并发流处理: readStdout / readStderr / completeStreamError
├── 消息聚合: delta 聚合、completed item 优先、finalReportLocked
├── 活动预览: commandExecution / fileChange / reasoning 等 activity preview
├── 日志压缩: compact JSON-RPC log、delta 去重、contract marker 计数
└── 线程安全: mu、writeMu、responses、done、completed
```

它当前依赖 pipeline 包内 helper 和类型：`CodexReviewRequest`、`CodexReviewResult`、`CodexDeltaUpdate`、`ArtifactWarning`、`capabilitySummary`、`writeText`、`appendText`、`newArtifactWarning`、`sha256Text`、`staticReviewMarkerCounts`、`truncateStringPrefix`、`firstNonEmpty`。迁移时必须一起处理这些依赖，不能只移动一个文件。

正确归属：`internal/codex/appserver/`。该包不应依赖 `internal/pipeline`。

#### 问题 3：`codex_session.go` 协议类型与 pipeline 策略混装

应迁移到 `internal/codex/appserver` 的内容：

```go
Session interface
Request / Result / DeltaUpdate / Warning
New(envKeys []string)
```

应保留在 `internal/pipeline` 的内容：

```go
runCodexReviewWithLog()
runCodexReviewSessionWithGuidance()
defaultCodexGuidanceDeadlines
codexGuidanceSchedule()
codexGuidanceMessageWithContract()
appendCodexGuidanceEvents()
CodexGuidanceEvent
```

原因很简单：前者回答“app-server 会话协议是什么”，后者回答“pipeline 在 D/E/F 中如何调度、注入合约提醒、记录 guidance 事件”。

#### 问题 4：`runState` 是 27 字段可变状态袋

当前 `runState` 同时持有：

```text
依赖与入口: runner, ctx, taskID, opts, progress
项目与历史: project, pathWarnings, previousRuns
运行标识: start, runID, artifactRoot, writer
资产/文档: released, releaseErr, importedDocs, docsManifest, docsImportErr, docsManifestErr
运行策略: keepRuntime
执行状态: run, stages, results, runtimeCleanupDone, cleanupFailed, runCreated, runFinished, artifactWarnings
```

问题不是字段数量本身，而是字段横跨 prepare、execute、cleanup、finish、abort/crash 多个阶段，且任何方法都能改。`runState.runner` 还形成 `Runner -> runState -> Runner` 的循环引用。

#### 问题 5：D/E/F 的 Codex review harness 重复

D/E 已由 `stageCodex()` 参数化共享，但 `stageCodex()` 本身过长；F 又复制了以下流程：

```text
读取 prompt profile
校验 codex.network == none
校验 writable_tmp == false
safeCodexExtraArgs()
codex.DetectCLI()
codex.ValidateAppServerCapability()
codexContext()
codex.NewSandbox()
runCodexReviewWithLog()
finalizeStaticReviewReport()
requiredStageText()
```

差异点不是只有 context：Stage F 还写 `repair_summary.json`、`short_comment.txt`，并使用 `annotator_fix.md` 与 F 专属报告名。共享抽象必须允许 stage 前置 artifact、context builder、输出/兼容输出策略都可配置。

#### 问题 6：`pipeline.go` 职责混杂

`pipeline.go` 当前包含公开 API、Runner 构造、Run 主流程、stage 选择和路由、stage 状态 helper、materialize skipped runtime artifact、artifact write failure finding、run manifest 写入、stage status 写入、崩溃/中止收尾、路径计算和输入校验。

其中很多函数不需要和公开 API 同文件，拆分后代码可读性会明显提升，且风险较低。

#### 问题 7：运行时证据和清理所有权是隐式的

当前时序：

```text
Stage B 写 artifactRoot/port_map.json
Stage C 从磁盘读 artifactRoot/port_map.json
cleanupCurrentRuntime 再从磁盘读 artifactRoot/port_map.json
runtimeCleanupPoint(stage, stages) 决定 B 后还是 C 后清理
```

这导致三个问题：

- B→C 的数据依赖不在类型签名里。
- B 创建 Docker 资源，cleanup 在 lifecycle 调，资源所有权跨文件分散。
- C 被跳过时 B 后清理，C 运行时 C 后清理，这个规则被藏在 `runtimeCleanupPoint()`。

#### 问题 8：缺少关键抽象

当前接口只有：

```text
runStore
CommandRunner
CodexReviewSession
```

缺失：

- Stage 执行单元：现在每个 stage 都是 Runner 方法。
- Stage 输出模型：现在只有 `StageRecord`，无法显式携带 B 产生的 runtime evidence。
- Codex review runner：D/E/F 直接串起 profile、sandbox、session、report finalize。
- 清理策略/运行时所有权：cleanup 直接读磁盘 artifact。
- Artifact sink 抽象：`ArtifactWriter` 是具体值对象，目前够用，但测试替换能力有限。

#### 问题 9：测试探针暴露错误包的内部实现

`testhooks.go` 中 `TestAppServerSessionProbe` 直接持有 `*appServerCodexReviewSession` 并调用私有方法。`tests/internal/pipeline/codex_app_server_session_internal_test.go` 实际在测 app-server session 内部状态机，而不是 pipeline。

这类测试应该随 appserver 包移动，pipeline 只保留 guidance 调度和 D/E/F 集成测试。

#### 问题 10：`RecoverStaleRuns()` 游离于 Runner

`RecoverStaleRuns(ctx, store *db.Store, cfg config.Config)` 是包级函数，却内部 `NewRunner(store, cfg)` 后调用 `runner.markRunCrashed()`。同时 `removeTaskLock()` 重复了 `taskRunLock.Release()` 的删除语义。

恢复逻辑可以保留包级入口给 CLI/TUI 调用，但实现应收敛到一个 `RecoveryService` 或 `Runner.RecoverStaleRuns()`，并复用 lock path/release helper。

---

## 三、目标架构

### 3.1 目录结构

```text
internal/pipeline/
├── model/
│   ├── model.go
│   └── stages.go
│
├── pipeline.go                 # 公开类型、Runner、NewRunner、Run 入口
├── lifecycle.go                # prepare / execute loop / finish / abort / crash
├── stage.go                    # Stage 接口、StageOutcome、registry、routing、stage helper
├── runtime_state.go            # RuntimeState、B→C evidence、cleanup target 判断
├── codex_review.go             # pipeline 对 appserver 的使用、guidance 调度、warning 转换
├── codex_context.go            # codexContext / refRunStaticContext / attached docs
│
├── stage_a.go
├── stage_b.go
├── stage_c.go
├── stage_codex_review.go       # D/E/F 共享 CodexReviewStage
├── stage_d.go                  # D 配置
├── stage_e.go                  # E 配置
├── stage_f.go                  # F 配置 + repair supplements
│
├── finding_contract.go         # static-review JSON 合约解析/normalize/truncate
├── finding_ids.go              # finding ID、severity rank、highestRisk
├── artifact_io.go
├── artifact_names.go
├── compose.go
├── cleanup.go
├── recovery.go
├── render.go
├── text_helpers.go
├── process_unix.go
├── process_windows.go
└── testhooks.go                # 移除 appserver session 探针，只保留 pipeline 探针

internal/codex/
├── cli.go
├── sandbox.go
└── appserver/
    ├── types.go                # Session / Request / Result / Update / Warning
    ├── session.go              # session 状态、New、Start、Wait、SendGuidance、complete、stop
    ├── protocol.go             # JSON-RPC message、request/notification/response
    ├── stream.go               # stdout/stderr/waitProcess
    ├── delta.go                # delta/completed item/final report/activity preview
    ├── log.go                  # compact event log、marker counting、log append
    └── testhooks.go            # appserver 专属测试探针

tests/internal/
├── pipeline/
│   ├── pipeline_test.go
│   ├── lifecycle_persistence_test.go
│   ├── stage_a_test.go
│   ├── stage_codex_test.go
│   ├── codex_review_test.go
│   └── ...
└── codex/
    └── appserver/
        ├── session_test.go
        └── session_internal_test.go
```

说明：如果坚持 white-box 访问未导出字段，appserver 内部测试应放在 `internal/codex/appserver/*_test.go` 并使用同包测试；如果延续当前 `tests/internal/...` 约定，则通过 `internal/codex/appserver/testhooks.go` 暴露有限探针。

### 3.2 Stage 核心抽象

Stage 内部业务失败不再额外返回 Go error，而是写入 `StageRecord.Status/ErrorSummary/Findings`。只有 lifecycle 持久化、store、manifest、abort/crash 这类编排错误继续由 lifecycle 返回 error。

```go
type Stage interface {
    ID() string
    Execute(ctx context.Context, sc StageContext) StageOutcome
}

type StageOutcome struct {
    Record  model.StageRecord
    Runtime *RuntimeState
}

type StageContext struct {
    Run       model.RunRecord
    Project   scanner.Project
    Options   RunOptions
    Prior     map[string]model.StageRecord
    Preflight preflight.CheckResult
    Runtime   RuntimeState
    Progress  func(RunProgress)
    Writer    ArtifactWriter
    Timeout   func(key string, fallbackSeconds int) time.Duration
}
```

`RuntimeState` 用来显式表达 B→C→cleanup 的运行时依赖：

```go
type RuntimeState struct {
    ComposeProject string
    ComposeFile    string
    WorkDir        string
    Services       []string
    Mappings       map[string][]portMapping
    Probes         []probeResult
}

func (s RuntimeState) HasCleanupTarget() bool
func (s RuntimeState) HasServiceMappings() bool
```

Stage B 写 `port_map.json` 仍然是持久化 artifact，但同时返回 `StageOutcome{Runtime: &runtime}`；Stage C 优先使用 `sc.Runtime`，不再把磁盘文件当作同一 run 内的主数据通道。恢复旧 run、stale cleanup 仍可从 artifact 读 `port_map.json`。

### 3.3 Codex appserver 包边界

`internal/codex/appserver` 不依赖 pipeline。为避免子包反向拉入 pipeline helper，`Request` 应使用原始字段，而不是 pipeline 的 `CodexReviewRequest`：

```go
type Request struct {
    Timeout           time.Duration
    ProjectPath       string
    LogPath           string
    Env               []string
    Prompt            string
    CommandPath       string
    CapabilitySummary string
    HasAppServer      bool
    Model             string
    MaxOutputBytes    int
    OnDelta           func(Update)
}

type Result struct {
    Result   executor.Result
    Warnings []Warning
}
```

pipeline 的 `codex_review.go` 负责把 `codex.Capability`、`safeCodexExtraArgs()` 和 `codex.AppServerModelFromArgs()` 适配成 appserver Request，把 `[]appserver.Warning` 转成 `[]pipeline.ArtifactWarning`，并在 guidance 调度层追加 `CodexGuidanceEvent`。

`Warning` 需要覆盖当前 `ArtifactWarning` 的字段，避免信息损失：

```go
type Warning struct {
    Path       string `json:"path"`
    Op         string `json:"op"`
    Error      string `json:"error"`
    Required   bool   `json:"required,omitempty"`
    RecordedAt string `json:"recorded_at,omitempty"`
}
```

### 3.4 产物命名规范

#### 核心原则

- 每次运行只生成**一份**文档，不生成内容相同、命名不同的兼容副本。
- 所有提交文档统一通过 `qaArtifactName()` 加 `QA_` 前缀（当前逻辑保留）。
- 打回重检文档使用 `_verification` 后缀，区分 initial 和 recheck。

#### 各 Stage 产物清单

| Stage | 模式 | 产物（写入名，自动加 `QA_` 前缀） | 说明 |
|---|---|---|---|
| A | any | `validation_report.md` | 静态验证报告 |
| A | any | `acceptance_report.md` | acceptance 报告 |
| A | any | `trajectory_archive.png` | 轨迹文件压缩包内部构造截图 |
| A | any | `acceptance.json`, `required_artifacts.json`, `readme_alignment.json`, `local_dependency.json`, `fake_impl.json`, `tests_inspection.json`, `english_only.json` | 辅助 JSON（无 QA_ 前缀） |
| B | any | `port_map.json` | Docker 端口映射（无 QA_ 前缀） |
| B | any | `docker_startup.png` | Docker 启动截图 |
| C | any | `test_runtime_summary.json` | 测试运行摘要（无 QA_ 前缀） |
| C | any | `run_tests_screenshot.png` | run_tests.sh 运行截图 |
| D | initial | `test_effectiveness_report.md` | 测试有效性报告 |
| D | recheck | `test_effectiveness_verification.md` | 测试有效性验证报告 |
| E | initial | `codex_report.md` | 质检 AI 测试报告 |
| E | recheck | `codex_report_verification.md` | 质检 AI 验证报告 |
| F | initial | `operator_prompt_requirements_verification.md` | 作业员 prompt 需求验证 |
| F | initial | `operator_codex_report_issues_verification.md` | 作业员 issues 验证 |
| F | recheck | `prompt_requirements_verification.md` | 质检员 prompt 需求验证 |
| F | recheck | `codex_report_issues_verification.md` | 质检员 issues 验证 |

> 说明：表中名称是传给 `qaArtifactPath()` 的参数，实际落盘文件名为 `QA_` + 上述名称。
> 标注"无 QA_ 前缀"的文件使用 `ArtifactWriter` 直接写入，不走 `qaArtifactPath()`。

#### 提交目录

首次质检总计 **8 份**提交内容，在 Stage F 完成后由 pipeline 统一聚合到 `result/{batch}/{task}/submit/`：

| 序号 | 实际文件名 | 来源 Stage |
|---|---|---|
| 1 | `QA_codex_report.md` | E |
| 2 | `QA_validation_report.md` | A |
| 3 | `QA_operator_prompt_requirements_verification.md`（首次）/<br>`QA_prompt_requirements_verification.md`（二次） | F |
| 4 | `QA_operator_codex_report_issues_verification.md`（首次）/<br>`QA_codex_report_issues_verification.md`（二次） | F |
| 5 | `QA_test_effectiveness_report.md` | D |
| 6 | `QA_docker_startup.png` | B |
| 7 | `QA_run_tests_screenshot.png` | C |
| 8 | `QA_trajectory_archive.png` | A |

pipeline 负责在 run 末尾将上述文件从 `artifactRoot/` 复制（或硬链接）到 submit 目录。

### 3.5 CodexReviewStage 共享实现

不要用”覆盖方法”的设计。Go 中如果 `CodexReviewStage.Execute()` 直接调用 `s.buildContext()`，嵌入它的 `StageF` 不会自动形成虚方法派发。应使用显式函数注入或小接口。

```go
type CodexStageSpec struct {
    ID           string
    Profile      string
    Output       string            // 单一路径，不再有 CompatOutputs
    RecheckOutput string           // recheck 模式下的输出路径
    BuildContext func(context.Context, StageContext) (string, error)
    BeforeReview func(*model.StageRecord, ArtifactWriter, StageContext)
    Unavailable  func(model.StageRecord, time.Time, string) model.StageRecord
}
```

各 Stage 配置（Output 是传给 `qaArtifactPath()` 的参数，落盘自动加 `QA_` 前缀）：

```go
// Stage D initial:
ID: “D”, Profile: “tests_coverage_report.md”,
Output:        “test_effectiveness_report.md”            // → QA_test_effectiveness_report.md
RecheckOutput: “test_effectiveness_verification.md”      // → QA_test_effectiveness_verification.md

// Stage E initial:
ID: “E”, Profile: “static_acceptance_audit.md”,
Output:        “codex_report.md”                         // → QA_codex_report.md
RecheckOutput: “codex_report_verification.md”            // → QA_codex_report_verification.md

// Stage F initial (首次质检):
ID: “F”, Profile: “annotator_fix.md”,
Output:        “operator_prompt_requirements_verification.md”  // → QA_operator_prompt_requirements_verification.md
BeforeReview:  write repair_summary.json (无 QA_ 前缀) + QA_operator_codex_report_issues_verification.md
BuildContext:  base codex context + priorStageSnapshot findings context

// Stage F recheck (二次质检):
RecheckOutput: “prompt_requirements_verification.md”     // → QA_prompt_requirements_verification.md
```

说明：Stage F 的 `QA_operator_codex_report_issues_verification.md` 和二次质检的 `QA_codex_report_issues_verification.md`，由 `BeforeReview` / profile 内容决定生成方式。

---

## 四、分阶段实施计划

### Phase 0：建立保护网

先不改结构，补齐基线确认：

1. 跑 `go test ./...`，记录当前基线。
2. 新增或确认现有测试覆盖：
   - Stage A 新增 `QA_trajectory_archive.png`。
   - Stage D 初始输出 `QA_test_effectiveness_report.md`，recheck 输出 `QA_test_effectiveness_verification.md`，无兼容副本。
   - Stage E 初始输出 `QA_codex_report.md`，recheck 输出 `QA_codex_report_verification.md`，无兼容副本。
   - Stage F 初始输出 `QA_operator_prompt_requirements_verification.md` + `QA_operator_codex_report_issues_verification.md`。
   - Stage F recheck 输出 `QA_prompt_requirements_verification.md` + `QA_codex_report_issues_verification.md`。
   - 提交聚合：run 末尾 `result/{batch}/{task}/submit/` 包含完整 8 份文件。
   - app-server delta compact log、completed item、closed pipe、guidance steer。
   - cancel/abort/crash 会持久化 stage status 和 cleanup summary。

### Phase 1：提取 `internal/codex/appserver`

这是收益最高且边界最清晰的拆分。

#### 1.1 新建包并迁移实现

| 新文件 | 内容 |
|---|---|
| `types.go` | `Session`、`Request`、`Result`、`Update`、`Warning` |
| `session.go` | session 结构体、`New()`、`Start()`、`Wait()`、`SendGuidance()`、`complete()`、`stop()` |
| `protocol.go` | `rpcMessage`、`rpcError`、request/notification/response 注册 |
| `stream.go` | `readStdout()`、`readStderr()`、`waitProcess()`、stream error 处理 |
| `delta.go` | delta、completed item、activity preview、final report |
| `log.go` | compact log、marker count、append log、hash/truncate helper |
| `testhooks.go` | appserver session 探针 |

#### 1.2 pipeline 侧保留编排

`codex_session.go` 重命名为 `codex_review.go`，只保留 pipeline 策略：

```go
func (r Runner) runCodexReviewWithLog(...)
func runCodexReviewSessionWithGuidance(...)
func codexGuidanceSchedule(...)
func codexGuidanceMessageWithContract(...)
func appendCodexGuidanceEvents(...)
```

#### 1.3 测试迁移

| 当前测试 | 迁移方式 |
|---|---|
| `tests/internal/pipeline/codex_app_server_session_test.go` | 拆分：纯 appserver 会话行为移动到 `tests/internal/codex/appserver/`；pipeline guidance 调度留在 `tests/internal/pipeline/codex_review_test.go` |
| `tests/internal/pipeline/codex_app_server_session_internal_test.go` | 移到 appserver 测试目录，并改用 `appserver.NewSessionProbeForTest` |
| `pipeline.NewAppServerCodexReviewSessionForTest` | 删除，替换为 appserver 包测试探针 |

验收：

```bash
rg "appServerCodexReviewSession|NewAppServerCodexReviewSessionForTest" internal/pipeline tests/internal/pipeline
go test ./...
```

第一个命令应无 pipeline appserver 实现残留；测试应通过。

### Phase 2：机械拆分 `pipeline.go`

先做低风险移动，不改变逻辑。

| 当前内容 | 迁移目标 |
|---|---|
| `RunOptions`、`RunProgress`、`Result`、`Runner`、`NewRunner`、`Run` | 保留在 `pipeline.go` |
| `selectedStages`、`initialStages`、`executeStage`、stage helper | `stage.go` |
| `crashRun`、`finishAbortedRun`、`markInFlightStageAborted` | `lifecycle.go` |
| `normalizeRunOptions`、`canonicalizeProjectForRun`、路径 warning | `preparation.go` 或 `lifecycle.go` |
| `writeRunManifest`、`writeStageStatus`、`runStatus` | `lifecycle_persist.go` |
| `stageLogPath`、`runArtifactRoot`、`SelfTestReportPath` | 保留或移动到 `paths.go` |

验收：`git diff --stat` 应主要是函数移动；`go test ./...` 通过。

### Phase 3：引入 Stage 接口与 RuntimeState

这一步解决 Runner God Object 和 B→C 隐式文件契约。

实施顺序：

1. 新建 `Stage`、`StageContext`、`StageOutcome`、`RuntimeState`。
2. 把 `stageA/B/C` 先薄封装为 `StageA/B/C`，内部可短期调用原函数，保证行为不变。
3. `executeStageLoop` 消费 `StageOutcome`：持久化 `outcome.Record`，保存/合并 `outcome.Runtime`。
4. Stage B 返回 runtime state；Stage C 读取 `sc.Runtime`，缺失时产生当前等价失败。
5. cleanup 优先使用 in-memory `RuntimeState`；stale run/recovery 仍从 artifact 读取。

验收：

```bash
go test ./tests/internal/pipeline -run 'StageC|Runtime|Cleanup|Run'
go test ./...
```

### Phase 4：合并 D/E/F Codex review harness

在 Stage 接口稳定后提取 `CodexReviewStage`。

实施要点：

1. `stage_codex_review.go` 承载 profile 读取、安全策略校验、CLI capability、sandbox、prompt、session、report finalize、artifact 写入。
2. `stage_d.go`、`stage_e.go` 只提供 `CodexStageSpec`（使用 3.4 节新名称，无 CompatOutputs）。
3. `stage_f.go` 只保留 F 专属：prior snapshot、repair supplement、prompt/issue 验证文档生成、F context builder。
4. `codex_context.go` 从 `stage_codex.go` 拆出 context 组装，避免共享 stage 文件继续膨胀。
5. `artifact_names.go` 保留 `qaArtifactName()`/`qaArtifactPath()`，仅删除 `qaArtifactCandidates()`（兼容副本查找不再需要）。
6. 更新 `refRunStaticContext` 仅查找新名称。

验收：

```bash
go test ./tests/internal/pipeline -run 'StageCodex|CodexContext|AppServer|RunPersistsRunningStage'
go test ./...
```

### Phase 5：拆解 `runState` 与恢复/清理服务

最后处理高风险生命周期重构。

建议拆成三个结构：

```go
type runPreparation struct {
    Project      scanner.Project
    PathWarnings []ProjectPathWarning
    PreviousRuns []model.RunRecord
    Released     []assets.ReleasedFile
    ImportedDocs []taskdocs.Document
    DocsManifest taskdocs.Manifest
}

type runExecution struct {
    Run                 model.RunRecord
    Stages              []model.StageRecord
    Results             map[string]model.StageRecord
    Runtime             RuntimeState
    RuntimeCleanupDone  bool
    CleanupFailed       bool
    ArtifactWarnings    []ArtifactWarning
}

type runLifecycle struct {
    ctx      context.Context
    runner   Runner
    taskID   string
    opts     RunOptions
    progress func(RunProgress)
    writer   ArtifactWriter
    start    time.Time
}
```

进一步目标：

- 移除 `runState.runner` 循环依赖，或者把 runner 依赖收敛在 `runLifecycle`。
- `RecoverStaleRuns` 保留包级入口，但内部委托 `RecoveryService`。
- `removeTaskLock` 复用统一 lock path helper，避免和 `taskRunLock.Release()` 分叉。
- cleanup 策略从 `runtimeCleanupPoint()` 改为显式：由 `RuntimeState`/cleanup coordinator 表达“B 后创建，C 后或 C skipped 后释放”的资源生命周期。

验收：

```bash
go test ./tests/internal/pipeline -run 'Recover|Abort|Crash|Lifecycle|Cleanup'
go test ./...
```

---

## 五、风险矩阵

| Phase | 风险 | 影响面 | 回滚难度 | 建议 |
|---|---|---|---|---|
| 0. 保护网 | 低 | 测试与基线 | 低 | 必做 |
| 1. 提取 appserver | 中低 | appserver session、testhooks、codex_review | 低到中 | 优先做，移动多但边界清晰 |
| 2. 机械拆分 pipeline.go | 低 | 文件组织 | 低 | 单独 commit，避免混入行为改动 |
| 3. Stage 接口 + RuntimeState | 中 | execute loop、A/B/C、cleanup | 中 | 分小步，先适配 A/B/C |
| 4. D/E/F CodexReviewStage | 中 | 静态审查 artifact 合约 | 中 | 必须锁住 artifact 名称测试 |
| 5. runState/recovery/cleanup 服务化 | 高 | 全生命周期 | 高 | 最后做，必须依赖前置测试 |

---

## 六、保持稳定的外部行为

重构期间不得改变这些行为：

| 行为 | 约束 |
|---|---|
| Stage 顺序 | 仍为 A → B → C → D → E → F |
| `--stage X` | 单 stage 仍自动包含 F（除非现有行为未来另有产品决策） |
| `--static-only` | 跳过 B/C，保留静态 stage |
| 命名原则 | 每次运行只生成一份文档，不生成内容相同命名不同的兼容副本；`qaArtifactName()` 自动加 `QA_` 前缀的逻辑保留；打回重检文档使用 `_verification` 后缀 |
| Stage A artifact | `QA_validation_report.md`、`QA_acceptance_report.md`、`QA_trajectory_archive.png`；辅助 JSON（`acceptance.json` 等）无 QA_ 前缀 |
| Stage B artifact | `port_map.json`（无 QA_ 前缀）、`logs/B_docker.log`、`QA_docker_startup.png` |
| Stage C artifact | `test_runtime_summary.json`（无 QA_ 前缀）、`logs/C_tests.log`、`QA_run_tests_screenshot.png` |
| Stage D initial | `QA_test_effectiveness_report.md` |
| Stage D recheck | `QA_test_effectiveness_verification.md` |
| Stage E initial | `QA_codex_report.md` |
| Stage E recheck | `QA_codex_report_verification.md` |
| Stage F initial | `QA_operator_prompt_requirements_verification.md`、`QA_operator_codex_report_issues_verification.md` |
| Stage F recheck | `QA_prompt_requirements_verification.md`、`QA_codex_report_issues_verification.md` |
| 提交聚合 | run 末尾将 8 份提交文件从 `artifactRoot/` 复制到 `result/{batch}/{task}/submit/`，清单见 3.4 节 |
| Codex policy | app-server、approval never、read-only sandbox、no network |
| 静态报告合约 | exactly one `p2r.static_review.v1` JSON block，marker 不变 |
| `refRunStaticContext` | 读取 ref-run 产物时仅查找新名称（不再尝试兼容旧名） |

---

## 七、不优先改动的内容

这些可以保持或只做机械搬迁：

| 文件/能力 | 处理 |
|---|---|
| `model/` | 保持稳定；除非新增 `RuntimeState` 不应污染 model |
| `artifact_names.go` | **微调**：`qaArtifactName()`/`qaArtifactPath()` 保留；仅删除 `qaArtifactCandidates()`（兼容副本查找不再需要） |
| `compose.go` | 保持，最多移动无关 helper |
| `render.go` | 保持 |
| `process_*.go` | 保持 |
| `artifact_io.go` | 先保持；`copyPackageSnapshot` 可在后续移到 Stage A 支撑文件 |
| `finding.go` | Phase 4 后再拆为 contract/ids；不在 Phase 1 中动 |
| `runtime_evidence.go` | 保留解析/环境构建函数，但数据入口改为 `RuntimeState` 优先 |
| `cleanup.go` | Phase 3/5 分步改，避免和 appserver 提取混在一起 |
| `recovery.go` | Phase 5 处理，不在早期混改 |

---

## 八、执行建议

推荐顺序：

1. Phase 0 建保护网。
2. Phase 1 提取 appserver，先移出最大职责错位。
3. Phase 2 机械拆 `pipeline.go`，降低后续 diff 噪音。
4. Phase 3 引入 Stage/RuntimeState，解决 Runner 和 B→C 隐式耦合。
5. Phase 4 合并 D/E/F review harness。
6. Phase 5 最后处理 lifecycle 状态机、recovery、cleanup 服务化。

每个 Phase 独立 commit。不要把“文件移动”和“行为改动”放在同一个 commit；尤其 Phase 2 必须保持机械移动，Phase 3 才引入新抽象。

最终验收：

```bash
go test ./...
rg "appServerCodexReviewSession" internal/pipeline
rg "NewAppServerCodexReviewSessionForTest|TestAppServerSessionProbe" internal/pipeline tests/internal/pipeline
```

后两个 `rg` 在最终状态下应无输出。
