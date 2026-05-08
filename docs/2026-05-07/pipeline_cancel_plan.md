# Pipeline 终止功能实现计划

Date: 2026-05-07
Status: Ralph-reviewed 二次修订版，待实现

## 0. Ralph 复核结论

本轮按 oh my codex Ralph 循环修订：先读取目标文档，再对照 `internal/scheduler/scheduler.go`、`internal/pipeline/pipeline.go`、`internal/pipeline/stage_*.go`、`internal/executor/`、`internal/tui/app.go`、`internal/tui/keymap.go`、`internal/tui/layout.go`、`internal/tui/render.go`、`internal/tui/pipelineview.go` 和现有测试，最后把发现的问题直接回写到本文档。

原草案的主要缺陷已经在本文修正：

1. **目标与非目标矛盾**：原文说“不支持终止排队中的单个作业”，但又要求 `CancelTask` 覆盖排队和运行中。修正为：Scheduler 和 TUI 都支持取消当前选中任务的排队或运行中 job。
2. **排队 job 取消会破坏并发槽位**：当前 scheduler 只有真正启动的 job 才占用 `sem`。原计划在取消 `JobQueued` 时调用 `releaseSlotLocked()`，会误释放正在运行 job 的槽位，造成超过 `max_concurrent` 的并发。修正为：取消 queued job 只移出队列和 `activeByTask`，绝不释放 `sem`。
3. **取消请求不等于已终止**：`CancelTask` 只能发出取消信号，真正退出要等 pipeline 保存 aborted run 后由 scheduler 通知。TUI 文案必须区分“已发送终止请求”和“已终止”。
4. **pipeline 持久化时机错误**：原计划在 `ctx` 已取消后仍先用同一个 `ctx` 写 `PutStage`、`InsertFindings`、`FinishRun`，这些写入会失败，并可能触发现有 defer 把 run 标成 `crashed`。修正为统一走 bounded background context 的 abort 持久化 helper，并设置 `runFinished = true`。
5. **Stage A 取消不生效**：`stageA` 的 Python 脚本当前通过 `context.Background()` 执行，不会响应 scheduler 的父 `ctx` 取消。修正为把 `ctx` 贯通到 `stageA`、`runStageAScript` 和 `pythonInvocation` 的版本探测。
6. **运行时清理会被跳过**：如果 B/C 已经创建 Docker runtime，取消后直接 abort 会绕过正常 `cleanupCurrentRuntime`。修正为 abort helper 在需要时用短 background cleanup context 尝试清理，并记录 `cleanup_summary.json`。
7. **scheduler 状态更新分散**：原计划把取消判断直接塞进 `runJob`，会继续扩大重复的结果拷贝和状态写入。修正为增加小型私有 helper，集中处理 result snapshot、取消完成、队列移除。
8. **TUI 活跃 job 判断过窄**：原文只判断 `JobRunning`，导致 queued job 无法从 UI 取消。修正为 `activeJobForTask` 同时识别 queued/running，并在确认文案中展示 job 状态。
9. **TUI 高度预算继续分散**：新增取消确认行后，如果继续只从高度里扣 pipeline bar，小终端会更容易溢出。修正为把 header/message/cancel prompt/pipeline bar/footer 的垂直占用集中计算。
10. **队列 slot 交接竞态**：`finishJob` 当前会从 `queue` 弹出下一个 job，再在锁外调用 `startJob`。新增 `CancelTask` 后，queued job 可能在“已弹出、未启动”的窗口被取消；如果 `startJob` 只释放 slot 而不继续交给下一个 queued job，会把后续队列饿住。修正为增加队列调度 helper，跳过 stale/cancelled queued job，并在 slot 已持有时继续向后交接。
11. **executor timeout 语义误导**：`runCommand` 当前只要 `ctx.Err() != nil` 就把 `Result.Timeout=true`，用户取消会被 stage B/C/D/E/F 产物写成“timed out”。修正为：`Timeout` 只表示 `context.DeadlineExceeded`，父 ctx 的 `context.Canceled` 仍保留在 `Err` 中。
12. **Stage A 取消后仍可能继续跑脚本**：仅把 `ctx` 传给单个 Python 命令还不够，`runStageAScripts` 必须在每个脚本之间检查 `ctx.Err()`，Python/uv fallback 探测也要在取消后立即停止。
13. **TUI 消息可能互相覆盖**：当前已有 `pendingJob` 提交反馈。新增 `pendingCancelJobID` 后，如果不规定更新顺序，`updatePendingJobMessage` 会把“已终止”覆盖成普通 “job failed”。修正为取消成功后清理同 job 的 `pendingJob`，并让 cancel 消息优先。
14. **aborted run 的 stage 终态策略不明确**：现有模型没有 `StageAborted`。修正为不新增 schema；如果取消时某 stage 仍是 `running`，在 abort helper 中把它收口成 `StageFailed` 并写明 `pipeline aborted: ...`，未开始的 pending stage 保持 pending，用 run 的 `aborted` 终态表达整体中止。
15. **测试清单不足**：原文没有覆盖 queued cancel 的 semaphore 回归、slot handoff 竞态、Stage A 取消、aborted run 不被标成 crashed、取消后 runtime cleanup、executor 取消不等于 timeout、TUI “请求中/已终止”两阶段反馈。本文已补齐。

