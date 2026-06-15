# P2R TUI 任务运行生命周期管理重构设计方案

> 版本: v5.1 | 日期: 2026-06-15 | 状态: 当前落地 + 后续计划

本文档基于当前代码和 3 个并行子代理的只读核验结果迭代。结论是：原 v4 文档抓住了 Ctrl+R 路径分裂、错误可观测性不足、Ctrl+E/Ctrl+S 竞态这三个核心问题，但也存在若干过度结论和不可落地建议，尤其是把 Scheduler Job、Pipeline Run、DB Task 三套状态机混在一起，以及建议将 `canOpenInspectionRunConfig` 改为永远 `true`。

本版直接修正这些问题，记录本次已落地的重构结果，并保留后续 schema/diagnostics 计划。

---

## 0. 本次核验摘要

### 0.1 已确认属实的核心问题

| 问题 | 代码证据 | 结论 |
|------|----------|------|
| Ctrl+R 存在 fall-through | `internal/tui/keymap.go:245-268` | TaskInput/TaskBoard/Overview 部分路径走 `runConfigActionInspection`，执行详情页和部分 Overview 空/陈旧选择路径会落到 `runConfigActionPipeline` |
| Pipeline 路径绕过 Inspection 生命周期校验 | `internal/tui/runconfig.go:276-280`, `internal/tui/app.go:960-967`, `internal/tui/shared.go:306-329` | 直接 pipeline 仍有 stage/refRun/scheduler 内存去重，但绕过 task 状态、`CurrentRunID`、容量、文档、batch/gitURL/projectType 等 Inspection guard |
| `submitInspection` 把所有提交错误写成 Git 同步错误 | `internal/tui/shared.go:629-645` | scheduler unavailable、active job、容量等错误都会进入 `tasks.sync_error`，UI 容易误报为 Git 同步失败 |
| `enrichTaskProject` 静默吞 DB 错误 | `internal/tui/shared.go:164-183` | `LatestRunForTask` 或 `Stages` 查询失败时直接返回，TaskBoard 会丢失运行/失败摘要上下文 |
| Ctrl+E/Ctrl+S 存在 check-then-act 竞态 | `internal/tui/shared.go:415-495`, `internal/db/store.go:382-390`, `internal/db/store.go:1188-1216` | `CompleteTask` 只约束 state，`RecordTaskRuntime` 没有 state/current_run_id 条件，可能在 completed 后写回 `docker_running=1` |
| `prepareRun` 早期失败缺少持久化证据 | `internal/pipeline/pipeline.go:141-213`, `internal/pipeline/run_lifecycle.go:154-193` | `CreateRun` 前失败没有 DB run/crash artifact；但 scheduler/TUI 仍可能短暂展示 `job.Err`，不是完全不可见 |
| `current_run_id` 空值契约是 `NULL` | `internal/db/store.go:1013-1021`, `internal/db/migrate.go:331` | 任何 reset 方案必须写 `NULL`，不能写空字符串，否则会破坏 FK/CAS 语义 |
| 生命周期 API 存在多套旧 facade | `internal/tui/shared.go:64-77`, `internal/tui/app.go:186-192`, `internal/tui/runconfig.go:30-35`, `internal/pipeline/recovery.go:58-72` | `TaskActionService`、`inspectionScheduler`、`schedulerClient`、`runConfigActionPipeline`、recovery package wrappers 都在保留旧路径 |
| DB 写入口也能绕过生命周期 | `internal/db/store.go:991-1021`, `internal/db/store.go:1028-1078`, `internal/db/store.go:1164-1216` | `CreateRun` 会把 task 改回 inspecting，`FinishRun` 和 runtime 写入缺少足够 CAS，必须纳入统一 transition |
| 测试层固化旧生命周期入口 | `tests/internal/scheduler/scheduler_test.go`, `tests/internal/tui/taskaction_test.go`, `internal/tui/testhooks.go:227-244` | 测试仍围绕旧 `Submit`/`SubmitInspection`/`dbTaskActionService`，必须同步迁移，不能只改生产代码 |

### 0.2 修正原文中过度或错误的说法

| 原说法/方案 | 修正 |
|-------------|------|
| PATH 4 “无任何检查” | 改为“无生命周期检查”。Pipeline 路径仍有 stage plan/refRun 检查、scheduler `activeByTask`、pipeline 文件锁 |
| `SubmitInspection` 的 Job 就是完整 inspection job | 实际是先创建 `JobGitSync`；Git 成功后 `enqueuePipelineAfterGit` 再创建独立 `JobPipeline` |
| `prepareRun` 失败用户完全看不到 | 短期可通过 `job.Err` 看到；问题是 `CreateRun` 前没有可长期查询的 DB run/crash artifact |
| `canOpenInspectionRunConfig` 改成 `return true` | 不可落地，会允许 `waiting_manual` 绕过人工处理。应改成基于完整 `Task` 上下文的 admission guard |
| `ResetTaskForRerun` 设置 `current_run_id=''` | 错误。必须设置 `current_run_id = NULL` |
| 本次不需要 DB schema 变更 | 过于武断。建议在 P1 引入 schema v7：运行唯一约束、状态 CHECK、任务事件/错误表 |
| TUI diagnostics 可直接复用 `taskRunLockStatus` | 当前该类型和 lock path 函数未导出，需导出 pipeline helper 或把诊断实现放在 pipeline/lifecycle 包内 |
| `_ = err` 完整审计为 29 处 | 需要区分 best-effort Close/Rollback 和真正丢失业务错误。本版只列已确认的 P0/P1 热点 |
| 为迁移方便保留旧 facade | 不接受。生命周期模块需要破坏式统一：删除旧接口、旧 wrapper、旧测试 fake，让编译器暴露全部调用点 |

### 0.3 本次已落地范围

- TUI 题目操作统一进入 `tasklifecycle.Manager`：提交质检、Git 重试、待处理 Docker 启动、人工完成不再通过旧 `TaskActionService` 分散执行。
- Scheduler 提交入口统一为 `SubmitRequest{Flow: ...}`，删除旧 `Submit`/`SubmitInspection` 双入口和空壳 `JobInspection`，Git sync 成功后派生 Pipeline job 并保留 `FlowID/ParentJobID`。
- Ctrl+R 删除 direct pipeline fallback，Overview/TaskBoard/Execution/TaskInput 都走 Inspection run config 和生命周期 admission。
- 人工完成使用 `CompleteTaskWithVerdict` 的同事务路径，verdict、task completion、runtime 清理字段、completed 溢出归档保持一致。
- DB run/task CAS 收紧：`CreateRun` 拒绝 `waiting_manual` 直接开 run；`FinishRun` 要求 run 存在、属于 task、原状态为 `running`，task 迁移要求 `current_run_id = runID`。
- Runtime 写入拆为 active-run 与 waiting-manual 两条受限路径：Stage B 使用 run 绑定写入，StartDocker 只能写 `waiting_manual` task。
- 新增回归测试覆盖非 Git submit error 不写 `sync_error`、Ctrl+R 统一入口、scheduler 空结果失败、waiting_manual 直接 CreateRun 拒绝、FinishRun run/task mismatch、stale runtime 写入拒绝、verdict completion 归档等关键路径。

---

## 1. 当前架构与状态机

### 1.1 三套状态机必须分开建模

当前系统同时存在三套状态机：

1. **DB Task 状态**，用于题目管理看板和容量统计。
2. **Scheduler Job 状态**，用于当前进程内排队/运行/取消。
3. **Pipeline Run 状态**，用于持久化运行记录、阶段结果和产物。

三者相关，但不能等同。

```
DB Task:
  inspecting -> waiting_manual -> completed

Pipeline Run:
  running -> completed_clean | completed_with_findings | aborted | crashed

Scheduler Job:
  queued -> running -> done | cancelled | failed
```

### 1.2 DB Task 状态

当前 DB schema 只允许 3 种 task state：

```sql
state TEXT NOT NULL DEFAULT 'inspecting'
CHECK (state IN ('inspecting', 'waiting_manual', 'completed'))
```

关键字段：

