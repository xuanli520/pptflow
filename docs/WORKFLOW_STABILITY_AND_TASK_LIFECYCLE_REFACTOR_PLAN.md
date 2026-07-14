# Harbor Factory 运行稳定性与题目生命周期重构方案

> 状态：Implementation in progress（确认决策具有约束力）
> 日期：2026-07-13
> 范围：Workflow Engine、Runner、Agent Runtime、ArtifactStore、Task Store、Scheduler、CLI/TUI 以及 Harbor 题目完整生命周期
>
> 实施优先级：[`WORKFLOW_STABILITY_DECISIONS.md`](./WORKFLOW_STABILITY_DECISIONS.md) 是本方案的确认决策记录。本文的历史迁移推理与确认决策冲突时，以确认决策为准；尤其是 CLI/TUI 的 hard cutover 不保留旧命令或 mutation alias。
>
> 当前交付的 Delivery 边界：Release 仅表示 Harbor Flow 受管目录中的不可变本地 package。系统生成并记录 package、source digest、evidence digest 与 immutable receipt，可按 receipt 和 digest 对账；绝不上传、复制到外部目的地、调用远端发布 provider 或执行远端 publish。本文下文保留的 `publish` 若指旧系统或历史推理，不构成当前行为；若指 Task 的 `published` 生命周期状态，也不表示远端发布。

## 1. 执行摘要

本次 `tower-http-request-decompression-limit-retry-2` 失败不是单一模型超时，而是现有系统缺少统一执行预算、阶段 checkpoint、稳定题目身份和版本化产物血缘后的集中表现：

- `generate_task_files` 的 Agent turn 配置为 1800 秒，但节点外层 attempt 固定为 600 秒；
- 一个节点内部固定执行 2 至 3 个 turn，却没有父子预算组合校验；
- 三次 Engine retry 都重新打开 conversation，从 turn 1 开始，前一轮进度和日志被覆盖；
- 节点失败时已经产生的日志或报告不会可靠进入 `NodeRun` 和 `RunResult`；
- 当前所谓“重跑”主要是复制 workspace 配置，“返修”主要是原地修改 `task_dir`；
- Task、TaskRevision、Run、StageAttempt、RepairSession、Release 和 Workspace 没有被建模为独立实体；
- 流程拓扑、阶段路径、timeout、attempt、Gate 行为、模型槽位和失效边界大量硬编码。

因此本方案不以“把 600 改成 1800”为目标，而是建立以下 V2 架构：

```text
Task
  └─ TaskRevision（不可变，绑定 task digest）
       ├─ WorkflowRun（冻结 workflow/profile）
       │    └─ RunAttempt
       │         └─ StageAttempt
       │              └─ NodeAttempt / TurnCheckpoint / Artifacts
       ├─ ReviewRequest / ReviewDecision
       ├─ RepairSession
       └─ Release

Workspace = 临时 checkout / 执行环境，不再充当 Task 或 Run 身份
```

V2 必须实现：

1. 统一且可验证的 turn、attempt、stage、run 预算模型；
2. 任意阶段可计划、预览和独立重跑；
3. 用户统一通过“继续处理”表达目标，由 Planner 区分技术重试、阶段重算和内容修订；
4. 内容发生变化时生成新的 TaskRevision，不原地污染已验证版本；
5. 所有证据绑定 TaskRevision、输入指纹、workflow 版本和 attempt；
6. Task Hub 管理出题、导入、创建、修改、克隆、运行、返修、本地打包、归档、删除和恢复；
7. CLI 与 TUI 共用同一个 application service，不各自实现生命周期规则。

## 2. 本次事故复盘

### 2.1 确定事实

故障工作区：

```text
.harbor-factory/workspaces/tower-http-request-decompression-limit-retry-2
```

`run_options.json` 配置 `agent_timeout=1800`，但 `generate_task_files` 在 `buildWorkflowDefinition` 中固定使用 600 秒 NodePolicy。Engine 用这 600 秒 context 包住整个插件调用。

生成插件不是单次调用，而是：

```text
draft
  → self-review
  → correction（仅校验失败时）
```

实际事件为：

| Attempt | 时间 | 结果 |
|---|---:|---|
| 1 | 约 600 秒 | turn 1 `context deadline exceeded` |
| 2 | 约 600 秒 | turn 2 `context deadline exceeded` |
| 3 | 约 600 秒 | 重新从 turn 1 开始并超时 |

第三次 attempt 已经输出约 21 KB JSON，但 conversation 没有完成，`task_files.json` 未发布，下游全部 `skipped`。

### 2.2 根因树

```text
表面原因：generate_task_files 超时
  ├─ 父节点 attempt_timeout=600
  ├─ 子 turn_timeout=1800
  ├─ required_turns>=2
  ├─ 没有预算组合校验
  ├─ retry 总是重建 conversation
  ├─ 没有 draft/self-review checkpoint
  ├─ attempt 共用并覆盖同一个 agent.log
  └─ timeout 后没有可恢复 candidate artifact
```

### 2.3 为什么简单增大常量不够

即使把节点 timeout 改为 1800 秒，仍然无法保证两个 1800 秒 turn 和收尾持久化完成。与此同时，Quality、TaskRepair、RuntimeSelfCheck 和 Harbor 也存在父子预算倒挂、外层 retry 与内部 retry 相乘等问题。

正确修复必须先定义预算层级和组合不变量，再由 profile 编译出每个节点的最终预算。

## 3. 当前架构缺陷

### 3.1 运行稳定性

#### 3.1.1 没有统一预算模型

当前 timeout 分散在 CLI、TUI、WorkflowDefinition、领域函数和 Runtime 中：

| 位置 | 当前值/行为 | 问题 |
|---|---|---|
| CLI Agent | 默认 600 秒 | 与节点固定 timeout 无关联 |
| RepoAnalyze/TaskDesign/TaskFiles | 外层 600 秒 | 多 turn 共享一个 600 秒父 context |
| Quality | 外层 300 秒 | 内层 Agent 可配置 1800 秒 |
| TaskRepair | 外层 600 秒 | 内层仍读取 AgentTimeout |
| RuntimeSelfCheck | 外层 1800 秒 | 内层最低 1800 秒，无收尾余量 |
| Human Gate | 86400 秒 | 长等待被建模为执行 timeout，而非 waiting 状态 |
| Harbor | 7200 秒 | 会与错误的外层三次 retry 相乘 |
| Codex turn fallback | 5 分钟 | 又一套默认值 |

系统没有验证：

```text
attempt_timeout >= required_turns * turn_timeout + grace
node_total_timeout >= attempts * attempt_timeout + backoff
child_timeout <= parent_remaining_budget
```

#### 3.1.2 retry 配置被覆盖

`chain(... attempts int ...)` 虽然接收每节点 attempts，但所有非 Gate 都被覆盖为全局 3。结果是：

- 声明 1 次的 mutation、publish、package、Harbor 节点实际可能跑 3 次；
- 声明 2 次的 similarity 实际跑 3 次；
- 普通 `unknown` 错误可能被盲重试；
- 外层 Engine retry 与插件内部 API/install/trial retry 相乘。

#### 3.1.3 checkpoint 粒度只到节点

当前 checkpoint 能表达 active node/attempt，但不能表达：

- conversation/thread；
- draft/self-review/correction 子步骤；
- 已验证 candidate；
- 幂等键和副作用状态；
- 剩余 retry/elapsed budget；
- `in_doubt` 或需要 reconcile 的 attempt。

所以崩溃后只能整节点重跑。

#### 3.1.4 失败证据从状态机丢失

插件可返回 `NodeResult{Artifacts: refs}, err`，但 Engine 仅在 `err == nil` 时吸收 artifacts。最终形成：

```text
ArtifactStore/磁盘：存在失败报告
RunResult/NodeRun/TUI：没有报告引用
```

这会破坏失败诊断、自动返修和恢复规划。

#### 3.1.5 运行定义没有冻结

恢复时重新调用当前代码的 `buildWorkflowDefinition`，而 `RunResult` 只保存 WorkflowID，不保存：

- 完整 WorkflowDefinition；
- profile/version；
- node config hash；
- template/prompt fingerprint；
- 程序版本；
- evaluator policy。

代码、模型、timeout、阈值或磁盘报告变化后，恢复可能在不同 DAG 上复用旧节点。

### 3.2 阶段重跑能力

当前 manual retry 只支持 failed/canceled/skipped 节点，并向下失效依赖闭包。它不能：

- 主动重跑成功节点；
- 选择一个阶段组；
- 指定重跑截止阶段；
- 分别重跑 Quality、Similarity、Qwen 或 Opus；
- 修改 profile 后预览失效范围；
- 解释每个保留/失效 artifact 的原因；
- 在重跑失败后保留旧的成功 artifact。

阶段目前不是实体，只是固定 Node ID、固定顺序和 `phase1/phase2/phase3` 路径约定。

### 3.3 检查 verdict 与执行状态混淆

Quality、Similarity、Docker、Initial 和 Oracle 将“检查执行成功但不通过”返回为 NodeError。Engine 随后跳过 FinalReview，返修控制器不可达。

正确模型必须拆分：

```text
execution_status = completed | infra_failed | canceled
verdict          = pass | needs_repair | reject | advisory
```

只有无法得到可信报告的技术错误才是 `infra_failed`。报告成功生成但发现问题时，节点应为 `completed + needs_repair`。

### 3.4 返修不是事务

当前 HumanGate/TaskRepair 直接原地修改 taskDir，之后才校验 revision 上限、持久化 directive 和失效旧证据。风险包括：