## 1. 目标与非目标

目标：

1. 在 TUI 中按 `Ctrl+X` 终止当前选中任务的活跃流水线 job，活跃包括 `JobQueued` 和 `JobRunning`。
2. Scheduler 提供 `CancelTask(taskID)` API，安全取消排队中或运行中的 job，并保持 `max_concurrent` 不被破坏。
3. Pipeline 收到父 `ctx` 取消后停止继续调度后续 stage，保存当前可用 stage 成果，尝试必要的 runtime cleanup，并把 run 标记为 `aborted`。
4. TUI 实时反馈取消请求、终止中状态和最终结果。
5. 顺手收束相关模块职责：Scheduler 只管 job 生命周期，Pipeline 只管 run/stage/cleanup 持久化，TUI 只管交互和展示，Executor 保持通用进程终止能力并修正取消/超时语义。

非目标：

1. 不实现暂停/恢复、批量取消、按 runID 跨进程取消。
2. 不修改 Codex CLI 的重试、fallback 或静态审查策略。
3. 不新增 DB 表保存 scheduler job 历史；job 仍是 TUI 进程内状态。
4. 不对用户取消和父 context deadline 做不同的 pipeline 产物策略；两者都保存已有成果并标记 run 为 `aborted`。单个 stage 自己的 timeout 仍保持现有 stage-level 行为。
5. 不重写 `internal/executor`；现有 Unix 进程组 `SIGKILL` 和 Windows `Process.Kill` 能满足本轮需求。本轮只允许做 `Result.Timeout` 语义小修，避免用户取消被误报为 stage timeout。

## 2. 当前代码契约

### 2.1 Scheduler

`internal/scheduler/scheduler.go` 当前契约：

1. `Job` 已有未导出的 `cancel context.CancelFunc` 和 `mu sync.RWMutex`。
2. `activeByTask map[string]*Job` 同时包含 queued/running job，保证一个 task 只有一个活跃 job。
3. `Submit` 只有在马上启动 job 时才向 `sem` 写入 token；排队 job 不占用 `sem`。
4. `finishJob` 负责从 `activeByTask` 删除 job，并在有队列时把同一个 slot 传给下一个 job；没有队列时才 `releaseSlotLocked()`。
5. `releaseSlotLocked()` 已存在，不应作为本轮新增方法重复设计。
6. 当前 `finishJob` 在锁外调用 `startJob(next)`，新增 queued cancel 后必须显式处理“next 已弹出但尚未启动时被取消”的交接竞态。
7. `Shutdown` 会失败化 queued job，并取消 running job；它是进程退出语义，不等同用户主动取消。

### 2.2 Pipeline

`internal/pipeline/pipeline.go` 当前契约：

1. `Runner.Run(ctx, taskID, opts)` 创建 run、写初始 stage 状态、执行 preflight、按 A-F 顺序执行 stage。
2. stage 结束后用同一个 `ctx` 调用 `PutStage`、`InsertFindings` 和最后的 `FinishRun`。
3. defer 中只要 `err != nil && runCreated && !runFinished`，就会调用 `markRunCrashed(context.Background(), ...)`。
4. B/C/D/E/F 的主要子进程会使用传入的 `ctx`，但 A 阶段脚本当前没有使用父 `ctx`。
5. 正常 runtime cleanup 只在 B/C 后的固定位置执行；取消路径如果提前 return，必须显式补 cleanup。

### 2.3 TUI

TUI 当前契约：

1. `app.activeJobs []scheduler.JobSnapshot` 由 `scheduler.Snapshot()` 填充。
2. `renderPipelineBar` 已经展示 queued/running job。
3. `handleKey` 中 `runConfig.active` 是 modal 状态，普通全局键不会穿透。
4. `Ctrl+R` 现在打开运行配置，而不是旧式 yes/no confirm；取消功能不能复用旧 `Confirm()` 测试语义。

### 2.4 Executor

`internal/executor` 当前契约：