| 字段 | 当前用途 | 生命周期语义 |
|------|----------|--------------|
| `tasks.state` | 看板列、容量统计 | 粗粒度任务阶段，不表达所有异常 |
| `tasks.current_run_id` | 当前活跃 pipeline run FK | 必须用 `NULL` 表示无活跃 run；`CreateRun` 依赖 `IS NULL` CAS |
| `tasks.sync_error` | Git 同步错误文本 | 当前被误用为 submit/admission 错误存储 |
| `tasks.docker_running` | DB 记录的 runtime 状态 | 会被 Stage B、StartDocker、CompleteTask、cleanup 修改 |
| `tasks.compose_meta` | Docker compose 元信息 | StartDocker/cleanup/diagnostics 依赖 |

重要事实：

- `CreateTaskWithBatch` 新建任务时 state 是 `inspecting`，此时可能还没有 run 记录。
- `CreateRun` 用 `WHERE id=? AND current_run_id IS NULL` 防止重复 active run。
- Go 层读取 task 时会通过 `COALESCE(current_run_id, '')` 把 NULL 显示成空字符串；这只是展示/模型便利，DB 写入仍必须使用 SQL `NULL`。
- `FinishRun` 成功 run 会把 task 转为 `waiting_manual`；aborted/crashed 会把 task 转为 `completed` 并清 runtime 字段。
- 当前没有 DB 级唯一索引保证 “同一 task 最多一个 `runs.status='running'`”。

### 1.3 Scheduler Job 状态

当前 `scheduler.Submit(taskID, opts)` 创建 `JobPipeline`。`scheduler.SubmitInspection(taskID, batchID, gitURL, opts)` 创建 `JobGitSync`，Git 成功后再创建一个独立 `JobPipeline`。

```
SubmitInspection()
  -> JobGitSync queued/running
  -> Git done
  -> enqueuePipelineAfterGit()
  -> JobPipeline queued/running/done|failed|cancelled
```

边界条件：

- `activeByTask` 是进程内去重，进程重启后失效。
- queued job 在 `Shutdown` 中会变为 `JobFailed`，不是 `JobCancelled`。
- scheduler 当前允许 runner 返回 `(Result{}, nil)` 时把 job 标为 `JobDone`，这是 runner contract 漏洞。
- `JobInspection` 常量和 `runJob` 分支存在，但当前没有实际提交路径；统一 `SubmitRequest` 时应删除该遗留 kind，或重新定义为可证明的 flow，而不是继续保留空壳分支。

### 1.4 Pipeline Run 状态

Pipeline 真实流程是：

```
Runner.Run
  -> loadAndValidateRunInputs
  -> acquireTaskRunLock
  -> prepareRun
  -> persistInitialArtifacts
  -> persistInitialStages
  -> runPreflightAndCleanup
  -> executeStageLoop
  -> finalizeRuntimeCleanup
  -> finishRun
```

关键边界：

- `acquireTaskRunLock` 是 `.qa-control/locks/<task>.lock` 文件锁，提供跨进程互斥的一层保护。
- `prepareRun` 在 `CreateRun` 前失败时，`state == nil`，无法通过 `persistCrash` 写 run/crash artifact。
- `CreateRun` 之后失败，defer 可以通过 `state.persistCrash` 尝试持久化 crashed run。
- `loadAndValidateRunInputs` 当前把 `GetProject` 原始错误替换成 `task not found`，会丢失 `database is locked`、disk I/O 等上下文。

---

## 2. Bug 根因分析

### 2.1 Bug #1: Ctrl+R 重跑行为不一致

**严重程度**: P0

当前 Ctrl+R 分派：

```go
case "ctrl+r":
    if focusTaskInput -> openRunConfigForTaskInput()              // Inspection
    if panelTaskBoard -> openRunConfigForTask(... Inspection)     // Inspection
    if panelOverview && item.HasTask -> Inspection                // Inspection
    // fall-through
    openRunConfigForSelected(runConfigActionPipeline)             // Pipeline
```

路径对比：

| 路径 | 场景 | action | submit | 主要问题 |
|------|------|--------|--------|----------|
| TaskInput | 搜索框输入 task id | Inspection | `SubmitInspectionForProjectType` | 正常 |
| TaskBoard | completed task | Inspection | `SubmitInspectionForProjectType` | 正常 |
| Overview | `HasTask=true` 且状态允许 | Inspection | `SubmitInspectionForProjectType` | 正常 |
| Execution | 执行详情页 | Pipeline | `scheduler.Submit` | 绕过 Inspection 生命周期 guard |
| Overview 空/陈旧选择 | `SelectedItem` 失败或 `HasTask=false` | Pipeline 或无提示 | `scheduler.Submit` 或直接返回 | 用户没有明确反馈 |

Pipeline 路径仍会执行 `rerunStagePlan`、ref run 选择等 run config 检查，并且 scheduler 有 `activeByTask` 内存去重，pipeline 还有文件锁。因此准确说法不是“无任何检查”，而是：

> Ctrl+R 的 Pipeline fallback 绕过了 TaskActionService 的生命周期准入检查：task state、`CurrentRunID`、inspect capacity、初检文档、batch/gitURL/projectType 和 `sync_error` 语义。

需要同步修正文案：

- `submitRunConfig` 当前统一显示“正在提交流水线 job...”，Inspection 入口也会显示这个文案。
- 执行详情无运行记录时显示“按 Ctrl+R 启动流水线”，统一 Ctrl+R 后应改为“按 Ctrl+R 开始质检/重跑质检”。

### 2.2 Bug #2: 失败原因可观测性不足且错误分类混乱

**严重程度**: P0

#### 2.2.1 SubmitInspection 错误被写成 Git 同步错误

`dbTaskActionService.submitInspection` 当前对所有 `scheduler.SubmitInspection` 错误调用 `RecordTaskGitError`：

```go
_, err = s.scheduler.SubmitInspection(task.ID, task.BatchID, task.GitURL, opts)
if err != nil {
    s.store.RecordTaskGitError(ctx, task.ID, err)
}
```

这会把以下非 Git 错误写入 `tasks.sync_error`：

- `scheduler unavailable`
- `scheduler is shut down`
- `task already has an active job`
- future admission/capacity/DB 相关错误

结果：

- Task card 会显示“Git 同步失败”，误导用户。
- Ctrl+W 的 retry 语义被污染。
- 原因没有结构化分类，后续 diagnostics 只能解析字符串。

#### 2.2.2 Git sync 先清错再写错存在窗口

`runGitSync` 开始时先 `RecordTaskGitError(ctx, taskID, nil)` 清空旧错误，结束后再写入新错误。若进程在两者之间退出，旧上下文会丢失。

这不是最主要的根因，但应通过事件表或 “last_error_kind + last_error_at” 保留历史。

#### 2.2.3 `enrichTaskProject` 静默吞掉查询失败

`ListByState` 给 TaskBoard 加载任务卡片时会调用 `enrichTaskProject`。当前：

```go
run, err := s.store.LatestRunForTask(ctx, project.ID)
if err != nil {
    return
}
stages, err := s.store.Stages(ctx, run.RunID)
if err != nil {
    return
}
```

这导致 DB 锁、I/O 错误、stage 查询失败时，Task card 看起来像没有运行记录，而不是显示“运行信息加载失败”。

注意：`sql.ErrNoRows` 对 “没有历史 run” 是正常情况，不应显示错误；其他错误必须可见。

#### 2.2.4 prepareRun 失败缺少持久化证据

需要区分两类：

| 失败点 | 当前可见性 | 当前持久化 |
|--------|------------|------------|
| `loadAndValidateRunInputs` 失败 | `job.Err` 短期可见 | 无 run artifact |
| `acquireTaskRunLock` 失败 | `job.Err` 短期可见 | 无 run artifact |
| `prepareRun` 中 `MkdirAll`/docs gate/`CreateRun` 失败 | `job.Err` 短期可见 | `CreateRun` 前无 run artifact |
| `CreateRun` 后失败 | `job.Err` + defer crash | 可尝试 crash artifact |

因此原文“用户完全看不到失败原因”过度。真实问题是：**失败原因没有稳定、可查询、可关联 task 的持久化证据**。

#### 2.2.5 原始 DB 错误被替换

`loadAndValidateRunInputs` 当前把 `GetProject` 的任何错误替换成 `dbNotFoundTask(taskID)`。这会把 SQLite 锁、磁盘错误、schema 错误伪装成 task not found。