- 进程崩溃后任务已变但旧证据仍有效；
- 重试时重复执行 Agent；
- 达到 revision 上限后仍可能已经修改文件；
- 两个 workspace 可同时返修同一个外部 taskDir；
- 返修 Agent 没收到结构化 machine findings；
- FinalReview 固定从 lint 重跑，Docker/Oracle 可能继续使用旧 digest。

### 3.5 当前系统不是题目生命周期管理器

SQLite v1 只有：

```text
tasks(task_dir UNIQUE, metadata...)
runs(workspace_path UNIQUE, task_id, status...)
```

这实际上是“工作区运行历史索引”，不是题目管理模型：

- Task 身份等于目录路径；
- 生成模式 TaskDir 为空时回退为 workspace；
- 同一道生成题每次重跑都会成为新 Task；
- Task 不能脱离 Run 存在；
- 同一 workspace 的新 run 会覆盖旧 DB 行；
- 没有 TaskRevision、Release、RepairSession、Review 或 AuditEvent；
- Hub 平铺展示 Run，而不是按 Task 聚合；
- Delete 直接 `RemoveAll` workspace；
- Store.DeleteTask 只删 DB，文件语义相反；
- Scheduler 队列只在内存，重启即丢失。

### 3.6 `Ctrl+X` 取消与恢复生命周期不完整

当前取消不是 durable domain command，只是从 TUI 向内存 context 传播：

- 全局 `Ctrl+X` 和单键 `x` 都打开“取消整个运行”，但日志页单键 `x` 又定义为直接取消选中的 Qwen/Opus stage；全局路由优先使后者正常路径不可达，语义既冲突又不可预测；
- `q/Ctrl+C` 退出也会调用 `cancelRun`，把“关闭界面”“脱离后台任务”“取消任务”混成同一动作；
- `cancelRun` 若找不到当前 workspace 的 scheduler job，会取消 TUI 共享 root context，可能连带取消本进程其他任务；
- TaskScheduler 只在内存把 job 标成 canceled，没有 CancelRequest、actor、reason、scope、ack 或 outbox event；
- terminal job 仍占用 `workspace` reservation，同 workspace 后续提交会被永久拒绝；
- Engine 把所有 `context.Canceled` 都归为 canceled，无法区分用户请求、父进程退出、worker lease 丢失和未知中断；
- 同一次取消可能被 Engine 写成 `cancelled`、Runner 覆盖成 `failed`、Scheduler 投影为 `canceled`；
- Unix executor 会先 SIGTERM、10 秒后 SIGKILL，但没有 termination receipt，无法确认外部提交/发布等副作用是否已发生；
- package、publish、repair 及部分 repo 命令没有统一接受可停止 runtime contract，取消可能留下已替换目标、半成品 candidate 或仍存活的子进程；
- checkpoint 虽使用 `context.WithoutCancel` 尝试落盘，但只保存节点结果，不保存 cancel intent、传播进度、runtime ack 和 quota 释放状态；
- canceled run 会写 terminal FinishedAt，因此不会进入现有自动 resume；后续 manual retry 又会重新编译当前 DAG 并破坏性失效 artifact。
- 取消确认默认焦点为“是”，确认后只显示“已请求取消”toast，没有 durable acknowledgement 或最终影响报告。

结果是 UI 显示“已请求取消”后，系统无法可靠回答：取消是否真正完成、哪个层级被取消、已消耗配额是否结算、candidate 是否污染、外部副作用是否需要 reconcile，以及应该从哪里继续。

## 4. 硬编码清单与治理原则

### 4.1 必须移出业务分支的硬编码

| 类别 | 当前硬编码 | 目标位置 |
|---|---|---|
| Workflow 拓扑 | 250 行 `buildWorkflowDefinition` | versioned WorkflowTemplate |
| Node 顺序 | `nodes.Order()` 固定列表 | StageCatalog/compiled DAG |
| timeout | 60/120/300/600/1800/7200/86400 | ExecutionPolicy |
| attempts | 全局覆盖为 3 | per-node RetryPolicy |
| revision | MaxRevisions=5 且静默截断 | RepairPolicy/RunPolicy |
| Gate 行为 | `final_review` 特判 | GatePolicy capability |
| restart 边界 | 裸 `restart_from` Node ID | InvalidationPlanner |
| phase 路径 | phase1/phase2/phase3 拼接 | ArtifactCatalog |
| evaluator | Qwen/Opus switch | EvaluatorProfile[] |
| trial count | 多处固定 4 | EvaluationPolicy |
| workspace | 默认路径和生成 task 路径 | WorkspaceService |
| clone reuse | 四个布尔开关 | ReusePlan + fingerprint |
| deletion | `os.RemoveAll` | LifecycleService + TrashPolicy |
| TUI action | 按 GateID 判断快捷键 | backend capabilities |

### 4.2 不应错误配置化的领域不变量

以下内容仍可作为代码中的版本化领域策略，而不是任意用户输入：

- Harbor 标准任务文件白名单；
- CodeEdge 提交所需字段；
- 当前正式 pass@4 规则；
- secret redaction 和路径逃逸规则；
- immutable artifact 和 digest 校验规则。

关键是只保留一个权威来源，并为策略加版本号和 fingerprint。

## 5. V2 目标架构

### 5.1 分层

```text
CLI / TUI
   ↓
Application Services
   ├─ TaskService
   ├─ RevisionService
   ├─ RunService
   ├─ ExecutionControlService
   ├─ TaskContinuationService
   ├─ BudgetGrantService
   ├─ ReviewService
   ├─ ReleaseService
   └─ DeletionService
   ↓
Control Plane Store（身份、状态、命令、审计）
   ↓
Workflow Runtime
   ├─ TemplateCompiler
   ├─ StageScheduler
   ├─ BudgetManager
   ├─ InvalidationPlanner
   ├─ OutcomeAggregator
   └─ RecoveryReconciler
   ↓
Runtimes / Plugins / Artifact Object Store
```

### 5.2 权威数据边界

V2 采用混合权威模型：

- SQLite 是 Task/Revision/Run/Stage/Command/Lifecycle 状态的权威控制面；
- 文件系统或内容寻址对象目录是 immutable artifact 内容的权威存储；
- `state.json`、`run_result.json` 仅可作为受管状态的导出投影，不存在兼容 reader；
- V1 workspace scan、import 和 reconcile 已退休；正常生命周期身份只由受管 V2 control plane 创建；
- 新数据库从 V2-only baseline 初始化，不创建 V1 `tasks`/`runs` 表或 schema-version 1。含任一 V1 表或版本 1 历史的数据库在 `Open` 与 `OpenReadOnly` 均被拒绝；系统不读取其中记录、不导入、不迁移、不删除也不转换，只能恢复经过验证的 pure-V2 backup 或新建 control-plane root。

### 5.3 可复用 Workflow Kernel 与 Harbor Domain 边界

统一执行预算、运行/阶段状态机、checkpoint、取消恢复、continuation、artifact lineage、Job/Lease 和 quota enforcement 都是任何 AI workflow 可复用的通用能力，应下沉为独立 Workflow Kernel。Kernel 不能导入 `internal/harbor/*`，也不能认识 Qwen、Opus、pass@4、TaskRepair 或 FinalReview。

```text
Harbor Factory Application
  ├─ Task/Revision/Review/Repair/Release lifecycle
  ├─ Harbor StageCatalog + resource vocabulary
  ├─ pass@4 / evaluator / local package policies
  └─ ChangeProvider + SubjectRevision adapter
                 ↓ typed contracts
Reusable Workflow Kernel
  ├─ DefinitionCompiler + generic DAG
  ├─ Execution/Attempt state machines
  ├─ BudgetManager + QuotaManager + AdmissionController
  ├─ ContinuationPlanner + InvalidationPlanner
  ├─ CancellationCoordinator + RecoveryReconciler
  ├─ Artifact lineage + checkpoint
  └─ Durable Job/Lease/Outbox runtime
```

| 应进入通用 Kernel | 应留在 Harbor 领域层 |
|---|---|
| run/stage/node/turn 的追加式状态机 | Task 的 draft/ready/published 生命周期 |
| execution status 与 verdict 双通道 | 哪些 finding 表示 `needs_repair` |
| timeout、attempt、elapsed、token、并发预算 | pass@4 必须恰好 4 次的正式规则 |
| 多维 quota 申请、预留、结算和释放 | 每个 evaluator/repair stage 申请多少 quota |
| canceled/interrupted/in_doubt 与 reconcile 协议 | 哪些 Harbor side effect 可 reconcile |
| effect/read/write、fingerprint 和失效闭包算法 | `task.instruction` 等资源键和 StageCatalog |
| immutable artifact、plan、checkpoint、event | TaskRevision、Release 和 ReviewDecision schema |
| generic subject revision binding | Harbor RevisionCandidate 的生成和切换规则 |

Kernel 接收冻结后的 typed descriptors，不接收 Harbor 的 `map[string]any`。`pkg/workflowkit` 只校验并执行通用 Workflow/ExecutionPlan；Harbor 的实际 stage operation、provider、checkout、runtime 和 secret reference 必须预先冻结在 typed `RunExecutionSpec` 中。不存在按旧 stage 名、Runner、动态 payload 或兼容分支回退的执行路径：

```go
type SubjectBinding struct {
    SubjectID   string
    RevisionID  string
    Digest      string
}

type DomainBindings interface {
    CompileStages(context.Context, WorkflowRequest) ([]StageDefinition, error)
    ResolveSubject(context.Context, string) (SubjectBinding, error)
    PrepareChange(context.Context, ChangeRequest) (MutationReceipt, error)
    AggregateVerdict(context.Context, []StageOutcome) (Verdict, error)
}
```