1. `exec.CommandContext` 已与自定义 `cmd.Cancel` 绑定。
2. Unix 下 `prepareCommand` 设置进程组，`terminateCommand` 对进程组发送 `SIGKILL`。
3. Windows 下退化为 `Process.Kill`。
4. `runCommand` 在 `ctx.Err() != nil` 时把 `Result.Err` 设为该 context error。
5. 当前 `Result.Timeout` 也在任意 `ctx.Err() != nil` 时置 true，需要修正为只在 `context.DeadlineExceeded` 时置 true。

## 3. 修订后的架构边界

### 3.1 责任边界

1. **Scheduler**：只负责 job 排队、启动、取消请求、snapshot 和 notify。它不直接改 DB run 状态，也不理解 stage artifact。
2. **Pipeline**：只负责 run/stage/findings/artifacts 的终态一致性。收到取消后，它决定如何保存 aborted run 和 cleanup 产物。
3. **TUI**：只负责用户选择、确认、发出取消命令和展示 scheduler snapshot。TUI 不直接修改 job 字段。
4. **Executor**：继续做通用子进程执行和终止，不引入 pipeline 专用 API；只修正通用 `Result.Timeout` 语义。

### 3.2 Scheduler 小重构

新增 sentinel error，避免 UI 和测试依赖脆弱字符串：

```go
var ErrJobCancelledByUser = errors.New("cancelled by user")
```

`Job` 和 `JobSnapshot` 增加取消请求标记：

```go
type Job struct {
    // existing exported fields...
    opts            pipeline.RunOptions
    cancel          context.CancelFunc
    cancelRequested bool
    mu              sync.RWMutex
}

type JobSnapshot struct {
    // existing fields...
    CancelRequested bool
}
```

建议增加私有 helper，减少 `runJob`、`CancelTask`、`Snapshot` 和队列调度中的重复状态写入：

1. `removeFromQueueLocked(jobID string)`：调用者持有 `s.mu`，只从 `s.queue` 移除 job。
2. `deleteActiveJobLocked(job *Job)`：调用者持有 `s.mu`，仅当 `activeByTask[job.TaskID] == job` 时删除，避免未来出现同 task 新 job 时误删。
3. `applyResultLocked(job *Job, result pipeline.Result)`：调用者持有 `job.mu`，集中复制 `Result`、`RunID`、`Stages`、`CurrentStage`。
4. `finishCancelledJobLocked(job *Job, now time.Time)`：调用者持有 `job.mu`，设置 `JobFailed`、`ErrJobCancelledByUser`、`FinishedAt`、`CurrentStage=""`。
5. `popQueuedJobLocked() *Job`：调用者持有 `s.mu`，从队列头开始跳过已取消、已失败、已从 `activeByTask` 脱钩的 stale job，只返回仍可启动的 `JobQueued` job。

不要新增第二个 semaphore release helper；复用现有 `releaseSlotLocked()`，且只在已占用 slot 的 job 完成、slot 无法继续交给 queued job，或 `startJob` 放弃启动且队列已无可启动 job 时调用。

### 3.3 TUI 高度预算小重构

新增一个小 helper 统一计算界面 chrome 高度，避免 `View()`、`applyLayout()`、`renderExecution()` 和 modal 渲染各自手算：

```go
func verticalChromeHeight(m app) int {
    height := 1 // header
    if m.message != "" {
        height++
    }
    if m.confirmCancelTaskID != "" {
        height++
    }
    height += pipelineBarHeight(m)
    height++ // footer
    return height
}
```

`applyLayout` 和 `renderExecution` 应使用 `max(8, m.height-verticalChromeHeight(m))` 作为内容高度预算。这样取消确认提示出现时不会把 overview/table/detail/footer 挤到不可预测的位置。

## 4. Scheduler 变更

### 4.1 `CancelTask(taskID)` 语义

`CancelTask` 按 taskID 取消当前 active job。queued job 是立即终态；running job 是发出取消请求，最终状态由 `runJob` 收口。

关键约束：

1. 入参 trim，空 taskID 返回错误。
2. 找不到 active job 返回明确错误。
3. queued job：标记 failed/cancelled，移出 `activeByTask` 和 `queue`，不释放 `sem`，不主动启动下一个 job。
4. running job：设置 `cancelRequested=true`，复制 `cancel` 到局部变量，释放所有锁后调用 cancel。
5. done/failed job：返回不可取消错误；如果已不在 `activeByTask`，走“无 active job”。
6. `notify()` 在锁外调用，避免扩大临界区。

参考实现形态：