#### 2.2.6 Runner contract 漏洞

Scheduler 当前在 `runner.Run` 返回 `(Result{}, nil)` 时会调用 `applyResultLocked` 并标记 `JobDone`。真实 `Runner.Run` 正常应返回非空 `RunID`，但 scheduler 应显式防御：

```go
if err == nil && strings.TrimSpace(result.Run.RunID) == "" {
    job.State = JobFailed
    job.Err = errors.New("pipeline returned no error but produced no run record")
}
```

#### 2.2.7 业务错误被 `_ =` 丢弃

已确认需要优先处理的热点：

| 优先级 | 位置 | 问题 |
|--------|------|------|
| P0 | `internal/scheduler/scheduler.go:563,637,656,658` | Git/scheduler 错误写 DB 失败被丢弃 |
| P0 | `internal/pipeline/stage_b.go:266,277` | runtime/mirror summary 合并失败被丢弃 |
| P1 | `internal/docker/mirrors.go` 多处 `writeDaemonMirrorSummary` | 运维 summary 写入失败被丢弃 |
| P1 | `internal/docker/compose.go:65,375` | WalkDir/JSON 解析错误被丢弃 |
| P1 | `internal/tui/app.go:248-254,922` | signal cleanup/recover orphan inspection 的记录错误被丢弃 |

Close/Rollback、best-effort 临时文件清理不应混入 P0 业务错误清单。

### 2.3 Bug #3: Ctrl+E 和 Ctrl+S 竞态

**严重程度**: P0

当前 `ConfirmComplete` 和 `StartDocker` 都是 read-check-slow-act-write：

```
ConfirmComplete:
  GetTask
  if DockerRunning -> docker compose down
  SetLatestRunManualVerdict
  CompleteTask(state=completed, docker_running=0)

StartDocker:
  GetTask
  if state != waiting_manual -> reject
  docker compose up -d
  RecordTaskRuntime(docker_running=1)
  inspect ports
  RecordTaskRuntime(docker_running=1)
```

`CompleteTask` 的 SQL 有 `WHERE state='waiting_manual'`，但 `RecordTaskRuntime` 没有 state 条件：

```sql
UPDATE tasks
SET frontend_url=?, docker_running=?, compose_meta=?, updated_at=?
WHERE id=?
```

所以可能出现：

```
StartDocker 读到 waiting_manual
ConfirmComplete 完成任务并写 docker_running=0
StartDocker 的 docker compose up -d 成功
StartDocker 再写 docker_running=1
结果: completed + docker_running=1
```

此外，`SetLatestRunManualVerdict` 与 `CompleteTask` 不是同一个 DB 事务。若 verdict 写入成功、complete 失败，用户判定和 task 状态会短暂或长期不一致。重构后必须把人工 verdict 与 complete transition 合并进一个条件事务。

单进程 per-task mutex 可以缩小窗口，但不能解决跨 TUI 进程、进程重启、外部 CLI 并发。最终必须靠 DB 条件更新和运行时补偿清理。

---

## 3. 重构目标

1. **入口统一**：普通 Ctrl+R 只代表“质检/重跑质检”，不再隐式走 direct pipeline。
2. **状态可解释**：明确区分 Task、Run、Job 三套状态及不变量。
3. **错误可追溯**：每个失败场景都有分类、文本、时间、关联 task/run/job，并能被 TUI 查询。
4. **并发可控**：单进程使用 per-task mutex，跨进程依赖 DB CAS、条件更新、唯一约束和文件锁。
5. **异常可修复**：用户能诊断单个任务，并按确定策略恢复到可重提或保留完成态。
6. **测试可证明**：每个生命周期入口、异常状态、迁移约束都有对应测试。

---

## 4. 迭代后的总体设计

### 4.1 新增 TaskLifecycleManager

建议新增 `internal/tasklifecycle` 包；如果为了减少初期改动，也可以先在 `internal/tui` 内实现，后续迁移。

当前代码没有独立 `internal/domain` 生命周期层，领域逻辑实际散落在 `internal/tui/shared.go`、`internal/tui/app.go`、`internal/scheduler/scheduler.go`、`internal/db/store.go` 和 `internal/pipeline/recovery.go`。重构的第一目标是把写入型生命周期规则收敛到 Manager，而不是继续在这些包之间用 facade 互相补丁。

职责：

- 统一 TUI 的任务操作入口。
- 为同一任务提供进程内 mutex。
- 调用 DB 条件化方法完成最终一致性保护。
- 统一 admission guard、错误分类和 TUI message。

```go
type Manager struct {
    store     *db.Store
    cfg       config.Config
    scheduler SchedulerSubmitter
    exec      executor.CommandRunner

    mu        sync.Mutex
    taskLocks map[string]*sync.Mutex
}

type SchedulerSubmitter interface {
    Submit(context.Context, scheduler.SubmitRequest) (scheduler.SubmitResult, error)
    CancelTask(string) error
    ActiveSnapshot() []scheduler.JobSnapshot
    NotifyCh() <-chan struct{}
    Shutdown(context.Context) error
}

type Action string

const (
    ActionOpenInspection Action = "open_inspection"
    ActionSubmitInspection Action = "submit_inspection"
    ActionRetryGit Action = "retry_git"
    ActionStartDocker Action = "start_docker"
    ActionComplete Action = "complete"
    ActionDiagnose Action = "diagnose"
)
```

关键原则：

- mutex 只提供单进程串行化，不作为唯一一致性机制。
- 慢操作前后都要检查 DB 状态，写入必须有 SQL 条件。
- 对外返回结构化错误，而不是只返回字符串。

### 4.2 Admission Guard 取代 `canOpenInspectionRunConfig(state string)`

当前函数只看 state：

```go
func canOpenInspectionRunConfig(state string) bool {
    return state != model.TaskInspecting && state != model.TaskWaitingManual
}
```

这过于粗糙。不能改成 `return true`，因为 `waiting_manual` 任务仍应先人工判定。

新接口应基于完整 task 上下文：

```go
type AdmissionDecision struct {
    Allow   bool
    Reason  string
    Hint    string
    Action  string // open_inspection | retry_git | diagnose | complete_first
}

func EvaluateInspectionAdmission(task model.Task, snapshot RuntimeSnapshot) AdmissionDecision
```

规则建议：

| 当前状态 | 条件 | Ctrl+R 行为 |
|----------|------|-------------|
| completed | `CurrentRunID == NULL/""` | 允许打开 Inspection run config |
| completed | `CurrentRunID != ""` | 阻止：已有活跃运行 |
| waiting_manual | 任意 | 阻止：请先完成待处理判定；可提示 Ctrl+E/Ctrl+S |
| inspecting | `CurrentRunID != ""` 或 scheduler active job | 阻止：已有运行中任务 |
| inspecting | `sync_error != ""` 且无 active run | 阻止普通 Ctrl+R，提示 Ctrl+W 重试 Git |
| inspecting | 无 active run、无 sync_error、无 running run | 阻止普通 Ctrl+R，提示 Ctrl+Shift+R 诊断 |
| 无 task 记录 | TaskInput 新题入口 | 允许创建并开始 Inspection |
| Overview `HasTask=false` | 项目不是已创建 task | 阻止：请选择已创建任务或从输入框创建题目 |

### 4.3 Ctrl+R 统一入口

新增 `openInspectionRunConfigForCurrentContext()`，所有 Ctrl+R 都走它。删除默认 fall-through 到 `runConfigActionPipeline`。

```go
case "ctrl+r":
    return m.openInspectionRunConfigForCurrentContext(cmds)
```

伪代码：

```go
func (m *app) openInspectionRunConfigForCurrentContext(cmds []tea.Cmd) (app, []tea.Cmd) {
    switch {
    case m.focus == focusTaskInput:
        if m.openRunConfigForTaskInput() {
            return *m, cmds
        }
        return *m, cmds

    case m.tab == panelTaskBoard:
        task, ok := m.taskBoard.SelectedTask()
        if !ok {
            m.message = "请选择一个任务"
            return *m, cmds
        }
        return m.openInspectionRunConfigForTaskProject(task, cmds)

    case m.tab == panelOverview:
        item, ok := m.overview.SelectedItem()
        if !ok {
            m.message = "请选择一个已创建任务"
            return *m, cmds
        }
        if !item.HasTask {
            m.message = "该项目尚未创建任务，请从任务输入框开始质检"
            return *m, cmds
        }
        return m.openInspectionRunConfigForTaskID(item.TaskID, cmds)

    case m.tab == panelExecution:
        taskID := m.selectedTaskID()
        if strings.TrimSpace(taskID) == "" {
            m.message = "当前执行详情没有选中的任务"
            return *m, cmds
        }
        return m.openInspectionRunConfigForTaskID(taskID, cmds)
    }
    m.message = "当前页面不支持重跑"
    return *m, cmds
}
```