架构约束：已删除的 `internal/workflow` 不得复活；`pkg/workflowkit` 不得导入 `internal/harbor`。Harbor adapter 可以依赖 Kernel。新增非 Harbor AI workflow 时，只实现自己的 typed binding、operation registry 和 policy bundle，不复制 Runner、timeout、取消、重跑和 scheduler 逻辑。

### 5.4 包与模块演进

本轮已经直接交付公开 `pkg/workflowkit`，并删除 `internal/workflow`。持久化、进程和 SQLite adapter 保留在 `internal/workflowruntime`；需要独立发布时再拆为单独 Go module，但不得重新引入旧 Engine 或 Harbor 领域依赖。

Kernel public package 只依赖标准库和小型接口，不依赖 Cobra、Bubble Tea、SQLite schema、具体 Agent SDK 或本地路径。默认 runtime adapters 可以替换，因此其他 AI workflow 可选择内存、Postgres、Kubernetes Job 或远程 artifact store。

## 6. V2 领域模型

### 6.1 Task

```text
task_id             UUID/ULID，稳定身份
slug                用户可读名称，不作为主键
title/metadata       语言、类型、应用、AHT、标签等
source_repo/commit   来源
lifecycle_state      draft/ready/published/archived/deleted
current_revision_id
created_at/updated_at/deleted_at
version              乐观锁版本
```

### 6.2 TaskRevision

```text
revision_id
task_id
version_number
parent_revision_id
origin               generated/imported/manual/repair/fork/rollback
task_digest
proposal_digest
manifest_id
state                sealed/validated/released/superseded
change_summary
created_by/created_at
```

规则：

- RevisionCandidate 是可变草稿，TaskRevision 从创建起不可变；
- sealed 以后不可原地修改；
- 编辑、返修、回滚都创建子 revision；
- Run 只针对 sealed revision；
- current revision 切换使用 expected version 乐观锁。

### 6.3 Workspace

```text
workspace_id
root_uri
purpose              generation/checkout/validation/repair/run
task_id/revision_id/run_id
lease_owner/expires_at
state                active/released/trash/purged
```

Workspace 是可清理执行环境，不再决定 Task 身份。

### 6.4 Run 与执行记录

```text
WorkflowRun
  run_id
  task_id/revision_id
  workflow_template_id/version
  resolved_profile_hash
  definition_hash
  parent_run_id
  trigger              create/continue/verify/package
  execution_epoch      每次人工“继续处理”成功提交后递增
  status

RunAttempt
  attempt_id/run_id/ordinal
  trigger/resume_from/status

StageAttempt
  stage_attempt_id
  retry_of_stage_attempt_id
  stage_key/group/ordinal
  input_fingerprint
  execution_status
  verdict
  timeout/retry snapshot
  artifact_manifest_id
  error/failure_class

NodeAttempt / TurnCheckpoint
  node_id/generation/attempt/turn/substep
  idempotency_key
  started_at/finished_at
  candidate artifact/validation result
```

### 6.5 Review、Repair、Release

```text
ReviewRequest
  revision_id + evidence_manifest_digest

ReviewDecision
  approve | request_changes | reject_terminal
  expected_revision_digest

RepairSession
  input_revision_id
  candidate_revision_id
  findings_refs
  round/max_rounds
  status
  invalidation_plan
  last_error

Release
  release_id/version/channel
  revision_id/task_digest
  package artifact refs
  package_created/withdrawn timestamps
```

## 7. 状态机

### 7.1 Task

```text
draft
  → ready
  → published
  → archived

review/validation request_changes
  → 新 TaskRevision
  → ready

任意非活动状态
  → deleted(tombstone)
  → restore 或 retention 后 purge
```

`reviewing/validating/repairing` 是由活跃 ReviewRequest、WorkflowRun 和 RevisionCandidate 派生的 activity status，不写入 Task lifecycle_state，避免进程活动与题目生命周期互相覆盖。

### 7.2 Run

```text
queued
  → running
  → waiting_review | waiting_continuation
  → succeeded | failed_recoverable | failed_terminal | canceled

failed_recoverable
  → continuation_planning
  → continuation_ready
  → queued/running
```

失败运行是 actionable terminal，不应默认变成只读死状态。`retry_attempt`、`recompute_stage`、`revise_task` 只作为 ContinuationPlan 的内部策略分类和审计字段，不再成为三套用户入口或三套 Engine 分支。

### 7.3 StageAttempt

```text
queued → running
  ├─ completed(pass)
  ├─ completed(needs_repair)
  ├─ infra_failed(retryable)
  ├─ infra_failed(terminal)
  ├─ interrupted
  ├─ in_doubt → reconciling → resolved terminal
  └─ canceled
```

### 7.4 通用执行控制状态机

“暂停”“取消阶段”“终止运行”和“进程意外中断”必须是不同状态迁移：

```text
queued
  → cancel_requested → canceled                 # 从队列移除，未启动

running
  → pause_requested → pausing → paused          # checkpoint 后可恢复同一 Run
paused
  → resume_requested → running

stage_running
  → cancel_requested → canceling → stage_canceled
  → Run 继续调度不依赖该 stage 的分支

running
  → stop_requested → canceling → canceled       # Run 终态

worker/进程/lease 意外丢失
  → interrupted                                  # 不是用户 canceled

外部副作用结果未知
  → in_doubt → reconciling
             ├─ completed
             ├─ canceled
             └─ failed_recoverable
```

所有 terminal StageAttempt/Run 都是追加式不可变记录。`paused` 恢复同一 Run 和 checkpoint；`canceled` 后继续处理必须创建新的 ContinuationExecution/StageAttempt，并保留 parent lineage；`interrupted/in_doubt` 必须先由 RecoveryReconciler 诊断，不能伪装成用户取消。

## 8. 统一执行预算

### 8.1 Budget schema

```go
type ExecutionBudget struct {
    TurnTimeout        time.Duration
    MaxTurns           int
    AttemptTimeout     time.Duration
    MaxAttempts        int
    MaxElapsed         time.Duration
    IdleTimeout        time.Duration
    StartupGrace       time.Duration
    ShutdownGrace      time.Duration
    Backoff            BackoffPolicy
}
```

`ExecutionProfile` 还必须声明版本化的 `continuation_plan_ttl`。本轮确认值固定为 `24h`；它不是 CLI/TUI/API 的可选 override，不存在运行时默认值。编译后的 profile fingerprint 和每个 `run-manifest.json` 都冻结该值，`TaskContinuationService` 仅从目标 Run 的 frozen manifest 计算 `ExpiresAt`。同一 command 的 idempotent replay 返回原计划，绝不按重放时间续期。

### 8.2 编译期校验

Workflow 启动前必须验证：

```text
AttemptTimeout >= MaxTurns * TurnTimeout + StartupGrace + ShutdownGrace
MaxElapsed >= MaxAttempts * AttemptTimeout + MaxBackoffTotal
所有插件声明的子预算 <= 父预算
```

若配置不合法，启动前返回明确错误，禁止运行十分钟后才偶然超时。

### 8.3 策略分类

| Node 类型 | 外层 retry | 恢复策略 |
|---|---:|---|
| deterministic pure | 1-2，仅 transient I/O | 整节点重跑 |
| Agent generation | 1-2 | 从最近 turn checkpoint 继续 |
| mutation/repair | 默认 1 | 幂等 reconcile，禁止盲重试 |
| Docker verify | 单一预算来源 | 保留命令证据后重跑 |
| Harbor evaluate | 外层 1 | 内部管理 install/API/trial retry |
| Gate | 无执行 deadline | durable waiting + SLA 提醒 |

### 8.4 Durable quota 与 admission control

Budget 表示一次执行允许使用的时间/次数上限；Quota 表示跨 resume/continuation 仍需累计的资源约束。两者都由通用 Kernel 管理 durable ledger，不能在每次 Engine invocation 时把局部循环重新置零。

```go
type QuotaRequest struct {
    Dimension     string // stage_attempt, agent_turn, token, wall_time_ms, trial, repair_round, api_call, concurrency_slot
    Units         int64
    Scope         ResourceScope
    ReclaimPolicy ReclaimPolicy
}

type QuotaLease struct {
    LeaseID       string
    Dimension     string
    Reserved      int64
    Consumed      int64
    Released      int64
    FencingToken  uint64
    ExpiresAt     time.Time
}

type BudgetGrant struct {
    GrantID        string
    Scope          ResourceScope
    Dimension      string
    Delta          int64
    ExpectedVersion int64
    Actor          string
    Reason         string
}
```

通用 Kernel 负责：admission、原子预留、事件驱动计量、未使用额度释放、超限阻止、grant 幂等和审计。Harbor policy 负责声明：pass@4 需要 4 个 trial、每个 evaluator 的并发上限、repair round 上限以及哪些 actor 可以补充额度。

配额规则：

- 已发生的 API 调用、token、trial 和 elapsed cost 永不因暂停、取消、clone 或新 Run 而回滚；
- paused 恢复 checkpoint 时不自动增加 stage attempt；无法恢复必须新建 attempt 时，plan 显示并预留 `+1`；
- 取消释放未使用 reservation 和 concurrency slot，但实际 consumed 保留；
- quota exhausted 时 ContinuationPlan 为不可执行，必须缩小范围、等待额度恢复或提交 BudgetGrant；
- BudgetGrant 与业务 command 分离，必须包含 actor、reason、delta、expected version 和 idempotency key；
- 外部 provider 限流可通过 quota adapter 映射为共享 bucket，不在每个 plugin 内各写 sleep/retry。