```go
func (s *Scheduler) CancelTask(taskID string) error {
    if s == nil {
        return errors.New("scheduler is nil")
    }
    taskID = strings.TrimSpace(taskID)
    if taskID == "" {
        return errors.New("task id is required")
    }

    var cancel context.CancelFunc
    var notify bool
    now := time.Now().UTC()

    s.mu.Lock()
    job := s.activeByTask[taskID]
    if job == nil {
        s.mu.Unlock()
        return fmt.Errorf("task %s has no active job", taskID)
    }

    job.mu.Lock()
    switch job.State {
    case JobQueued:
        job.cancelRequested = true
        finishCancelledJobLocked(job, now)
        s.deleteActiveJobLocked(job)
        s.removeFromQueueLocked(job.JobID)
        notify = true
    case JobRunning:
        if !job.cancelRequested {
            job.cancelRequested = true
            cancel = job.cancel
        }
        notify = true
    default:
        err := fmt.Errorf("job %s is in state %s, cannot cancel", job.JobID, job.State)
        job.mu.Unlock()
        s.mu.Unlock()
        return err
    }
    job.mu.Unlock()
    s.mu.Unlock()

    if cancel != nil {
        cancel()
    }
    if notify {
        s.notify()
    }
    return nil
}
```

### 4.2 `runJob` 收口

`runJob` 必须在 pipeline 返回后检查 `cancelRequested`，并且即使 `err != nil` 也要保留 pipeline 返回的 partial result：

```go
result, err := pipeline.NewRunner(s.store, s.cfg).Run(ctx, job.TaskID, opts)

job.mu.Lock()
if job.cancelRequested {
    if result.Run.RunID != "" {
        applyResultLocked(job, result)
    }
    finishCancelledJobLocked(job, time.Now().UTC())
} else if err != nil {
    job.State = JobFailed
    job.Err = err
    job.FinishedAt = time.Now().UTC()
} else {
    applyResultLocked(job, result)
    job.State = JobDone
    job.Err = nil
    job.FinishedAt = time.Now().UTC()
}
job.mu.Unlock()
s.notify()
```

`applyProgress` 也要尊重 `cancelRequested`：允许继续更新 `RunID` 和 stage snapshot，但 `Done` 事件不能把已请求取消的 job 改回 `JobDone` 或清空取消错误。

### 4.3 队列 slot 交接

新增 queued cancel 后，`finishJob -> startJob` 之间出现一个必须处理的小窗口：

```text
running job 完成
  -> finishJob 从 queue 弹出 TASK-002，准备复用当前 sem slot
  -> 锁外 startJob(TASK-002) 前，用户取消 TASK-002
  -> startJob 发现 TASK-002 已失败
  -> 这个已持有的 slot 必须继续交给 TASK-003，不能只释放后返回
```

建议把“找下一个可启动 queued job”的逻辑收束为 `popQueuedJobLocked()`，并让 `finishJob` 和 `startJob` 的异常分支都复用它：

1. `finishJob` 删除 active job 时使用 `deleteActiveJobLocked(job)`，然后调用 `popQueuedJobLocked()`。
2. 如果拿到 next，则锁外调用 `startJob(next)`，把当前 slot 交给它。
3. 如果 `startJob(next)` 发现 scheduler 已关闭、job 已取消或状态不再是 `JobQueued`，不要直接释放 slot；应在同一个 `s.mu` 临界区内再次调用 `popQueuedJobLocked()`。
4. 只有没有可启动 queued job 时，才调用 `releaseSlotLocked()`。
5. `popQueuedJobLocked()` 跳过 stale job 时不能改写 sem；这些 job 没有单独占用 slot。

这段逻辑是为了维持一个不变量：`sem` 中的 token 数只等于正在运行或正在交接启动的 slot 数，queued cancel 本身永远不改变 token 数。

### 4.4 `Shutdown` 兼容

`Shutdown()` 不设置 `cancelRequested`，因为这是 TUI 退出语义。pipeline 仍会把 run 标记为 `aborted`，但 scheduler job 的最终错误可以保持 `context canceled`。这样 UI 能区分用户主动 `Ctrl+X` 和进程退出。

## 5. Pipeline 变更

### 5.1 取消检查点

在 `Runner.Run` 中增加明确取消检查点：

1. `CreateRun` 成功后、初始 stage 写入前。
2. 初始 stage 写入后、preflight 前。
3. preflight 和 stale cleanup 后。
4. 每个 stage 开始前。
5. 每个 stage 执行后、普通 `PutStage(ctx, ...)` 前。
6. runtime cleanup 后。
7. 最终 `FinishRun` 前。

不要只在 stage 后检查；否则用户在 stage 间隙取消时仍会启动下一个 stage。

### 5.2 abort 持久化 helper

新增私有 helper，例如 `finishAbortedRun`。它负责一次性完成以下工作：