普通 run config 不再携带 action 分支。应删除 `runConfigActionPipeline` / `runConfigActionInspection` 这个 enum，run config 只表达“质检提交配置”。本轮重构删除 direct pipeline 普通入口，不实现旧 shortcut 的替代入口。未来新增调试/运维能力时，必须另建全新的 debug/admin 命令，不能复用普通 run config：

- 显示“直接运行流水线，不执行 Git 同步/文档/容量检查”的确认。
- 仍检查 `CurrentRunID` 和 scheduler active job。
- 不作为普通用户的默认 Ctrl+R 行为。

当前 `cmd/run.go` 的 `p2r run` 也会直接调用 `pipeline.NewRunner(...).Run`。它不能作为生命周期重构后的隐形后门继续存在：要么迁入 `TaskLifecycleManager`/scheduler submit flow，要么重命名/隔离为明确的 debug/admin 命令，并在命令层声明会绕过 Git sync/inspection admission，同时保留 task lock、`current_run_id` CAS 和运行唯一约束。

### 4.4 Scheduler 提交接口统一

这里不保留兼容 wrapper。`Submit` 和 `SubmitInspection` 的双入口正是生命周期分裂的根源之一，继续保留只会让新旧路径长期并存。重构应一次性删除旧方法，改为唯一提交入口：

```go
type SubmitRequest struct {
    TaskID  string
    Flow    JobFlow
    BatchID string
    GitURL  string
    Opts    pipeline.RunOptions
}

type JobFlow string

const (
    FlowPipelineDirect JobFlow = "pipeline_direct"
    FlowGitSyncThenPipeline JobFlow = "git_sync_then_pipeline"
    FlowGitSyncOnly JobFlow = "git_sync_only"
)

type SubmitResult struct {
    FlowID string
    JobID  string
}

func (s *Scheduler) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error)
```

`FlowPipelineDirect` 不是为了保留旧 Ctrl+R 路径。初始迁移不向 TUI/普通 CLI 暴露它；未来的调试/运维入口必须由独立命令构造该 flow，并接受同一套 active run/admission 检查。

必须同步删除：

- `func (s *Scheduler) Submit(taskID string, opts pipeline.RunOptions) (string, error)` 旧签名。
- `func (s *Scheduler) SubmitInspection(taskID, batchID, gitURL string, opts pipeline.RunOptions) (string, error)`。
- 未使用的 `JobInspection` kind 或将其改为 `SubmitRequest.Flow` 的派生展示字段。
- TUI 侧 `inspectionScheduler` 旧接口。
- 测试里的旧 fake scheduler 接口。

迁移策略不是“先兼容再慢慢改”，而是让编译失败暴露所有调用点，然后逐个改为显式 `SubmitRequest{Flow: ...}`。这样调用者必须在代码里声明意图，无法再误把普通 Ctrl+R 接到 direct pipeline。最终合并状态不得留下旧签名、adapter 或 wrapper。

同步变更：

- `JobSnapshot` 增加 `FlowID`、`ParentJobID`、`Flow`，让 TUI 把 GitSync + Pipeline 视为一次逻辑质检。
- scheduler 处理 `err == nil && result.Run.RunID == ""` 时强制 `JobFailed`。
- scheduler 写 DB 错误不再 `_ =`，至少进入 job.Err 或 structured warning。

### 4.5 删除其他生命周期兼容面

Scheduler 双入口只是问题的一部分。以下旧 facade 必须跟随统一架构一起删除，而不是标记 deprecated 后长期共存。

#### 4.5.1 删除 `TaskActionService` 多方法入口

当前 TUI action service 同时暴露：

```go
StartInspection
StartInspectionForProjectType
SubmitInspection
SubmitInspectionForProjectType
ReInspect
RetryGitSync
StartDocker
ConfirmComplete
```

这些方法把“新题质检、已有题重跑、Git 重试、人工完成、Docker 启动”拆成多个入口，导致校验和错误分类分散。重构后删除该接口，改成唯一 typed command：

```go
type LifecycleCommand struct {
    Kind        LifecycleCommandKind
    TaskID      string
    ProjectType string
    Verdict     string
    Flow        scheduler.JobFlow
    RunOptions  pipeline.RunOptions
}

type LifecycleCommandKind string

const (
    CommandSubmitInspection LifecycleCommandKind = "submit_inspection"
    CommandRetryGitSync LifecycleCommandKind = "retry_git_sync"
    CommandStartDocker LifecycleCommandKind = "start_docker"
    CommandCompleteManual LifecycleCommandKind = "complete_manual"
    CommandDiagnose LifecycleCommandKind = "diagnose"
    CommandRepair LifecycleCommandKind = "repair"
)

func (m *Manager) Execute(ctx context.Context, cmd LifecycleCommand) (LifecycleResult, error)
```

要求：

- `TaskQueryService` 可以保留为只读查询服务，但不能再混入生命周期写入。
- 删除 `dbTaskActionService` 作为 TUI 直接依赖的 facade。
- 删除 `taskActionCmd(action string, taskID string)` 的字符串 switch，TUI 必须构造 `LifecycleCommand`。
- 删除 `ReInspect` 独立入口；重检只是 `CommandSubmitInspection` 的 `RunOptions.Mode="recheck"`。
- 删除 `StartInspection`/`SubmitInspection` 命名差异；新题和已有题都进入 `CommandSubmitInspection`，由 admission 决策创建 task 或复用 task。
- 删除 `RetryGitSync` 独立提交路径；它是 `CommandRetryGitSync`，只允许 `sync_error` 的 Git 错误状态。

#### 4.5.2 删除 TUI 旧 scheduler 接口

当前：

```go
type schedulerClient interface {
    Submit(string, pipeline.RunOptions) (string, error)
    CancelTask(string) error
    ActiveSnapshot() []scheduler.JobSnapshot
    NotifyCh() <-chan struct{}
    Shutdown(context.Context) error
}

type inspectionScheduler interface {
    SubmitInspection(string, string, string, pipeline.RunOptions) (string, error)
}
```

重构后：

- `schedulerClient.Submit` 必须使用 `Submit(ctx, SubmitRequest)` 新签名。
- 删除 `inspectionScheduler`。
- `newTaskActionService(... scheduler schedulerClient)` 这层注入删除，Manager 直接依赖新 scheduler client。
- 所有 fake scheduler 和 test hook 必须按新请求模型重写，不允许提供旧 `SubmitInspection` fake。

#### 4.5.3 删除 recovery package-level wrapper 和启动补丁命令

当前 `pipeline.RecoverStaleRuns`、`RecoverOrphanedRuns`、`RecoverOrphanedRunForTask`、`RecoverInterruptedRuns` 都是 `NewRecoveryService(...).Method(...)` 的 package-level wrapper。TUI 又在 `newApp` 中手工拼：

```go
RecoverOrphanedRuns + RecoverStaleRuns + store.RepairTaskStates
RecoverOrphanedRunForTask + store.RepairTaskStates
```

同时 `recoverOrphanInspectionCmd` 还通过 `FindStaleInspecting -> SubmitInspection -> RecordTaskGitError` 修补 inspecting orphan。

这些都是旧生命周期分裂留下的补丁路径。重构后：

```go
type RecoveryRequest struct {
    Scope  RecoveryScope
    TaskID string
    Refs   []pipeline.RunReference
    Reason string
}

func (m *Manager) Recover(ctx context.Context, req RecoveryRequest) (RecoveryResult, error)
```

要求：