Harbor 当前把三个不同概念都叫 attempt，必须拆开：

```text
StageAttempt     workflow stage 的一次执行
TrialExecution   pass@4 中一个逻辑独立样本
TrialAttempt     同一 trial 因技术故障产生的 retry
```

所有非 Gate node 被强制三次外层 attempt，而 Harbor stage 内又运行 4 个 trial 并各自允许 infra retry，最坏消耗近似 `3 × 4 × (1 + infra_retries)`，既突破配额，也可能改变正式样本集合。近期止血是 Harbor stage 外层 `MaxAttempts=1`；长期由 HarborEvaluationPolicy 编译 4 个 WorkItem，Kernel 负责 fan-out、quota、取消和 checkpoint：

```go
type WorkItemPlan struct {
    LogicalItemID    string
    InputFingerprint string
    RetryPolicy      RetryPolicy
    ResourceClaims   []QuotaRequest
}
```

Kernel 不知道“4”的含义，只保证 WorkItem 的 logical identity、资源结算和 retry 不新增业务样本；Harbor policy 负责恰好四个独立 trial、模型/agent/task digest 绑定和 pass@4 聚合。

## 9. Generation 多回合 checkpoint

把黑盒 `runJSONConversation` 拆成 durable substeps：

```text
draft
  → persist candidate.json + validation.json
self_review
  → persist review.json
correction（按需）
  → persist corrected_candidate.json
publish
  → canonical artifact
```

每步保存：

- input digests；
- prompt/template/model/runtime fingerprints；
- candidate output；
- validation errors；
- attempt/turn 编号；
- conversation metadata；
- started/finished/interrupted 状态。

日志不得覆盖：

```text
runs/<run-id>/stages/<stage>/nodes/<node>/attempt-002/turn-001/agent.log
```

同进程优先继续原 conversation；跨进程恢复使用已保存 candidate 启动新 conversation，不依赖临时 app-server thread 一定可恢复。

## 10. 阶段目录与统一 Continuation 协议

### 10.1 StageCatalog

建议阶段组：

| Stage | 典型节点 |
|---|---|
| source_prepare | repo_prepare |
| task_analysis | repo_analyze |
| task_design | task_design、task_review |
| task_generation | task_files、instruction、task.toml、Dockerfile、solve、tests、materialize |
| runtime_verify | self_check、Docker、initial、oracle |
| quality | lint、quality |
| similarity | similarity |
| final_review | final review / repair decision |
| evaluation | evaluator profiles，如 Qwen、Opus |
| submission | result review、submission lint |
| delivery | local package、receipt reconcile、release |

每个 StageDefinition 声明：

```text
stage_key/version/group
dependencies
input_specs/output_specs
read_sets/write_sets
execution_policy
verdict_policy
retry_policy
cache/reuse policy
invalidation policy
capabilities: cancel/continue/approve
```

### 10.2 一个用户动作，三种内部策略

面向用户只提供一个动作：`继续处理 / ContinueProcessing`。用户描述“希望系统继续达到什么结果”，可以附带目标阶段、反馈或配置变更；用户不需要先判断这是 retry、rerun 还是 repair。

内部仍严格区分三种策略，因为它们具有不同的一致性边界：

| Planner 诊断 | 内部策略 | TaskRevision | 执行记录 |
|---|---|---|---|
| timeout、网络、限流、进程中断，且输入未变 | `retry_attempt` | 不变 | 追加 StageAttempt；同一 node generation 增加 NodeAttempt，并优先恢复 checkpoint |
| 用户要求重新计算成功/失败阶段，且题目内容不变 | `recompute` | 不变 | execution epoch 和受影响 node generation 递增 |
| findings、人工反馈、patch，或选中的 stage 声明为内容生产/修改 | `revise_task` | 先创建隔离 RevisionCandidate；digest 变化后才提交新 revision | 新 revision 上创建受影响 generation |

这三种名称只出现在计划解释、审计事件和指标中。Engine 不接收 `mode=retry|rerun|repair`，也不能按这些用户动词分支；它只执行冻结后的 node transition。

编号语义必须分离：

```text
execution_epoch   每次成功提交 ContinuationPlan 递增
task_revision     仅题目内容 digest 改变时递增
node_generation   节点被要求重新计算时递增
stage_attempt     每次阶段执行都追加，终态记录永不重开或覆盖
node_attempt      运行中自动 retry，或新 StageAttempt 内的节点执行次数
```

运行中的自动 retry 可以在同一 StageAttempt 内增加 NodeAttempt；terminal StageAttempt 后的人工“继续处理”必须追加 StageAttempt。两者都不能覆盖旧记录。

### 10.3 三层命令模型

不能用一个包含 mode、feedback、失效节点和执行细节的 god object 取代现有分支。统一协议拆成三层：

```text
ContinueTaskCommand（用户意图）
  → PreparedChange / ChangeSet（已规范化的内容变化）
  → ContinuationPlan（精确、不可变、可执行）
```

```go
type ContinueTaskCommand struct {
    CommandID         string // 客户端幂等键
    TaskID            string
    RunID             string
    Expected          CheckpointRef
    Target            TargetSelector
    Change            *TaskChangeRequest
    Policy            ContinuationPolicy
    BudgetOverrides   *BudgetOverrides
    Actor              string
    Reason             string
}

type CheckpointRef struct {
    Sequence          uint64
    ExecutionEpoch    int
    TaskVersion       int64
    TaskRevisionID    string
    TaskDigest        string
    WorkflowDigest    string
}

type TargetSelector struct {
    StageIDs          []string
    NodeIDs           []string
    ResolveBlockers   bool
}

type TaskChangeRequest struct {
    ProviderID        string
    OperationKey      string
    Payload           ArtifactRef
    Findings          []ArtifactRef
}

type ContinuationPolicy struct {
    Freshness         FreshnessPolicy // cache_allowed | force_selected
    ScheduleScope     ScheduleScope   // selected | affected_closure
    MaxAffectedNodes  int
    MaxRepairRounds   int
}
```

`TaskChangeRequest.Payload` 必须按 provider manifest 的 schema 校验，不能使用无约束的 `map[string]any`。人工 patch、审核意见驱动修订、自动返修和导入修改都是 ChangeProvider；新增 provider 不应修改 Engine。

`PreparedChange` 只保存 selector 展开结果、MutationReceipt 和规范化资源变化。`ContinuationPlan` 不得再保留动态 stage selector、裸 `restart_from` 或自由字符串 mode：

```go
type ContinuationPlan struct {
    PlanID              string
    CommandID           string
    Strategy            ContinuationStrategy // 仅审计和解释
    BaseCheckpoint      CheckpointRef
    NextExecutionEpoch  int
    SourceRunID          string
    TargetRunRelation   RunRelation // same_run_attempt | child_run
    PreparedChangeID     string
    TaskRevisionID      string
    TaskDigest          string
    CandidateRevisionID string
    Nodes               []NodeTransition
    RetireArtifacts     []string
    Schedule            []ScheduleBatch
    Assertions          []PlanAssertion
    ExpiresAt           time.Time // frozen Run profile 的 continuation_plan_ttl 派生
}

type NodeTransition struct {
    NodeID              string
    FromGeneration      int
    ToGeneration        int
    Disposition         NodeDisposition // preserve | schedule | invalidate
    ReasonCodes         []PlanReason
    ExpectedInputDigest string
    InputBindings       []ArtifactBinding
}
```

### 10.4 Effect 与 lineage 驱动失效

每个 stage/plugin 必须声明 effect 和资源读写集合：

```go
const (
    EffectReadOnly           StageEffect = "read_only"
    EffectEvidenceOnly       StageEffect = "evidence_only"
    EffectContentProducer    StageEffect = "content_producer"
    EffectContentMutator     StageEffect = "content_mutator"
    EffectExternalSideEffect StageEffect = "external_side_effect"
)
```

stage 只用于用户选择和展示分组，不决定失效语义。Planner 使用 `declared writes ∪ observed changes` 与 artifact 输入指纹计算受影响闭包；声明缺失或变化无法分类时，保守退化为 `task/**`。本地 package 物化、提交等 effect 默认不因继续处理自动重复，必须在计划中显示并单独确认。当前本地 package 的对账只核验 immutable receipt、package digest 和 source digest，不触发任何远端操作。

主动重算 `EffectContentProducer/EffectContentMutator` stage 时，即使 command 没有显式 ChangeRequest，也必须先把该 stage 包装成 durable PreparedChange，在隔离 candidate 中执行并 checkpoint。candidate 准备完成、digest 和 observed changes 已知后，plan 才能进入 `continuation_ready`；不能先假定“主动重算永远不改变 TaskRevision”。

内容变化使用不可变候选协议：

```text
sealed TaskRevision
  → isolated RevisionCandidate
  → ChangeProvider 修改 candidate
  → 计算 before/after digest
     ├─ 未变化：丢弃 candidate，按 recheck/no-op 处理
     └─ 已变化：执行时原子提交新 TaskRevision 与 ContinuationPlan
```

任何 Agent 都不得修改 sealed、validated 或 released revision。不能仅凭“执行过 repair Agent”就增加虚假版本。

### 10.5 Planner 决策与执行流程