1. 先调用 `markInFlightStageAborted(stages, abortErr)`：如果有 `StageRunning`，将该 stage 改为 `StageFailed`，补 `FinishedAt`、`DurationMS` 和 `ErrorSummary="pipeline aborted: ..."`；不要新增 `StageAborted` 常量。
2. 用短 `saveCtx := context.WithTimeout(context.Background(), 5*time.Second)` 保存当前 `stages`、当前 stage findings 和 `stage_status.json`。
3. 如果 runtime stage 被选中且 `runtimeCleanupDone == false`，用独立 cleanup context 尝试 `cleanupCurrentRuntime`。
4. cleanup context 建议 30 秒起步，不复用已取消的父 `ctx`；cleanup 失败时继续记录 `cleanupFinding`，但不阻止 aborted run 落库。
5. cleanup 之后创建新的 `finishCtx := context.WithTimeout(context.Background(), 5*time.Second)`，再调用 `FinishRun(finishCtx, runID, taskID, model.RunAborted, time.Since(start))`。不要复用可能已经被 cleanup 消耗掉时间预算的 `saveCtx`。
6. 更新内存中的 `run.Status`、`run.FinishedAt`、`run.DurationMS`。
7. 一旦 abort helper 接管收口，就设置调用方的 `runFinished = true`，避免现有 defer 把同一 run 二次标记为 `crashed`。即使个别 stage/findings 写入失败，也不要用 `markRunCrashed` 覆盖用户取消。
8. 写 `abort_summary.json`，至少包含 `run_id`、`task_id`、`reason`、`save_errors`、`cleanup_status`、`recorded_at`，方便排查持久化或清理失败。
9. 发送 `RunProgress{Event:"run_done", Done:true, Err: abortErr}`，让 scheduler/TUI 立即知道这是异常终止。
10. 返回 `Result{Run: run, Stages: stages}, abortErr`。

推荐调用形态：

```go
if abortErr := ctx.Err(); abortErr != nil {
    return r.finishAbortedRun(abortErr, &run, taskID, start, artifactRoot, stages, keepRuntime, &runtimeCleanupDone, &runFinished, progress)
}
```

实际实现时可以按 Go 代码可读性调整参数，但不要把 abort 持久化逻辑散落在 stage loop 里。

### 5.3 Stage A 贯通 context

当前 A 阶段是取消链路的断点，必须修改：

```go
func (r Runner) stageA(ctx context.Context, run model.RunRecord, project scanner.Project) model.StageRecord
func (r Runner) runStageAScripts(ctx context.Context, project scanner.Project, scriptRoot, logPath string, outputs map[string]string) map[string]scriptExecution
func (r Runner) runStageAScript(ctx context.Context, workDir, inputRoot, script string, extraArgs []string) scriptExecution
func (r Runner) pythonInvocation(ctx context.Context, workDir string) (string, []string, string)
```

所有 Python version check 和脚本执行都使用传入 `ctx`。只有不属于本次 run 的纯本地探测 helper 才能使用 `context.Background()`；A 阶段不属于这种情况。

`runStageAScripts` 还要在每个脚本之间检查 `ctx.Err()`：一旦取消，停止启动后续 helper，给尚未运行的脚本写入 `scriptExecution{OK:false, Error: ctx.Err().Error()}` 这样的轻量结果即可。`pythonInvocation` 在取消后也不要继续从 `python` fallback 到 `python3` 或 `uv`，否则用户已经终止后仍会出现额外进程探测。

### 5.4 Codex context 小修

`codexContext` 当前在 recheck 分支中用 `context.Background()` 读取 ref run。建议改为：

```go
func (r Runner) codexContext(ctx context.Context, project scanner.Project, opts RunOptions, stage string) (string, error)
```

并把 `stageCodex`、`stageF` 中的调用改为传入父 `ctx`。这不是取消功能的主路径，但能避免用户取消后还继续做 DB 读取和上下文拼接。

### 5.5 Executor 取消/超时语义小修

`internal/executor/cmd.go` 不需要重写，但需要把 `Timeout` 从“context 有错误”修正为“确实 deadline exceeded”：

```go
if ctxErr := ctx.Err(); ctxErr != nil {
    result.Err = ctxErr
    result.Timeout = errors.Is(ctxErr, context.DeadlineExceeded)
}
```

这样用户按 `Ctrl+X` 时，stage 产物不会把主动终止写成 timeout；单个 stage 自己的 deadline 仍会保留 `Timeout=true`。

## 6. TUI 变更

### 6.1 app 状态

新增字段：

```go
type app struct {
    // existing fields...
    confirmCancelTaskID string
    confirmCancelJobID  string
    pendingCancelJobID  string
}
```

`confirmCancelJobID` 用于确认文案和后续消息匹配；真正取消仍调用 `CancelTask(taskID)`，因为 scheduler 的公开契约是 task 级取消。

如果取消的是刚提交的同一个 job，确认成功后要同步清掉 `pendingJob`，否则现有提交反馈会在下一次 `schedulerJobsMsg` 中把终止反馈覆盖成普通失败文案。