- 删除 package-level recovery wrappers；恢复能力改为显式 `RecoveryService` 或并入 `TaskLifecycleManager.Recover`。
- 删除 TUI 的 `recoverStaleRunsFn` / `recoverOrphanRunFn` 函数字段注入。
- 删除 `recoverOrphanInspectionCmd`。stale inspecting 由 `TaskDiagnostics` 或 `Manager.Recover` 统一处理，不再自动调用 SubmitInspection。
- `RepairTaskStates` 不再作为 TUI 启动时的兜底补丁随处调用；改为 recovery/diagnostic 事务内的显式 transition。

删除顺序：

1. 先新增 `Manager.Recover(ctx, RecoveryRequest)` 和 diagnostics transition。
2. 迁移 TUI 启动恢复、poller 周期恢复、Ctrl+X orphan recovery 到 typed recovery request。
3. 删除 `recoverOrphanInspectionCmd` 的自动重提交流程。
4. 删除 package-level wrappers 和测试中的函数注入点。

#### 4.5.4 删除 DB 层直接生命周期写入口

DB Store 里这些方法目前可被不同上层直接调用：

```go
CreateTaskWithBatch
CreateRun
FinishRun
RecordTaskGitError
RecordTaskTerminalGitError
ReopenTaskForInspection
CompleteTask
RepairTaskStates
FinishRunAndTransitionTask // 只是 FinishRun facade，也要删除
```

重构后不保留这些分散写入口作为公开生命周期 API。替换为：

```go
ApplyTaskTransition(ctx, TaskTransition) (model.Task, error)
RecordTaskEvent(ctx, TaskEvent) error
SetGitSyncError(ctx, GitSyncErrorUpdate) error // 仅 scheduler git sync flow 可调用
```

要求：

- `CreateTaskWithBatch` 只能由 `CommandSubmitInspection` 的新题分支调用，不能作为任意上层创建 inspecting task 的便捷入口。
- `CreateRun` 必须改为 run-start transition：要求 admission 已通过，检查 task 当前状态、`current_run_id IS NULL`、running run 唯一约束；不能让 direct pipeline 把 `waiting_manual` 或异常 task 直接改回 `inspecting`。
- `FinishRun` 正常路径必须验证 run 存在、`runs.task_id == taskID`、原 status 为 `running`，并要求 task `current_run_id = runID`；允许 `current_run_id IS NULL` 的修复只能走 recovery/diagnostic 专用 API。
- TUI/Manager 不直接调用 `RecordTaskGitError` 写任意错误。
- `RecordTaskTerminalGitError` 迁入 GitSync flow 的 transition，至少要求 `state='inspecting' AND current_run_id IS NULL`，不能无条件 terminalize 任意 task。
- `CompleteTask` 语义并入 `ApplyTaskTransition(CommandCompleteManual)`，保证 verdict、Docker cleanup、state transition 在同一 DB 事务下完成，至少要求 `state='waiting_manual' AND current_run_id IS NULL`。
- `ReopenTaskForInspection` 删除；重新质检只能通过 `CommandSubmitInspection` + admission + scheduler flow。
- `RepairTaskStates` 当前只是历史补丁：只能覆盖部分 `inspecting + current_run_id` 和 aborted/crashed latest run 组合，不能处理 waiting/completed 携带 terminal `current_run_id`、run/task 不匹配、latest completed 但 task 未转 waiting 等情况。应删除或下沉为 recovery/diagnostics 内部枚举修复函数，不能作为长期运行时修补器。

#### 4.5.5 删除旧测试 hook 镜像

测试不能继续围绕旧 facade 提供便利函数，否则会把旧架构固化下来。需要删除或重写：

- `InspectionSchedulerForTest`
- `StartInspectionForTest`
- `StartInspectionForProjectTypeForTest`
- `SubmitInspectionForProjectTypeForTest`
- 任何只为旧 `TaskActionService` 服务的 fake scheduler
- `tests/internal/scheduler/scheduler_test.go` 中直接调用旧 `Submit` / `SubmitInspection` 的用例
- `tests/internal/tui/taskaction_test.go` 中围绕 `dbTaskActionService` 的用例

替换为：

- `NewLifecycleManagerForTest`
- `ExecuteLifecycleCommandForTest`
- `SubmitRequestRecorder`
- diagnostics/recovery 专用 probe

### 4.6 错误分类与持久化

#### 4.6.1 错误分类

新增错误类型：

```go
type LifecycleError struct {
    Kind    ErrorKind
    TaskID  string
    RunID   string
    JobID   string
    Stage   string
    Message string
    Cause   error
}

type ErrorKind string

const (
    ErrorGitRetryable ErrorKind = "git_retryable"
    ErrorGitTerminal ErrorKind = "git_terminal"
    ErrorSchedulerRejected ErrorKind = "scheduler_rejected"
    ErrorAdmissionRejected ErrorKind = "admission_rejected"
    ErrorPrepareFailed ErrorKind = "prepare_failed"
    ErrorDBFailed ErrorKind = "db_failed"
    ErrorLockFailed ErrorKind = "lock_failed"
    ErrorDockerFailed ErrorKind = "docker_failed"
    ErrorPipelineCrashed ErrorKind = "pipeline_crashed"
    ErrorCancelled ErrorKind = "cancelled"
)
```

规则：

- `tasks.sync_error` 只存 Git sync runner 产生的 retryable/terminal Git 错误。
- scheduler/admission/DB/lock/docker 错误不写 `sync_error`。
- TUI message 使用 `LifecycleError.Kind` 决定文案，而不是解析字符串。

#### 4.6.2 持久化方案

P0 可以先不改 schema，只做两件事：

- 非 Git submit error 不写 `sync_error`。
- `prepareRun` 在 `CreateRun` 前失败时写 `.qa-control/run-failures/<taskID>_<timestamp>.json`，TUI diagnostics 可读取。

P1 推荐 schema v7：

```sql
CREATE TABLE task_events (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    run_id TEXT,
    job_id TEXT,
    kind TEXT NOT NULL,
    message TEXT NOT NULL,
    source TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX idx_task_events_task_created ON task_events(task_id, created_at DESC);
```

可选地给 `tasks` 增加 last error cache：

```sql
ALTER TABLE tasks ADD COLUMN last_error_kind TEXT DEFAULT '';
ALTER TABLE tasks ADD COLUMN last_error TEXT DEFAULT '';
ALTER TABLE tasks ADD COLUMN last_error_at TEXT DEFAULT '';
```

### 4.7 DB 原子约束与新 API

#### 4.7.1 `current_run_id` 必须使用 NULL

所有 reset/recovery API 必须遵守：

```sql
SET current_run_id = NULL
```

不能写 `''`。现有 `CreateRun` 依赖：

```sql
WHERE id = ? AND current_run_id IS NULL
```

#### 4.7.2 条件化 runtime 写入

为 TUI StartDocker 使用专用 API，不复用无条件 `RecordTaskRuntime`：

```go
func (s *Store) MarkTaskDockerStartedIfWaiting(ctx context.Context, taskID, frontendURL string, meta model.ComposeMeta) error
```

SQL：

```sql
UPDATE tasks
SET frontend_url = ?,
    docker_running = 1,
    compose_meta = ?,
    updated_at = ?
WHERE id = ?
  AND state = 'waiting_manual'
  AND docker_running = 0;
```

如果 docker up 成功但 DB 条件更新失败：

- 立即执行 compensating `docker compose down`。
- 返回 `LifecycleError{Kind: ErrorDockerFailed}` 或 `ErrorAdmissionRejected`。
- 不允许把 completed task 写回 `docker_running=1`。

Pipeline Stage B 通过 `PutStageAndRecordTaskRuntime` 同时写 stage 和 task runtime。本次重构已把 task runtime 更新改为 `WHERE id=? AND current_run_id=?`，后续如引入新 API，应继续保持 run 绑定语义：

```go
func (s *Store) PutStageAndRecordTaskRuntimeIfActiveRun(
    ctx context.Context,
    runID string,
    stage model.StageRecord,
    taskID string,
    frontendURL string,
    dockerRunning bool,
    meta model.ComposeMeta,
) error
```

要求：

- 先验证 `runs.run_id=runID AND runs.task_id=taskID AND runs.status='running'`。
- task runtime 写入要求 `tasks.id=taskID AND tasks.current_run_id=runID`。
- 条件失败时 pipeline 返回结构化错误或执行补偿清理，不允许写 completed/waiting task 的 runtime。