| 输入事实 | 默认计划 |
|---|---|
| 无 Change，目标为 `infra_failed/interrupted` | 追加 StageAttempt，恢复最近 checkpoint；保留输入未变且 artifact 完整的上游 |
| Run 为 paused 且 checkpoint/definition/subject 未变 | 恢复同一 Run；可续用当前 attempt 时不增加 attempt/quota |
| Run/Stage 已可靠 canceled | 保留旧终态，创建 parent-linked ContinuationExecution 和新 StageAttempt |
| execution 为 in_doubt/reconciling | 禁止执行 continuation；先完成 side-effect reconcile |
| 无 Change，目标已成功且 `force_selected` | 新 node generation；按 lineage 失效依赖旧输出的下游 |
| 目标为 skipped 且 `ResolveBlockers=true` | 解析全部真实失败祖先作为执行根 |
| 有 Change，after digest 未变 | 不创建 TaskRevision；只重检显式目标或返回 no-op |
| 有 Change，after digest 已变 | 新 TaskRevision 和 child WorkflowRun；调度所有读取变化资源的节点及依赖闭包 |
| profile/template/plugin policy 改变，题目未变 | TaskRevision 不变，但创建冻结新定义的 child WorkflowRun |
| artifact 缺失、损坏或输入指纹不一致 | 即使节点曾成功也不得 preserve |
| quota exhausted 或 admission rejected | 返回不可执行 plan，展示缺口；不能通过 clone/new run 清零 |
| 计划包含本地 package 物化或未来 submit 等 side effect | 默认阻止自动重复，要求显式确认和幂等键；本轮 package 对账仅比较 receipt 和 digest |

公开 application API 保持两步：

```go
PlanTaskContinuation(command ContinueTaskCommand) (ContinuationPlan, error)
ExecuteTaskContinuation(planID string) (ContinuationExecution, error)
```

`PlanTaskContinuation` 内部完成 command 校验、必要的 durable candidate preparation 和精确 lineage 规划。准备阶段可耗时，因此自身也是可观察、可取消、可恢复的 Job，但不得切换 current revision 或调度正式验证节点。计划预览只展示：

- 失败/继续原因和系统建议；
- Task 版本是否变化；
- 将执行、失效和复用的阶段及原因；
- budget/profile 变化；
- 外部副作用和人工确认项。

执行时不重新解释用户输入，只消费冻结计划：

1. 用 `BaseCheckpoint` 比较 sequence、execution epoch、Task version/revision/digest 和 workflow digest；
2. 使用 compare-and-swap 原子提交 candidate revision、artifact retirement、execution epoch 和 durable jobs；
3. Engine 幂等执行 `NodeTransition` 和 `ScheduleBatch`；
4. 崩溃恢复重放同一 plan，不重新调用 ChangeProvider，不重复已提交 generation。

### 10.6 幂等、过期与并发规则

- 相同 `CommandID` 和相同 payload 重复提交，返回同一个 plan/result；payload 不同则冲突；
- `OperationKey` 在调用 ChangeProvider 前持久化，崩溃后进入 reconcile，禁止盲目再次调用 Agent；
- plan 执行前 checkpoint 任一字段变化即返回 `plan_stale`，不得静默重算后继续；
- plan 固定 workflow template、plugin manifest、policy 和 artifact binding 版本；
- 同一 Task 同时只能有一个 revision write lease；不可变 revision 的只读 run 可并发；
- 基于同一 checkpoint 的两个 continuation 最多一个能通过 CAS；
- 放弃或过期 plan 的 candidate 进入可回收状态，不得成为 current revision；
- 每个编译节点在 plan 中必须恰有一个 `preserve/schedule/invalidate` disposition。
- WorkflowRun 始终只绑定一个 TaskRevision 和 definition/profile hash；revision 或冻结定义改变时必须创建 child WorkflowRun。

## 11. Artifact V2 与失效规划

### 11.1 ArtifactRefV2

```text
artifact_id/content_digest/schema_version
run_id/stage_id/node_id/attempt/turn
workflow_definition_hash
task_revision_id/task_digest
input_artifact_digests
producer_version
created_at
state=active|superseded|quarantined
```

### 11.2 不可变存储

- 同路径不再覆盖旧 artifact；
- canonical/latest 只是索引；
- rerun 失败后旧成功证据仍可查看和回滚；
- 删除 workspace 不得破坏其他 run 引用的 evidence；
- clone/reuse 使用 artifact ID 和引用计数，不引用可被删除的源 workspace 裸路径。

### 11.3 InvalidationPlanner

裸 `restart_from` 被替换为读写集合和输入 digest：

```text
repair writes task.files
  → TaskRevision N+1
  → 所有 read_sets 包含 task.files/task.digest 的 stage 失效
  → Docker/Oracle/Lint/Quality/Similarity/Evaluation/Package 重新绑定 N+1
```

## 12. ChangeProvider 与修订事务

### 12.1 内容变更事务

```text
1. 持久化 ContinueTaskCommand/OperationKey，并检查 round/budget
2. 收集规范化 findings bundle
3. 从 input revision 创建 candidate checkout
4. ChangeProvider 仅修改 candidate
5. 计算 after digest，写 MutationReceipt
6. 生成精确 ContinuationPlan
7. 用户确认后原子 seal candidate TaskRevision 并提交 plan/jobs
8. 在新 revision 上运行受影响检查
9. pass → 切换 current revision
10. fail → 下一轮 ContinueTaskCommand 或 needs_human
```

任何 task 写操作都必须发生在 revision/budget 校验之后。

### 12.2 Findings bundle

必须包含：

- checker/stage/check ID；
- severity、message、stderr 摘要；
- report artifact ID/digest；
- 当前 TaskRevision digest；
- 操作员 guidance；
- 已尝试修复与结果。

不得再只传 `blocking=true` 和自由文本 notes。

### 12.3 自动循环

RepairSession 是 `revise_task` 策略的审计聚合，不是另一套执行入口。它持久化 `round/max_rounds/status`；自动循环每一轮都通过 TaskContinuationService 生成新 command/plan，直到：

- checks pass；
- 达到 round 或 elapsed budget；
- 发生 non-repairable finding；
- Agent/存储进入需要人工 reconcile 的状态。

耗尽后状态为 `needs_human`，不是让整个 DAG 无解释地 skipped。

## 13. WorkflowTemplate 与策略配置

### 13.1 编译方式

新增版本化 `WorkflowTemplateCompiler`：

```text
WorkflowTemplate + ExecutionProfile + EvaluatorProfiles + RunRequest
  → validate
  → resolved WorkflowDefinition
  → freeze typed RunExecutionSpec + canonical profile
  → persist managed run inputs / run_manifest.json / definition_hash
  → compile dependency-level ExecutionPlan
  → execute
```

`StartRun` 在任何 durable job 调度前把 canonical profile 和 typed `RunExecutionSpec` 写入受管 Run 目录，并将其 digest 写入 Run manifest；调用方的松散文件不是执行权威。初始 `ExecutionPlan` 按 DAG dependency level 分 batch，同一层的全部 ready stage 并发启动，只受冻结 quota/capacity 限制，不能因 catalog 顺序被意外串行化。运行中不因磁盘突然出现 report 而改变 DAG；不存在 V1 import/reuse fallback 或另一张兼容图。

### 13.2 Typed plugin contract

替代无 schema 的 `Config map[string]any`：

```go
type PluginDescriptor struct {
    Kind         string
    Version      string
    InputSpecs   []ArtifactSpec
    OutputSpecs  []ArtifactSpec
    Capabilities CapabilitySet
    PolicyClass  string
}
```

配置在 workflow compile 时完成 typed decode 和校验。

### 13.3 EvaluatorProfile

模型评测改为数组：

```text
evaluator_id
model
agent
trial_policy
concurrency
infra_retry_policy
evidence_validator
```

Qwen/Opus 是默认 profile，不再是 Runner、Registry、TUI 和路径层的固定分支。

## 14. 完整题目生命周期服务

### 14.1 TaskService

```text
CreateDraft
ImportTask
GetTask / ListTasks
ForkTask
ArchiveTask
SoftDeleteTask
RestoreTask
PurgeTask
```

### 14.2 RevisionService

```text
CreateCandidate
CheckoutCandidate
CommitCandidate(expected_version)
DiscardCandidate
DiffRevisions
SealRevision
SetCurrentRevision
CreateRollbackRevision
```

修改操作不再直接编辑 live taskDir。TUI `$EDITOR` 编辑的是 RevisionCandidate checkout，保存后按 digest 决定提交新 revision 或丢弃 no-op candidate。

### 14.3 RunService / TaskContinuationService

```text
PlanRun
StartRun
AttachRun

RequestExecutionControl(pause|cancel_stage|terminate_run)
GetControlOperation
ReconcileExecution

PlanTaskContinuation
GetContinuationPlan
ExecuteTaskContinuation
CancelContinuation

GetBudgetAndQuota
GrantBudget(expected_version)
```

TaskContinuationService 是暂停/失败后的恢复、主动阶段重算和内容修订后的唯一 application service。ExecutionControlService 负责目标明确的暂停、stage cancel 和 Run terminate；它不自行决定后续重跑。任意声明 `cancel` capability 的耗时阶段均可取消，不再只支持 Qwen/Opus。

`StartRun` 和 `TaskContinuationService` 都只接受完整的 frozen profile 与 `RunExecutionSpec`。profile/spec 的 canonical bytes、格式版本和 fingerprint 必须在受管目录与 Run manifest 中一致；缺失、篡改或不匹配时拒绝执行，绝不降级到旧 Runner、旧配置文件或 stage-name fallback。

### 14.4 ReviewService