### 6.2 活跃 job 查询

新增 helper：

```go
func (m app) activeJobForTask(taskID string) (scheduler.JobSnapshot, bool) {
    for _, job := range m.activeJobs {
        if job.TaskID != taskID {
            continue
        }
        if job.State == scheduler.JobQueued || job.State == scheduler.JobRunning {
            return job, true
        }
    }
    return scheduler.JobSnapshot{}, false
}
```

不要只判断 `JobRunning`，否则 queued job 仍会阻塞同 task 重新提交，却无法从 UI 清掉。

### 6.3 键位优先级

`Ctrl+X` 是普通页面的全局快捷键，但 modal 优先：

1. `runConfig.active` 时按 `Ctrl+X` 不穿透，提示“请先关闭运行配置再终止作业”。
2. `confirmCancelTaskID != ""` 时只接受 `y/Y/enter`、`n/N/esc`。
3. 普通页面按 `Ctrl+X`：取 `selectedTaskID()`，查 `activeJobForTask`，没有 active job 就提示“该任务没有排队或运行中的作业”，有则打开确认。

确认成功后返回 `cancelTaskCmd`，但文案使用“已发送终止请求”，不要立即说“已终止”。

### 6.4 消息类型和命令

```go
type taskCancelRequestMsg struct {
    taskID string
    jobID  string
    err    error
}

func cancelTaskCmd(s *scheduler.Scheduler, taskID, jobID string) tea.Cmd {
    return func() tea.Msg {
        if s == nil {
            return taskCancelRequestMsg{taskID: taskID, jobID: jobID, err: fmt.Errorf("scheduler unavailable")}
        }
        err := s.CancelTask(taskID)
        return taskCancelRequestMsg{taskID: taskID, jobID: jobID, err: err}
    }
}
```

`Update` 处理成功后：

1. `pendingCancelJobID = msg.jobID`
2. `message = "已发送终止请求 " + msg.taskID`
3. 如果 `pendingJob == msg.jobID`，立刻置空 `pendingJob`
4. reload scheduler jobs 和 overview/detail

`schedulerJobsMsg` 中增加 `updatePendingCancelMessage`：当 `pendingCancelJobID` 对应 job 变成 `JobFailed` 且 `CancelRequested` 为 true 时，显示“已终止 TASK-XXX 的运行”；如果失败原因不是取消，则显示“终止后 job 失败: ...”。

消息更新顺序必须是：

1. 先处理 `updatePendingCancelMessage`。
2. 再处理 `updatePendingJobMessage`。
3. `updatePendingJobMessage` 必须跳过 `job.JobID == pendingCancelJobID` 的 job。

这样用户看到的是终止语义，而不是底层 scheduler 的普通 failed 语义。

### 6.5 渲染与 footer

`View()` 在 message 后、pipeline bar 前渲染取消确认提示：

```go
if m.confirmCancelTaskID != "" {
    prompt := fmt.Sprintf("确认终止 %s 的 %s？(y/n)", m.confirmCancelTaskID, m.confirmCancelJobID)
    builder.WriteString(errorStyle.Render(truncateDisplay(prompt, max(8, m.width-2))))
    builder.WriteString("\n")
}
```

`footerFor` 增加：

```go
case m.confirmCancelTaskID != "":
    return "y/Enter 确认终止  n/Esc 取消"
```

普通 footer 增加 `Ctrl+X 终止`。布局高度预算统一走 `verticalChromeHeight`，避免小终端下取消确认、pipeline bar、正文和 footer 互相挤占。测试钩子不要复用 `Confirm()`；新增 `CancelConfirm()` 或等价方法，继续让 `Confirm()` 只表示运行配置 modal。

## 7. 完整交互流程

运行中 job：

```text
用户选中 TASK-001
  -> Ctrl+X
  -> TUI 通过 activeJobForTask 找到 job-xxx running
  -> 显示确认终止 TASK-001 的 job-xxx
  -> y/Enter
  -> scheduler.CancelTask("TASK-001")
  -> running job 设置 cancelRequested=true，锁外调用 cancel()
  -> TUI 显示“已发送终止请求 TASK-001”
  -> executor 收到 ctx 取消并终止子进程
  -> pipeline 保存当前 stage、尝试 cleanup、FinishRun(aborted)
  -> runJob 看到 cancelRequested，JobFailed + ErrJobCancelledByUser，并保留 partial result
  -> scheduler.notify()
  -> TUI 刷新后显示“已终止 TASK-001 的运行”
```

排队中 job：