#### 4.7.3 CompleteTask 强化

`CompleteTask` 当前 SQL 有 state 条件，但操作序列仍可能竞态。重构后：

- `ConfirmComplete` 在 Manager per-task lock 内运行。
- DB 更新仍保留 `WHERE state='waiting_manual'`。
- verdict 与 complete transition 合并为同一事务，要求 `current_run_id IS NULL`，并检查 latest run 属于该 task。
- 若任务曾记录 `docker_running=true`，cleanup 成功后再 complete。
- 若 complete 成功后发现 Docker 仍运行，记录 task event 并提示 diagnostics。

#### 4.7.4 FinishRun 强化

本次已完成：

- `UPDATE runs` 检查 RowsAffected。
- `UPDATE runs` 要求 `runs.task_id == taskID`。
- 只允许 `status='running'` 的 run finish 到 terminal。
- task 更新要求 `current_run_id = runID`，不再通过 `(current_run_id IS NULL)` 驱动正常 finish。

后续 recovery/diagnostic 若需要修复 legacy `current_run_id IS NULL` 的 running run，应使用显式 recovery API，而不是放宽正常 `FinishRun`。

#### 4.7.5 Running run 唯一约束

schema v7 建议添加：

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_one_running_per_task
ON runs(task_id)
WHERE status = 'running';
```

迁移前必须先恢复/终端化历史重复 running runs。

### 4.8 TaskDiagnostics 诊断与修复

#### 4.8.1 定位

`TaskDiagnostics` 是主动工具，不替代现有 `RecoveryService`：

| 维度 | RecoveryService | TaskDiagnostics |
|------|-----------------|-----------------|
| 触发 | 启动/轮询/Ctrl+X | 用户 Ctrl+Shift+R |
| 范围 | running run 恢复 | 单 task 五维诊断 |
| 可见性 | 主要静默 | 报告、确认、修复摘要 |
| 数据 | DB + artifact + lock + runtime | DB + run + lock + docker + scheduler + task events |

当前 pipeline 的 lock helper 未导出。落地方式二选一：

1. 在 `internal/pipeline` 导出 `TaskRunLockStatus(scanPath, taskID)` 和 `TaskRunLockPath(scanPath, taskID)`。
2. 将 diagnostics 的 lock 相关实现放入 pipeline/lifecycle 子包，由 TUI 调用公开服务。

#### 4.8.2 诊断快照

```go
type TaskStateSnapshot struct {
    Task          model.Task
    Runs          []model.RunRecord
    StagesForRun  map[string][]model.StageRecord
    Lock          LockSnapshot
    Docker        DockerSnapshot
    ActiveJobs    []scheduler.JobSnapshot
    Events        []TaskEvent
    FailureFiles  []RunFailureFile
    CollectedAt   time.Time
}
```

#### 4.8.3 诊断规则

| Code | 严重度 | 匹配条件 | 修复策略 |
|------|--------|----------|----------|
| `INSPECTING_WITHOUT_RUN` | critical | state=inspecting, no run, no active job, no sync_error | terminal reset |
| `INSPECTING_WITHOUT_JOB` | critical | state=inspecting, no active scheduler job, current_run_id 指向 running run 或历史 run | recover run 或 terminal reset |
| `RUN_STUCK_RUNNING` | critical | run.status=running 且超过预期时间 | recover/mark crashed |
| `RUN_WITHOUT_STAGES` | high | run 存在但 stage 记录为空，且非刚创建 | mark crashed |
| `INSPECTING_DOUBLE_RUN` | critical | 同 task 多个 running run | 保留最新，其他 mark crashed |
| `ORPHANED_LOCK_FILE` | high | lock 存在且 PID dead / task mismatch | 删除 stale lock |
| `DOCKER_LEAKED_COMPLETED` | high | state=completed 且 Docker 实际仍运行或 DBRunning=true | stop leaked docker |
| `DOCKER_LEAKED_INSPECTING` | high | inspecting 且无 active run 但 Docker 仍运行 | stop docker + terminal reset |
| `STALE_WAITING_MANUAL` | info | waiting_manual 超过阈值 | 仅提示，不自动完成 |
| `PRE_RUN_FAILURE_ONLY` | warning | 有 run-failure 文件但无 DB run | 显示失败并允许重新提交 |

#### 4.8.4 修复策略

修复策略保留简单，但不再采用“保持原 state + 放开 Ctrl+R”的方案。

| Policy | 适用 | 动作 |
|--------|------|------|
| `StopLeakedDocker` | completed 任务仅 Docker 泄漏 | `docker compose down` + `MarkTaskDockerStopped`，保留 completion/verdict |
| `TerminalReset` | inspecting 异常、stale run、double run、orphan lock 等 | 停 Docker、删除 stale lock、将 running run 标为 crashed、`current_run_id=NULL`、`docker_running=0`、`state=completed`、记录 task event |
| `NoFix` | waiting_manual stale 或健康任务 | 只显示说明 |

`TerminalReset` 设置 `state=completed` 是有意设计：

- 当前 schema 没有 `failed`/`ready` 状态。
- 现有 `FinishRun` 对 crashed/aborted run 也会把 task 转为 `completed`。
- 这样普通 Ctrl+R 仍可以通过 completed admission guard，而不需要把 `canOpenInspectionRunConfig` 放成永远 true。

DB API 示例：

```go
func (s *Store) TerminalResetTaskForRerun(ctx context.Context, taskID string, reason string) error {
    return s.withWriteTx(ctx, func(tx *sql.Tx) error {
        now := time.Now().UTC().Format(time.RFC3339)

        // 1. Mark running runs as crashed. The implementation must check rows.
        _, err := tx.ExecContext(ctx, `
            UPDATE runs
            SET status = ?, finished_at = ?, duration_ms = CASE
                WHEN started_at <> '' THEN CAST((julianday(?) - julianday(started_at)) * 86400000 AS INTEGER)
                ELSE duration_ms
            END
            WHERE task_id = ? AND status = ?`,
            model.RunCrashed, now, now, taskID, model.RunRunning)
        if err != nil {
            return err
        }

        // 2. Terminalize task and clear blockers. current_run_id must be NULL.
        result, err := tx.ExecContext(ctx, `
            UPDATE tasks
            SET state = ?,
                current_run_id = NULL,
                docker_running = 0,
                frontend_url = '',
                compose_meta = '',
                sync_error = '',
                updated_at = ?
            WHERE id = ?`,
            model.TaskCompleted, now, taskID)
        if err != nil {
            return err
        }
        return requireAffected(result, "task", taskID)
    })
}
```

如果引入 `task_events`，上述事务应同时写入 diagnostic reset 事件。

---

## 5. 分阶段实施计划

### Phase 1: P0 行为修复，不强制迁移 schema

目标：先消除用户可见的错误入口和竞态。

1. **统一 Ctrl+R**
   - 删除 `keymap.go` 中 Ctrl+R fallback 到 `runConfigActionPipeline`。
   - 删除 `runConfigActionPipeline` / `runConfigActionInspection` enum，run config 只保留质检提交模型。
   - 删除 TUI 普通 `submitRun` direct pipeline 提交路径；Execution/TaskBoard/Overview/TaskInput 全部走统一质检配置或明确提示。
   - Overview `HasTask=false`、`SelectedItem` 失败、stale selectedID 均给出提示。
   - 更新“启动流水线”相关文案。

2. **引入完整 admission guard**
   - 用 `EvaluateInspectionAdmission(task, snapshot)` 替代 `canOpenInspectionRunConfig(state string)`。
   - `waiting_manual` 保持阻止。
   - `inspecting + sync_error` 提示 Ctrl+W。
   - 异常 inspecting 提示 Ctrl+Shift+R 诊断。

3. **修复 submitInspection 错误分类**
   - 只有 Git sync runner 的错误写入 `sync_error`。
   - scheduler unavailable/shutdown/active job/capacity/admission 错误直接返回 TUI，不写 Git 错误。
   - 现有测试 `TestStartInspectionRecordsSyncErrorWhenSubmitFails` 需要改为“scheduler down 不写 sync_error”。

4. **消除 Ctrl+E/Ctrl+S 竞态**
   - 在 TaskLifecycleManager 内为 `StartDocker` 和 `ConfirmComplete` 加 per-task mutex。
   - 为 TUI StartDocker 增加 `MarkTaskDockerStartedIfWaiting` 条件更新。
   - 条件更新失败后自动 `docker compose down` 补偿。
   - TUI `StartDocker` 不再调用无条件 `RecordTaskRuntime`；pipeline Stage B 也要迁到 `PutStageAndRecordTaskRuntimeIfActiveRun`。

5. **修复 `enrichTaskProject` 静默错误**
   - `sql.ErrNoRows` 当作无历史 run。
   - 其他错误写入 `TaskProject.RunStatus="error"`、`FailedSummary="加载运行信息失败: ..."`。
   - `Stages` 查询失败同样展示“加载阶段信息失败”。

6. **保留原始 DB 错误**
   - `loadAndValidateRunInputs` 使用 `fmt.Errorf("load project %s: %w", taskID, err)`。
   - 只有 `errors.Is(err, sql.ErrNoRows)` 时才格式化为 task not found。

7. **防御 nil-result**
   - scheduler 在 `err == nil && result.Run.RunID == ""` 时标记 JobFailed。

### Phase 2: P1 生命周期结构化

目标：建立可追溯、可诊断、可跨进程证明的一致性模型。

1. **新增 `SubmitRequest` / `JobFlow`**
   - 统一 git sync then pipeline、git sync only；direct pipeline 只允许作为独立 debug/admin flow，不作为普通 TUI/CLI 默认入口。
   - 删除旧 `Submit(taskID, opts)` 和 `SubmitInspection(taskID, batchID, gitURL, opts)`，不保留 wrapper。
   - 全量迁移 TUI、recovery、tests/fakes 调用点，让编译器强制发现遗漏。
   - `JobSnapshot` 增加 flow/parent-child 信息。

2. **删除旧生命周期 facade**
   - 删除 `TaskActionService`、`dbTaskActionService`、`inspectionScheduler` 和 TUI `taskActionCmd(action string, ...)`。
   - 新增 `TaskLifecycleManager.Execute(ctx, LifecycleCommand)`，所有 TUI 写操作都构造 typed command。
   - 删除 `StartInspectionForTest`、`SubmitInspectionForProjectTypeForTest` 等测试镜像，改用 `ExecuteLifecycleCommandForTest`。

3. **统一 recovery 入口**
   - 删除 `pipeline.RecoverStaleRuns` 等 package-level wrappers。
   - 删除 TUI `recoverStaleRunsFn` / `recoverOrphanRunFn` 函数字段和 `recoverOrphanInspectionCmd`。
   - `RepairTaskStates` 下沉到 recovery/diagnostics 的显式 transition，不作为启动兜底补丁。

4. **替换 DB 直接生命周期写入口**
   - TUI/Manager 不直接调用 `RecordTaskGitError`、`CompleteTask`、`ReopenTaskForInspection`、`RepairTaskStates`。
   - `CreateRun`、`FinishRun`、`PutStageAndRecordTaskRuntime` 纳入 run/task CAS，禁止绕过 `current_run_id=runID` 的状态写入。
   - 用 `ApplyTaskTransition` / `RecordTaskEvent` / `SetGitSyncError` 表达状态写入意图。
   - `FinishRunAndTransitionTask` wrapper 删除，正常 finish 与 recovery finish 使用不同显式 API。

5. **新增结构化错误**
   - `LifecycleError` / `ErrorKind`。
   - TUI message 和 task card 不再解析字符串。

6. **新增 pre-run failure 持久化**
   - `CreateRun` 前失败写 `.qa-control/run-failures/*.json`。
   - diagnostics/TUI 可按 taskID 查询并显示。

7. **新增 TaskDiagnostics**
   - Ctrl+Shift+R 打开诊断报告。
   - Enter 执行 `StopLeakedDocker` 或 `TerminalReset`。
   - Esc/q 关闭。
   - 修复日志写 `.qa-control/logs/diagnostic_<task>_<time>.log`。

8. **修复业务 `_ =` 热点**
   - scheduler DB 写错进入 job.Err 或 warning。
   - Stage B summary merge 错误进入 artifact warning 或返回错误。
   - Docker mirror summary 写错集中处理并记录。
   - Signal cleanup 错误写 error log 或 task event。

### Phase 3: P1/P2 DB schema v7

目标：把生命周期不变量下沉到 DB。

1. **迁移前清理**
   - 恢复/终端化重复 running run。
   - 将任何历史 `current_run_id=''` 规范化为 `NULL`。

2. **约束与索引**
   - `runs.status` CHECK。
   - `runs.manual_verdict` CHECK。
   - `run_stages.status` CHECK。
   - `UNIQUE INDEX runs(task_id) WHERE status='running'`。

3. **事件表**
   - 新增 `task_events`。
   - 可选 `tasks.last_error_kind/last_error/last_error_at` cache。

4. **FinishRun 强化**
   - 已完成：所有 UPDATE 检查 RowsAffected。
   - 已完成：正常路径要求 run 属于 task、原状态为 running、task `current_run_id=runID`。
   - 后续：recovery/diagnostic 使用显式 API，不与正常 finish 混用。

---

## 6. 测试矩阵

### 6.1 TUI Ctrl+R 与 admission

| 场景 | 期望 |
|------|------|
| TaskInput 输入合法 task id + Ctrl+R | 打开统一质检 run config |
| TaskBoard completed + Ctrl+R | 打开统一质检 run config |
| TaskBoard waiting_manual + Ctrl+R | 不打开，提示先完成待处理 |
| TaskBoard inspecting + current_run_id | 不打开，提示已有运行 |
| TaskBoard inspecting + sync_error | 不打开普通重跑，提示 Ctrl+W |
| TaskBoard inspecting + no run/no job/no sync_error | 不打开，提示 Ctrl+Shift+R 诊断 |
| Overview `HasTask=false` + Ctrl+R | 不打开 Pipeline，提示从任务输入框创建 |
| Overview stale selectedID + Ctrl+R | 不打开 Pipeline，提示重新选择任务 |
| Execution completed + Ctrl+R + 无文档 Enter | 走 Inspection，显示文档必填 |
| Execution waiting_manual/inspecting active + Ctrl+R | 阻止并显示具体原因 |
| 普通 Ctrl+R 路径 | 不存在 direct pipeline fallback |

### 6.2 错误分类

| 场景 | 期望 |
|------|------|
| scheduler nil/shutdown | TUI 显示提交失败，不写 `sync_error` |
| active job duplicate | TUI 显示已有运行，不写 `sync_error` |
| Git auth failed retryable | 写 `sync_error`，Ctrl+W 可重试 |
| Git terminal not found | 通过 GitSync transition 记录 terminal Git 错误并 terminalize task |
| `GetProject` database locked | 错误链保留原始 DB 错误 |
| `prepareRun` mkdir/docs/CreateRun 失败 | 有 `job.Err`，并有 pre-run failure 持久化 |
| runner returns empty result nil error | scheduler 标 JobFailed |

### 6.3 并发与 DB

| 场景 | 期望 |
|------|------|
| Ctrl+E 与 Ctrl+S 同任务并发 | 不出现 completed + docker_running=1 |
| 两个 Store 实例并发 CreateRun | 至多一个成功 |
| Direct pipeline 尝试从 waiting_manual CreateRun | 被 admission/run-start transition 阻止 |
| StartDocker docker up 成功但 DB 条件更新失败 | 自动 compose down 补偿 |
| Stage B runtime 写入时 task current_run_id 已变化 | 写入失败并触发补偿/错误，不改旧 task |
| Verdict 写入成功但 complete 条件失败 | 同事务回滚，不留下 verdict/state 不一致 |
| FinishRun run/task mismatch | 返回错误，不改 task |
| FinishRun 非 running run | 返回错误或幂等策略明确 |
| TerminalReset 使用 NULL | 后续 CreateRun 能通过 `IS NULL` CAS |

### 6.4 Diagnostics

| 规则 | 测试 |
|------|------|
| `INSPECTING_WITHOUT_RUN` | 报告 critical，Enter 后 task -> completed/current_run_id NULL |
| `RUN_STUCK_RUNNING` | mark crashed，生成 crash summary 或 task event |
| `ORPHANED_LOCK_FILE` | stale lock 被删除，live lock 不删除 |
| `DOCKER_LEAKED_COMPLETED` | 只停 Docker，保留 verdict/completion |
| `INSPECTING_DOUBLE_RUN` | 只保留最新 running 或全部 terminalize，策略固定 |
| Docker daemon unavailable | 诊断报告 warning，不误判为健康 |
| malformed lock/manifest/port_map | 报告采集错误，不 panic |

### 6.5 删除旧入口的静态门禁

| 搜索目标 | 期望 |
|----------|------|
| `func (s *Scheduler) SubmitInspection` | 不存在 |
| `func (s *Scheduler) Submit(taskID string, opts pipeline.RunOptions)` | 不存在 |
| `JobInspection` | 不存在，或被明确改为非提交入口的展示字段 |
| `type inspectionScheduler` | 不存在 |
| `runConfigActionPipeline` / `runConfigActionInspection` | 不存在 |
| `TaskActionService` / `dbTaskActionService` | 不存在 |
| `taskActionCmd(action string` | 不存在 |
| `submitRun(taskID string` | 不存在 |
| `cmd/run.go` 直接调用 `pipeline.NewRunner(...).Run` | 不存在，除非命令已迁为明确 debug/admin 路径 |
| `recoverOrphanInspectionCmd` / `recoverStaleRunsFn` / `recoverOrphanRunFn` | 不存在 |
| `InspectionSchedulerForTest` / `StartInspectionForTest` / `SubmitInspectionForProjectTypeForTest` | 不存在 |
| `internal/tui` 中直接调用 `RecordTaskGitError` | 不存在 |
| `tests/internal/scheduler` 中直接调用旧 `Submit` / `SubmitInspection` | 不存在 |

---

## 7. 风险与注意事项

1. **不要把 `canOpenInspectionRunConfig` 放开成 `true`**。这会让 `waiting_manual` 直接重跑，绕过人工判定语义。
2. **per-task mutex 不是跨进程锁**。它必须与 DB 条件更新、唯一索引、文件锁共同使用。
3. **`current_run_id` 空值必须是 `NULL`**。空字符串会破坏 FK 和 `CreateRun` CAS。
4. **`sync_error` 只保留 Git 语义**。非 Git 错误应进入 task event 或 TUI message。
5. **普通 direct pipeline 入口直接删除**。未来新增调试/运维能力时，另建新命令、新权限和新测试，不复用旧 run config。
6. **CLI direct run 不能成为后门**。`p2r run` 要么走生命周期提交，要么被明确降级为 debug/admin 命令并带同等 CAS/lock 保护。
7. **runtime 写入必须绑定 run/task**。Stage B 不能继续用无条件 task runtime update。
8. **diagnostic reset 要 terminalize，而不是保持异常 state**。在当前三态 schema 下，异常 `inspecting` 修复后应转为 `completed`，再允许 Ctrl+R 重新质检。
9. **schema v7 需要迁移前清理**。唯一 running 索引必须在重复 running run 被恢复后再创建。

---

## 8. 建议文件变更清单

| 文件 | 阶段 | 变更 |
|------|------|------|
| `internal/tui/keymap.go` | P0 | Ctrl+R 统一入口，删除 Pipeline fallback，新增 Ctrl+Shift+R diagnostics |
| `internal/tui/runconfig.go` | P0 | 删除 action enum 和 direct pipeline branch，submit 只生成生命周期命令 |
| `internal/tui/render.go` | P0 | 删除基于 run config action 的渲染分支，统一质检配置展示 |
| `internal/tui/viewmodel.go` | P0 | “启动流水线”文案改为“开始质检/重跑质检” |
| `internal/tui/shared.go` | P0/P1 | 保留只读查询，删除 `TaskActionService`/`dbTaskActionService`/`inspectionScheduler` |
| `internal/tui/app.go` | P0/P1 | 删除 `submitRun`/`submitInspection`/字符串 action switch/recovery 函数字段/`recoverOrphanInspectionCmd` |
| `internal/tui/poller.go` | P0/P1 | 不再周期性调用旧 orphan inspection 补丁命令 |
| `internal/tui/tasktype.go` | P0/P1 | 项目类型选择后进入统一质检命令，不再传 action enum |
| `internal/tui/testhooks.go` | P1 | 删除旧测试 hook/fake，改为 lifecycle manager 测试入口 |
| `tests/internal/scheduler/scheduler_test.go` | P1 | 按 `SubmitRequest`/`JobFlow` 重写 scheduler 测试 |
| `tests/internal/tui/taskaction_test.go` | P1 | 删除旧 action facade 测试，改测 lifecycle command |
| `cmd/run.go` | P1 | 迁入生命周期提交，或重命名为 debug/admin direct run 并补 admission/CAS 文案 |
| `internal/tasklifecycle/manager.go` 或 `internal/tui/lifecycle_manager.go` | P0/P1 | 统一任务操作入口、per-task mutex、错误分类 |
| `internal/tasklifecycle/diagnostics.go` 或 `internal/tui/diagnostics.go` | P1 | 诊断快照、规则、修复策略 |
| `internal/db/store.go` | P0/P1 | 条件 runtime API、run/task CAS、TerminalReset API、ApplyTaskTransition/RecordTaskEvent/SetGitSyncError、删除直接生命周期写 wrapper |
| `internal/db/migrate.go` | P1/P2 | schema v7、task_events、CHECK、unique running index |
| `internal/scheduler/scheduler.go` | P0/P1 | nil-result 防御、SubmitRequest、flow snapshot、错误不丢弃 |
| `internal/pipeline/run_lifecycle.go` | P0/P1 | 保留原始 DB 错误、pre-run failure 持久化 |
| `internal/pipeline/recovery.go` | P1 | 删除 package-level wrappers，改为显式 RecoveryService/Manager.Recover |
| `internal/pipeline/cleanup.go` | P1 | 导出 lock snapshot 或提供 diagnostics helper |
| `internal/pipeline/stage_b.go` | P1 | summary merge 错误不再丢弃 |
| `internal/docker/mirrors.go` | P1 | mirror summary 写入失败集中记录 |
| `internal/docker/compose.go` | P1 | WalkDir/JSON 解析错误处理 |

---

## 9. 子代理调研摘要

本次使用 3 个并行 explorer 子代理交叉核验：

| 视角 | 主要结论 |
|------|----------|
| DB/持久化 | `writeMu` 只保护写事务；`current_run_id` 必须为 NULL；需要 DB 条件更新和可选 schema v7；原文 reset 示例错误 |
| Scheduler/Pipeline | Job/Run/Task 三套状态机需拆分；GitSync 成功后会派生 Pipeline job；prepareRun 早期失败缺少持久化但不等于完全不可见 |
| TUI/测试 | Ctrl+R fall-through 属实；`canOpenInspectionRunConfig=true` 不可落地；需补 Execution/Overview stale/Inspection admission 测试 |

---

## 10. 本版相对 v4 的关键更改

1. 将单一“任务状态机”拆为 Task、Run、Job 三套状态机，并明确不变量。
2. 修正 `JobGitSync` 语义：它不是完整 inspection job，Git 成功后会派生 `JobPipeline`。
3. 修正 “PATH 4 无任何检查” 为 “绕过生命周期 guard”。
4. 删除 `canOpenInspectionRunConfig return true` 方案，改为完整 admission guard。
5. 修正 reset SQL：`current_run_id` 必须写 `NULL`，不能写 `''`。
6. 将 diagnostic reset 策略改为 terminalize 到 `completed`，避免放开 waiting_manual/inspecting 普通重跑。
7. 补充 DB 条件化 runtime 写入和 StartDocker 失败补偿，防止 completed 后写回 `docker_running=1`。
8. 把错误可观测性从“全部不可见”改为“短期 job 可见但缺少稳定持久化”，并提出 pre-run failure/task_events。
9. 把 `_ = err` 清单改为业务热点优先级，不再把 best-effort Close/Rollback 混入 P0。
10. 增加可执行测试矩阵，覆盖 Ctrl+R、错误分类、并发、diagnostics 和 DB invariant。
11. 删除“短期兼容 wrapper”迁移路线，改为一次性删除 Scheduler、TUI、Recovery、DB、测试层旧生命周期入口。