```text
RequestReview
Decide(approve|request_changes|reject_terminal)
```

`request_changes` 与终止性 reject 必须分离。Decision 绑定 expected revision/evidence digest，禁止批准已变化的题目。

### 14.5 ReleaseService（本地 package）

```text
ValidateForPackage
PackageRevision
ListReleases
PromoteChannel
WithdrawRelease
```

本地 package 产生不可变 Release v1/v2，不覆盖同一 `TaskOutputDir`。重跑默认不得自动创建 package，除非命令明确包含 package intent 和 expected release version。每个 package 均以 immutable receipt、package digest 和 source digest 对账；Harbor Flow 不提供远端发布或上传命令。

### 14.6 DeletionService

删除必须区分：

- 删除 disposable workspace；
- 删除/取消 run；
- soft-delete Task；
- withdraw Release；
- retention 后 purge。

Purge 前检查 active run、release、artifact 引用和 lease，并写 AuditEvent。默认操作是软删除和可恢复回收站。

## 15. Task Hub 与 TUI

### 15.1 首页从 Workspace Hub 改为 Task Hub

建议三视图：

```text
Tasks | Runs | Queue
```

Task 列表展示：

- Task 名称和稳定 ID；
- 当前 revision/version/digest；
- lifecycle 状态；
- 最新 run 和失败阶段；
- 最新 release；
- 活跃 repair/review；
- 磁盘占用。

### 15.2 Task Detail

Tabs：

```text
概览 | 修订历史 | Runs | Reviews | Repairs | Releases | Artifacts
```

所有会改变状态的快捷键使用“两段式语义命名空间”，不允许单个字符直接执行 mutation：

```text
t n  新建 Task
t i  导入 Task
t g  从仓库出题
t e  基于当前 revision 创建 draft 修改
t f  Fork Task
t a  归档 Task
t d  软删除 Task
t u  恢复 Task

x c  继续处理 paused/canceled/terminal run/stage
x n  启动新 Run
x a  Attach 到仍在运行的 durable job（不改变状态）
x k  打开运行控制（等价于 Ctrl+X，不直接取消）

v a  审核通过
v c  要求修改
v r  终止性拒绝

p p  生成本地 package
p w  撤回 release
```

`t/x/v/p` 只进入快捷键前缀状态，不立即执行动作。footer 常态只显示命名空间；按下前缀后显示该上下文允许的第二键、动作名称和禁用原因。前缀在 1.2 秒后或按 `Esc` 取消，输入框聚焦时完全关闭序列解析。这样同一个字母不会因页面不同而偷偷改变含义。

### 15.3 Run Detail

按 Stage group 展示，不再只展示扁平 node list。Task、Run、Stage 和 FinalReview 上下文统一提供：

```text
x c  继续处理
```

打开后默认带入当前 Task、Run、Stage、failure 和 findings。主表单只收集可选处理说明；“范围：系统建议”是默认值，阶段范围、截止阶段、模型/预算覆盖和自动循环轮数放入高级设置。用户不能手工选择复用不满足 lineage 的证据，也不再填写目标 workspace。

计划预览固定展示：原因、建议处理、Task 版本影响、重新执行范围、失效结果、复用证据及原因、预算变化和外部副作用。内容修订先显示 candidate；只有 digest 真正变化，执行确认页才显示将提交 `vN+1`。

`x a` 只重新附着仍由有效 lease 持有的 durable job，不创建 attempt。paused、canceled、interrupted 或 terminal failure 都通过 `x c` 进入 Planner，由状态和 reconcile 结果决定恢复 checkpoint 还是创建新 execution。审核拒绝固定为 `v r`，不再与 retry、rerun、resume 共享 `r`。导航、滚动、关闭 overlay 可以保留单键；任何创建、修改、执行、取消、审核、生成本地 package 或删除操作都必须使用两段式快捷键并进入确认/计划页。

执行前显示 invalidation plan，不允许用四个无解释的“复用证据”布尔开关代替规划。

### 15.4 旧入口移除

本轮采用 hard cutover：不保留 manual retry、clone rerun、task repair、FinalReview repair 的 handler、薄适配器、迁移测试入口或 CLI alias。所有用户可见继续、重算和内容修订都只能进入 typed `TaskContinuationService` 或 `ChangeProvider` 流程。

### 15.5 `Ctrl+X` 运行控制

`Ctrl+X` 是组合快捷键，可以保留，但它只能打开 RunControlOverlay，不能直接取消：

```text
运行控制

> 返回并保持运行
  P 暂停运行
  K 取消选中阶段
  S 终止本次运行

当前阶段          harbor_qwen / attempt 2
最近 checkpoint  14:32:18 / turn 3
已用/剩余预算     stage 2/3, trial 2/4, elapsed 18m/45m
未决副作用        无
```

`P/K/S` 只改变 overlay 选中项，必须再按 Enter 查看影响预览并确认；默认选中“返回并保持运行”，`Esc` 永远无副作用。非支持 cancel capability 的 stage 禁用 K 并显示原因。确认后 UI 持续显示 `requested → pausing/canceling → acknowledged|reconcile_required`、最近 checkpoint、runtime ack 和 quota settlement。

删除所有 plain `x` cancel 绑定。退出 TUI 不再是全局 detach：界面逐个列出 active Run，运营者对每个 Run 单独确认受控 child-worker handoff；未完成选择不得静默取消或把同一选择批量应用给所有 Run。要暂停或终止必须显式进入 RunControlOverlay。TUI context、scheduler context、每个 run context 和每个 stage context 必须分离，任何 target cancel 都不得向上取消共享 root context。

Hub 使用四个不同标签：`已暂停·可继续`、`阶段已取消·Run 仍进行`、`已终止`、`异常中断·待 reconcile`。canceled/skipped stage 不计入“可复用成功阶段”。

## 16. CLI/API 设计

CLI 与 TUI 调用相同 service：

```text
harbor task list|show|create|import|fork|archive|delete|restore
harbor task continue --run <run-id>
harbor task continue --run <run-id> --dry-run --json
harbor task continue --run <run-id> --from-stage quality --scope affected
harbor task continue --task <task-id> --guidance-file review.txt
harbor task continue --plan <plan-id> --yes
harbor revision list|show|diff|edit|seal|rollback
harbor run start|show|attach
harbor run pause --run <run-id>
harbor run cancel-stage --stage-attempt <id>
harbor run terminate --run <run-id>
harbor run reconcile --run <run-id>
harbor budget show --run <run-id>
harbor budget grant --run <run-id> --dimension stage_attempt --delta 1 --reason ...
harbor review decide --action request-changes|approve|reject
harbor release package|list|withdraw|promote
harbor workspace list|trash|purge
```

公开 CLI 不提供 `--mode retry|rerun|repair`。无参数时 Planner 根据 failure、verdict、findings、目标状态和 digest 自动诊断；`--from-stage` 只表达用户目标，不直接决定失效边界。

`run retry-stage`、`run rerun` 和 `repair start` 不注册、不隐藏，也不提供兼容 mutation 路径。旧 Runner 和 workspace-clone 路径已删除。

所有 mutation command 包含：

- actor/reason；
- idempotency key；
- expected entity version；
- dry-run/plan 能力；
- AuditEvent。

## 17. Scheduler 与并发控制

### 17.1 当前问题

- 队列仅在内存；
- 进程重启丢 queued/running 状态；
- terminal job 不释放 workspace reservation；
- 锁只按 workspace，两个 workspace 可同时修改同一 taskDir；
- 后台任务的生命周期不进入 durable command log。

### 17.2 V2

引入 durable Job/Lease：

```text
Job(id, command_type, entity_id, state, priority, payload, idempotency_key)
Lease(resource_type, resource_id, owner, expires_at, fencing_token)
```

并发规则：

- immutable TaskRevision 可被多个只读 Run 并发使用；
- RevisionCandidate checkout 需要 candidate/task write lease；
- current revision 切换使用乐观锁；
- Release 同 channel/version 使用幂等键；
- workspace lease 只保护执行目录，不代替 Task 锁。

### 17.3 Durable execution control 与取消恢复

`Ctrl+X` 不再直接 cancel context，而是创建 target-scoped control command：

```go
type ExecutionControlCommand struct {
    OperationKey      string
    Action            ControlAction // pause | cancel_stage | terminate_run
    RunID             string
    StageAttemptID    string
    Expected          CheckpointRef
    Actor              string
    Reason             string
    GracePeriod        time.Duration
}

type ControlOperation struct {
    OperationID       string
    Status            ControlStatus // requested | propagating | acknowledged | reconcile_required | failed
    RuntimeReceipts   []RuntimeTerminationReceipt
    CheckpointID      string
    QuotaSettlementID string
}
```

协议：

1. 在事务中持久化 command，并 CAS `running → pause_requested/cancel_requested/stop_requested`；
2. 同事务写 outbox，由 worker 使用 run/stage scoped token 传播，禁止取消 TUI root context；
3. runtime adapter 先请求 graceful checkpoint/stop，再在 grace period 后升级终止；
4. 保存 partial artifacts、TurnCheckpoint、子进程/外部 job termination receipt；
5. 结算已消耗 quota，释放未使用 reservation、concurrency slot、workspace/job lease；
6. 无未决副作用才写 `paused/stage_canceled/canceled`；否则写 `in_doubt` 并调度 reconcile；
7. `acknowledged` 之后 UI 才显示“暂停完成/终止完成”，不能用“已请求”toast 代替最终状态。

恢复决策：