```text
用户选中 TASK-002
  -> Ctrl+X
  -> TUI 找到 job-yyy queued
  -> y/Enter
  -> scheduler.CancelTask("TASK-002")
  -> queued job 直接 JobFailed + ErrJobCancelledByUser
  -> 从 queue 和 activeByTask 删除
  -> 不释放 sem，不主动启动新 job
  -> TUI 刷新后显示“已终止 TASK-002 的运行”
```

## 8. 边缘情况

| 场景 | 行为 |
|---|---|
| 未选中任务 | message: `没有选中的任务` |
| 选中任务没有 queued/running job | message: `该任务没有排队或运行中的作业` |
| runConfig 打开时按 Ctrl+X | 不穿透，提示先关闭运行配置 |
| 取消确认中按其他键 | 忽略 |
| 确认后 job 已自然完成 | `CancelTask` 返回无 active job 或不可取消错误，TUI 显示终止失败并刷新 |
| 同一 running job 重复 CancelTask | 幂等：已有 `cancelRequested` 时返回 nil，不重复修改状态 |
| queued job 取消后提交新 job | 允许；旧 queued job 已从 `activeByTask` 删除 |
| queued job 取消后正在运行的 job 未完成 | 不释放 `sem`，新提交 job 仍应排队 |
| queued job 已从 queue 弹出但尚未 start 时被取消 | 复用当前 slot 继续寻找下一个 queued job；没有下一个才释放 `sem` |
| 用户取消时 pipeline 在 Stage A | Python 子进程通过父 ctx 被终止，A 产物尽量保存 |
| 用户取消时 Stage A 正在 Python 版本探测 | 停止 fallback 探测，不再尝试 `python3`/`uv` |
| 用户取消时 pipeline 在 B/C/D/E/F | executor 终止子进程，当前 stage 返回失败或部分产物，run 标记 aborted |
| 用户取消触发 executor 返回 | `Err=context.Canceled`，`Timeout=false` |
| 用户取消时 Docker runtime 已启动 | abort helper 尝试 bounded cleanup，写 `cleanup_summary.json` |
| 用户取消后又 Ctrl+C 退出 | `cancel()` 幂等，Shutdown 不改变 `cancelRequested`，最终由已有流程收口 |
| save/cleanup background context 超时 | run 仍尽量 `FinishRun(aborted)`；失败细节写入可用的 artifact/log，不能卡住 UI |

## 9. 文件变更清单

| 文件 | 操作 | 内容 |
|---|---|---|
| `internal/scheduler/scheduler.go` | 修改 | `ErrJobCancelledByUser`；`Job.cancelRequested`；`JobSnapshot.CancelRequested`；`CancelTask`；`removeFromQueueLocked`；`deleteActiveJobLocked`；`popQueuedJobLocked`；result/cancel 私有 helper；slot handoff；`runJob` 和 `applyProgress` 取消收口 |
| `internal/pipeline/pipeline.go` | 修改 | 取消检查点；`finishAbortedRun` helper；in-flight stage 取消收口；`abort_summary.json`；aborted run 持久化；取消时 bounded runtime cleanup |
| `internal/pipeline/stage_a.go` | 修改 | `stageA`、`runStageAScripts`、`runStageAScript`、`pythonInvocation` 贯通父 `ctx`，取消后停止后续脚本和 fallback 探测 |
| `internal/pipeline/stage_codex.go` | 小改 | `codexContext(ctx, ...)`，避免取消后继续用 background DB 读取 |
| `internal/pipeline/stage_f.go` | 小改 | 调整 `codexContext` 调用签名 |
| `internal/executor/cmd.go` | 小改 | `Result.Timeout` 只在 `context.DeadlineExceeded` 时置 true，用户取消保持 `Timeout=false` |
| `internal/tui/app.go` | 修改 | 取消确认状态、取消消息、`cancelTaskCmd`、pending cancel 最终消息、取消确认行渲染 |
| `internal/tui/keymap.go` | 修改 | `Ctrl+X`、取消确认 modal、footer |
| `internal/tui/render.go` | 小改 | `renderExecution` 复用统一 chrome 高度预算 |
| `internal/tui/layout.go` | 小改 | 复用统一 chrome 高度预算，避免新增提示行造成内容溢出 |
| `internal/tui/pipelineview.go` | 小改 | `CancelRequested` 时展示“终止中” |
| `internal/tui/testhooks.go` | 修改 | 支持 `ctrl+x`、暴露取消确认测试钩子，`Confirm()` 仍只表示运行配置 |
| `internal/pipeline/model/model.go` | 不变 | `RunAborted` 已存在 |

## 10. 实施顺序

### Phase 1: Scheduler API 与状态机