| 取消/中断事实 | 后续处理 |
|---|---|
| queued job 取消，尚未执行 | 直接 canceled，释放全部 reservation，不产生 StageAttempt/trial 消耗 |
| paused 且 checkpoint/input/definition 未变 | `x c` 生成 ResumeFromCheckpoint plan，恢复同一 Run，不新增 attempt |
| paused 但 checkpoint 不可恢复 | plan 明示新增 StageAttempt 和 quota `+1` |
| stage_canceled，Run 仍在运行 | 其他独立分支继续；目标 stage 后续由 ContinuationPlanner 决定是否补跑 |
| Run 已可靠 canceled | 旧 Run 保持终态；`x c` 创建 parent-linked ContinuationExecution/StageAttempt |
| worker 无 cancel intent 消失 | 标记 interrupted，恢复前校验 lease、definition、subject 和 artifact |
| candidate preparation 被取消 | current revision 不变；candidate quarantine/discard，或在新计划中显式复用 checkpoint |
| 本地 package materialization receipt、trial 或未来外部 API 结果未知 | in_doubt；package 先按 receipt、package digest 和 source digest 对账；其他情况先查询 operation/artifact，再决定 completed/canceled/failed_recoverable |

进程重启时，RecoveryReconciler 扫描 `requested/propagating/running` 且 lease 过期的 operation。系统可以自动 reconcile，但不得自动恢复用户明确 terminate 的 Run。terminal job 必须从 active reservation 索引移除，历史仍保留在 jobs 表。

### 17.4 AdmissionControl 与 BudgetLease

通用 Kernel 在 job 入队前做一次原子 admission，在 dispatch 时再租用瞬时 capacity，避免“先检查后抢占”的竞态，也避免 queued job 长期占用并发槽：

```go
type AdmissionControl interface {
    AdmitAndReserve(context.Context, AdmissionRequest) (AdmissionDecision, error)
    AdmitDispatch(context.Context, DispatchRequest) (DispatchPermit, error)
}

type QuotaManager interface {
    Reserve(context.Context, ReserveRequest) (BudgetLease, error)
    Charge(context.Context, UsageEvent) (BudgetSnapshot, error)
    Heartbeat(context.Context, LeaseHeartbeat) (BudgetLease, error)
    Settle(context.Context, SettlementRequest) (BudgetSettlement, error)
    Reconcile(context.Context, QuotaReconcileRequest) (BudgetSettlement, error)
}

type ExecutionLifecycle interface {
    Prepare(context.Context, LifecycleRequest) (PreparedExecution, error)
    Start(context.Context, StartExecutionRequest) (ExecutionLease, error)
    Checkpoint(context.Context, LifecycleCheckpoint) error
    Complete(context.Context, CompletionRequest) error
    RequestControl(context.Context, ExecutionControlCommand) (ControlOperation, error)
    Reconcile(context.Context, RecoverySubject) (RecoveryDecision, error)
}
```

Admission 事务同时校验 frozen plan/policy、CAS quota bucket、写 BudgetLease、durable Job、decision 和 outbox。每个 `UsageEvent` 必须带 turn/trial/API operation key，重复上报只计一次。外部调用已经 started 但 usage 未知时，settlement 记为 uncertain，额度不能直接退还，等待 reconcile。

部署级 worker/concurrency capacity、tenant/provider API bucket 和 run budget 可以组合取最小 grant。`MaxTaskConcurrency=10` 应成为 capacity pool 配置；pass@4、repair rounds 和 evaluator 限制只产生 ResourceDemand，不进入 AdmissionControl 的领域分支。

## 18. 数据库与文件布局迁移

### 18.1 V2-only Schema Baseline

新 control-plane database 直接从 schema version 2 引导，不创建 V1 `tasks`/`runs` 表，不写入 schema-version 1。此交付将 V2 schema 收敛为单一 consolidated baseline：任何 pre-consolidation V2 store，以及任何检测到 V1 `tasks`/`runs` 表或 schema-version 1 历史的数据库，均在 `Open` 和 `OpenReadOnly` 被拒绝，不做升级、转换或兼容读取。拒绝前只识别 schema marker，不读取业务记录，不执行 import、migration、delete、drop 或 rewrite。损坏恢复也只接受 checksum 和 SQLite integrity 都验证通过的 consolidated-V2 backup；其余情况必须建立新的受管 root。

V2 baseline 包含：

```text
tasks_v2
task_revisions
workspaces_v2
workflow_runs
run_attempts
stage_attempts
node_attempts
turn_checkpoints
review_requests
review_decisions
repair_sessions
releases
artifact_manifests
artifact_refs
continuation_commands
prepared_changes
continuation_plans
mutation_receipts
continuation_executions
control_operations
runtime_termination_receipts
side_effect_operations
reconciliation_attempts
admission_decisions
quota_policies
quota_accounts
quota_ledger_entries
quota_leases
usage_events
budget_settlements
budget_grants
jobs
leases
audit_events
outbox_events
deletion_records
```

### 18.2 Legacy import

已退休。V2 不读取、扫描、导入或兼容 V1 workspace、V1 task identity、
V1 run result 或 V1 evidence。所有新 Task、Revision、Run、StageAttempt 和
artifact lineage 必须由受管 V2 control plane 原生创建；不做 V1/V2 双写，也不把旧数据库转换为 V2。

### 18.3 文件布局

```text
.harbor-factory/
  harbor.db
  objects/sha256/<digest>
  tasks/<task-id>/revisions/<revision-id>/manifest.json
  runs/<run-id>/run-manifest.json
  runs/<run-id>/stages/<stage>/attempt-<n>/...
  workspaces/<workspace-id>/...
  trash/...
```

## 19. 分阶段实施计划

### M0：事故止血与可观察性

- 修正 per-node attempts 覆盖；
- 为 Generation/Quality/Repair 计算一致的父子预算；
- 启动前执行预算校验；
- attempt/turn 使用独立日志路径；
- Engine 在 error 时保留 NodeResult artifacts；
- 删除 plain `x` cancel；`Ctrl+X` 只针对明确 Run，并将 per-run context 与 TUI/scheduler root context 分离；
- Engine、Runner、Scheduler 统一投影 `canceled`，terminal job 释放 workspace reservation；
- failed terminal workspace 允许通过统一 Continuation 入口继续；
- 为 tower-http 事故增加回归测试。

完成标准：当前故障不会再在固定 600 秒重复三次并丢失全部进度。

### M1：冻结运行与统一 Continuation 基础

- 抽取不依赖 Harbor 的 Workflow Kernel package/module，并增加禁止反向依赖的架构测试；
- 持久化 run manifest、resolved workflow/profile/hash；
- 引入 StageCatalog 和 StageAttempt；
- 引入通用 Execution/Attempt 状态机、BudgetManager、QuotaManager 和 AdmissionController；
- 引入 target-scoped durable ControlOperation、runtime receipt、pause/terminate ack 和 restart reconcile；
- 引入 TaskContinuationService、ContinuationPlanner 和冻结 plan；
- 允许 Continue 主动选择成功阶段并 force recompute；
- TUI/CLI 上线单一 `继续处理` 主入口和计划预览；
- artifact 改为 immutable + superseded；
- 初始 DAG 按 dependency layer 并发调度；
- `StartRun` 在受管目录冻结 canonical profile 与 typed `RunExecutionSpec`；
- 不保留旧 Runner、legacy import/reuse fallback 或 command adapter。

### M2：Task/TaskRevision 控制面

- 上线 schema v2 和 stable Task ID；
- 引入 immutable TaskRevision 和 RevisionCandidate checkout；
- 新数据库只引导 V2；检测到 V1 history/table 时拒绝打开；
- 不双写、不导入、不迁移旧 workspace；
- Hub 按 Task 聚合。

### M3：ChangeProvider 与 RevisionCandidate

- 结构化 findings；
- durable RepairSession；
- candidate revision 和 MutationReceipt；
- 自动返修、人工 patch、外部审核反馈接入统一 ChangeProvider contract；
- repair round、Agent turn/token 纳入 durable quota ledger；
- 幂等执行和 crash reconcile；
- digest-based invalidation；
- 真正有界自动循环；
- `needs_human` 升级。

### M4：完整题目生命周期与本地 package

- Task/Revision/Run/Review/Repair/Release 页面和 CLI；
- Fork、Diff、Rollback、Archive；
- immutable Release；
- soft delete/trash/restore/purge；
- durable scheduler/jobs/leases；
- Harbor TrialExecution/TrialAttempt 与本地 package materialization 接入 quota 与 reconcile；本轮 package reconcile 仅基于 immutable receipt、package digest 和 source digest，远端 provider/publish/upload 不在范围内。

### M5：移除兼容债务

- TUI 不再直接修改/删除 live 文件；
- 删除 manual retry、workspace clone rerun 和 repair overlay 的全部源代码与测试；
- 删除裸 workspace evidence 引用；
- 移除 V1 `tasks`/`runs` schema 定义、读写接口和 `restart_from`；含 V1 marker 的已有数据库拒绝打开，不尝试删除或转换其内容；
- 移除旧图外 `gen.Run` 编排；
- 删除旧 prompt/template assets、Python shim、status/index 路径和所有 legacy TUI/CLI command；
- 统一文档中的 Gate 和阶段定义。

## 20. 代码模块建议

```text
internal/app/
  task_service.go
  revision_service.go
  run_service.go
  execution_control_service.go
  task_continuation_service.go
  continuation_planner.go
  change_provider_service.go
  budget_grant_service.go
  review_service.go
  release_service.go
  deletion_service.go

pkg/workflowkit/
  template.go
  compiler.go
  stage.go
  state_machine.go
  budget.go
  quota.go
  admission.go
  scheduler.go
  control.go
  invalidation.go
  continuation.go
  recovery.go
  outbox.go
  outcome.go

internal/workflowruntime/
  sqlite_store.go
  durable_scheduler.go
  process_runtime.go
  outbox_dispatcher.go
  artifact_store.go

internal/harbor/workflowadapter/
  bindings.go
  stage_catalog.go
  evaluation_policy.go
  quota_policy.go
  change_provider.go

internal/harbor/catalog/
  task.go
  revision.go
  release.go
  artifact.go

internal/harbor/store/
  schema_v2.go ... schema_v18.go
  v2_tasks.go
  v2_execution.go
  stage_store.go
  lifecycle_store.go
```

不再保留 `Runner`。通用执行逻辑属于 `pkg/workflowkit`，Harbor 实际 stage operation 只经由受控 registry 解析 frozen `RunExecutionSpec`；HumanGate 只负责 durable review decision。

## 21. 测试策略与不变量

### 21.1 预算和恢复

- `turn_timeout=1800`、两个 550 秒 turn 不会被 600 秒父 context 截断；
- 非法父子预算在启动前失败；
- per-node attempts=1/2/3 原样生效；
- Harbor 外层一次，内部 retry 不与 Engine 相乘；
- attempt 1/2/3 和 turn 日志分别保留；
- draft 后崩溃只恢复 self-review/correction；
- failed artifacts 出现在 NodeRun、RunResult、TUI 和 Repair findings。

### 21.2 Continuation 和 lineage

- 相同 CommandID 重复提交不重复 mutation、增加 revision 或调度；
- stale checkpoint 在任何 current revision 切换和 job 创建前失败；
- 基于同一 checkpoint 的并发 continuation 只有一个通过 CAS；
- 成功阶段可以显式 force recompute；
- 单 stage、phase、start-through、多根节点计划正确；
- task digest 不变时只重跑选择范围；
- task digest 改变时所有相关证据失效；
- Qwen/Opus 可分别重跑，修改题目时两者都失效；
- 修改模型、阈值、template 或程序版本后旧结果不静默复用；
- artifact 缺失/损坏时，即使输入 digest 未变也不能 preserve；
- 每个节点在 plan 中恰好有一个 disposition，schedule 满足拓扑顺序；
- retry、rerun、repair 旧 adapter 构造出相同 PreparedChange 时，plan 语义等价；
- recompute 失败后旧 artifact 仍可查看。

### 21.3 返修

- Quality/Similarity/Verify verdict fail 仍能到达 TaskContinuationService/ChangeProvider；
- findings 包含 checker/check ID/report digest；
- 第 1/2/3/5 轮通过和预算耗尽均可审计；
- repair intent 后崩溃不会重复调用 Agent；
- revision 上限在任何文件写入前检查；
- 两个 workspace 不能同时写同一 RevisionCandidate；
- repair Agent 无 digest 变化时不创建空 TaskRevision；
- mutation 完成后、plan 提交前崩溃不会再次调用 ChangeProvider；
- sealed/published revision 在所有路径均不可写。

### 21.4 生命周期

- 同一 Task 多 revision、多 run 始终保持同一 Task ID；
- import/generated/manual/repair/fork lineage 正确；
- published revision 不可原地修改；
- rollback 创建新 revision；
- 删除 workspace 不破坏共享 evidence；
- soft delete 可恢复；
- active run/release dependency 阻止 purge；
- scheduler 重启后 queued/running job 可 reconcile。
- 新 V2 database 不含 V1 表或 version-1 history；
- `Open` 与 `OpenReadOnly` 都拒绝 V1 table/history marker，且拒绝过程不创建版本表、不读取 V1 行或转换数据库；
- 仅由当前 consolidated V2 baseline 创建、且完整性已验证的 backup 可以恢复；任何 pre-consolidation V2 store 均被拒绝且不会被转换。

### 21.5 取消、恢复与 quota

- plain `x` 在任何页面都不触发 mutation，`Ctrl+X` 只打开 RunControlOverlay；
- overlay 默认“返回并保持运行”，`P/K/S` 只选择，Enter+确认后才创建 ControlOperation；
- 取消一个前台 Run 不会取消 TUI root context、其他 queued/running jobs 或 Hub；
- queued cancel 不创建 StageAttempt，不消费 trial，仅释放 reservation；
- pause 在 checkpoint ack 后才进入 paused，恢复可用时不新增 attempt；
- terminate 后 Engine、state projection、Scheduler 对状态统一为 `canceled`，不再出现 failed/cancelled/canceled 三套值；
- stage cancel 只影响目标 capability，独立分支继续运行；
- TUI exit 对每个 active Run 都有独立 handoff 决策和集成测试，任何单个选择都不影响其他 Run；
- external side effect 未确认时进入 in_doubt，reconcile 前禁止 continue/retry；
- terminal job 释放 active workspace reservation，但保留历史；
- 已消费 token/API/trial/elapsed 不因 pause/cancel/clone/continuation 清零；
- quota exhausted 禁止执行 plan，BudgetGrant 幂等且 stale version 被拒绝。

### 21.6 通用 Kernel 可复用性

- `pkg/workflowkit` 的依赖图中不存在 `internal/harbor`，且已删除的 `internal/workflow` 不得恢复；
- 使用 fake domain bindings 可运行一个非 Harbor AI workflow，并复用预算、quota、checkpoint、取消和 continuation；
- 同一 dependency level 的初始 stage 形成一个可并行 batch，跨 level 依赖顺序被验证；
- 缺失、篡改或非 canonical 的 `RunExecutionSpec`/profile 被拒绝，且不存在 legacy Runner、stage-name 或动态 payload fallback；
- Kernel 不包含 Qwen、Opus、pass@4、FinalReview、TaskRepair 等字符串或分支；
- Harbor TrialCount、TrialAttempt 和 StageAttempt 三种计数不会互相覆盖或相乘；
- 新增 evaluator/change provider 不修改 Engine 即可编译、计划、取消和恢复。

## 22. 验收主线

必须通过以下端到端场景：

```text
创建 Task
  → 从仓库出题生成 revision 1
  → task_generation 在 self-review 前崩溃
  → `x c` 从 checkpoint 继续，不重做 draft
  → Quality verdict needs_repair
  → `x c` 携带 findings，RepairSession round 1 生成 revision 2
  → 只重跑受 digest 影响的阶段
  → FinalReview request_changes
  → `x c` 继续修改，RepairSession round 2 生成 revision 3
  → 验证通过
  → Qwen 运行到 trial 2/4 时通过 Ctrl+X 暂停
  → checkpoint ack 后关闭 TUI，后台状态和 quota ledger 保持
  → `x c` 恢复且不重复已完成 trial
  → Qwen/Opus 分别完成，可用 `x c` 单独重新计算 Opus
  → 生成本地 package Release v1
  → 手工修改形成 revision 4
  → 生成本地 package Release v2
  → 查看 revision diff、run/stage/repair/release 历史
  → 归档 Task
  → soft delete
  → restore
```

全过程要求：

- Task ID 不变；
- 每个 revision digest 唯一且不可变；
- 每个 StageAttempt 和 artifact 可追溯；
- 不存在旧 digest 证据混用；
- 失败后始终存在明确可执行动作；
- 不通过重启整个进程或复制裸目录来模拟生命周期；
- 所有状态变更快捷键均为无冲突的组合键/两段式语义序列；
- pause/cancel/continue 不会重置已消费 budget/quota。

## 23. 成功指标

- 预算矛盾在启动前发现率 100%；
- Agent stage 崩溃后可从最近 durable substep 恢复；
- 技术 retry 不重复已完成副作用；
- 任意 paused/canceled/terminal stage 均可生成可解释 ContinuationPlan；
- Ctrl+X 从请求到 runtime ack、checkpoint、quota settlement 和恢复全链路可审计；
- Workflow Kernel 可被至少一个非 Harbor AI workflow 直接复用；
- 返修后旧 task digest 证据误复用为 0；
- 同一题目的生成、修改、返修、运行和本地 package/release 历史可在一个 Task 视图查看；
- 删除和本地 package 均可审计、可恢复或明确不可逆；
- CLI/TUI 对同一命令产生相同计划和状态迁移。

## 24. 明确不采用的方案

- 仅把 `generate_task_files` timeout 从 600 改成 1800；
- 继续以 workspace path 作为 Task/Run 身份；
- 通过复制报告文件实现无 lineage 的复用；
- 继续原地修改已验证或已发布 taskDir；
- 继续用裸 `restart_from` 表达题目变更后的失效范围；
- 让 TUI 直接实现业务状态迁移或直接 `RemoveAll` 生命周期实体；
- 用单字符快捷键在不同页面复用 retry、rerun、resume、reject 或 cancel 语义；
- 把关闭 TUI/进程退出等同于取消所有 durable jobs；
- 在每次 Engine resume、clone 或新 Run 时重置 retry/trial/token quota；
- 把 pass@4、Qwen/Opus、TaskRevision 等 Harbor 规则硬编码进通用 Kernel；
- 用 `mode=retry|rerun|repair` 把旧分支包装成新的统一 API；
- 用更多 Gate ID、路径 switch 和布尔开关扩展功能。

本方案的核心不是增加更多特殊分支，而是建立稳定身份、不可变版本、冻结执行定义、统一预算、持久阶段和可计算血缘，使 Harbor Factory 从“工作区驱动的长脚本”演进为可恢复、可审计、可管理的题目生产系统。