1. 增加 `ErrJobCancelledByUser`、`cancelRequested`、`CancelRequested` snapshot 字段。
2. 增加 `removeFromQueueLocked`、`deleteActiveJobLocked`、`popQueuedJobLocked`、`applyResultLocked`、`finishCancelledJobLocked`。
3. 实现 `CancelTask`，特别确认 queued cancel 不碰 `sem`。
4. 更新 `finishJob` 和 `startJob` 的 slot handoff：跳过 stale queued job，不能饿住后续队列。
5. 更新 `runJob`：取消请求优先，保留 partial result。
6. 更新 `applyProgress`：取消中 job 不被 `run_done` 改成 done。

### Phase 2: Executor 与 Pipeline abort 收口

1. 修正 executor `Result.Timeout`：只有 `context.DeadlineExceeded` 才是 timeout。
2. 把 Stage A 的父 `ctx` 贯通到底，并在脚本循环和 Python fallback 中短路取消。
3. 新增 `finishAbortedRun` helper 和 `markInFlightStageAborted`。
4. 在 run 创建后、初始 stage 写入后、preflight 后、stage 前后、cleanup 后、最终 finish 前增加取消检查。
5. abort helper 中补 runtime cleanup、`abort_summary.json`，并确保 `runFinished = true`。
6. `codexContext` 接受 `ctx`。

### Phase 3: TUI 交互

1. 增加取消确认状态和 `activeJobForTask`。
2. 增加 `taskCancelRequestMsg`、`cancelTaskCmd` 和 pending cancel 消息更新。
3. `Ctrl+X` 键位接入，modal 优先级按本文约束实现。
4. 渲染确认提示、footer、pipeline bar “终止中”。
5. 更新 testhooks，新增取消确认 probe，不复用运行配置 `Confirm()`。

### Phase 4: 测试与回归

1. Scheduler queued cancel：取消 queued job 后不释放 `sem`，第三个 job 仍排队直到第一个 job 完成。
2. Scheduler slot handoff：取消一个已经从 queue 弹出但尚未启动的 job 后，后续 queued job 仍会启动或 sem 被正确释放。
3. Scheduler running cancel：`CancelTask` 后 job 最终 `JobFailed`，`CancelRequested=true`，`Err == scheduler.ErrJobCancelledByUser.Error()`，partial `RunID/Stages` 保留。
4. Executor parent cancel：父 `ctx` 取消时 `Err=context.Canceled` 且 `Timeout=false`；stage timeout 仍为 `Timeout=true`。
5. Pipeline Stage A cancel：fake Python sleep 中取消父 ctx，run 最终是 `aborted`，不是 `crashed`，并且后续 Stage A helper 不再继续启动。
6. Pipeline runtime cleanup cancel：B/C 已生成 `port_map.json` 时取消，写出 `cleanup_summary.json` 和 `abort_summary.json`。
7. Pipeline in-flight stage 收口：取消时仍为 `running` 的 stage 最终落库为 `failed` 且 `ErrorSummary` 含 aborted/canceled 原因，未来 pending stage 不被伪造成 done。
8. TUI keymap：无 active job、queued job、running job、确认取消、确认提交、runConfig active 时 `Ctrl+X`。
9. TUI message：取消刚提交的 `pendingJob` 时，最终消息是“已终止”，不会被普通 job failed 文案覆盖。
10. TUI render：小宽度/小高度下取消确认、pipeline bar、footer 不重叠。

建议验证命令：

```bash
go test ./tests/internal/scheduler ./tests/internal/pipeline ./tests/internal/tui
go test ./internal/pipeline ./internal/tui
go test ./...
```

## 11. 实现注意事项

1. 所有新增 helper 优先放在现有 package 内，不为本轮取消功能新增跨包抽象。
2. `notify()` 保持非阻塞，但尽量在锁外调用。
3. 不要在 queued cancel 中调用 `finishJob`，因为 queued job 没有 goroutine，也没有占用 slot。
4. 不要让 `CancelTask` 等待 pipeline 完成；等待会卡住 Bubble Tea update loop。
5. abort 持久化失败不能覆盖用户取消错误；scheduler 对用户取消的最终错误应稳定为 `ErrJobCancelledByUser`。
6. `RunAborted` 是 run 的业务终态，`JobFailed` 是 scheduler 的执行结果，两者不冲突。
7. `finishJob` 删除 `activeByTask` 时用指针匹配，避免未来同 task 新 job 被旧 job 的收尾逻辑误删。
8. `Result.Timeout` 不要再作为“进程被终止”的泛化标记；用户取消、Shutdown 取消和 stage deadline 是三种不同信号。
9. TUI 的取消确认和运行配置是两个不同 modal 状态，测试钩子也要分开，避免旧 `Confirm()` 语义再次漂移。
10. 如果后续要支持跨进程取消，再考虑把 job/run cancel 请求落 DB；本轮不要提前引入未使用的持久化表。
