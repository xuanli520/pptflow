# Harbor Factory `running` 卡死问题复盘与精简修复方案

> 状态：Implemented
>
> 日期：2026-07-18
>
> 完成日期：2026-07-18
>
> 事故 Run：`019f7069-0858-74a5-a421-1f650532248a`

## 1. 结论

本次任务已经停止执行，但持久化状态没有完成收敛：

```text
WorkflowRun       = running
StageAttempt      = in_doubt       (generate_task_files)
NodeAttempt       = running
DurableJob        = running
DispatchLease     = active but expired
DispatchClaim     = active
RunWorker         = exited
QuotaLeases       = uncertain x 6
```

最主要原因不是模型超时，也不是单纯的 TUI 展示错误，而是 dispatch lease 心跳失败后出现了两个问题：

1. 已失去可信执行权的 worker 仍通过兜底路径写入了部分 Stage 和 quota 状态；
2. worker 随后退出，下一次过期扫描和语义恢复都没有发生。

系统已有 lease 过期和 reconciliation API，因此并非完全没有“垃圾清理”。真正缺少的是一个完整、可重复执行的状态收敛闭环。这里的记录是审计事实，正确处理方式是标记过期并对账，不是删除数据库行。

本次修复应遵守一个核心规则：

> lease 有效时 worker 可以写执行结果；lease 丢失后 worker 立即停止，只有 reconciler 可以收敛 Job、Node、Stage、Run 和 quota。

## 2. 事故证据

### 2.1 持久化状态

| 实体 | 状态 | 关键事实 |
| --- | --- | --- |
| WorkflowRun | `running` | version 4，无 `finished_at` |
| StageAttempt | `in_doubt` | `generate_task_files`，`error_text=context canceled` |
| NodeAttempt | `running` | 无终态、无 `finished_at` |
| DurableJob | `running` | 无 durable failure |
| Dispatch claim | `active` | 对应 lease 已过期 |
| Dispatch lease | `active` | `expires_at=2026-07-17T14:28:09Z` |
| Quota leases | `uncertain` x 6 | task/actor 两组 agent turn、submission、stage attempt |
| Artifact manifest | 无 | 本阶段没有提交产物 |
| Side-effect operation | 无 | 本阶段没有外部副作用记录 |

### 2.2 时间线

以下时间为 UTC：

| 时间 | 事件 |
| --- | --- |
| 14:23:59.559 | 审核通过，Run 回到 `running` |
| 14:23:59.661 | 创建 `generate_task_files` StageAttempt |
| 14:23:59.703 | Job 被 claim，创建 90 秒 dispatch lease |
| 14:24:00.577 | 写入唯一 checkpoint：`turn_ready`，`resumable=false` |
| 14:26:39.712 | 最后一次成功的 dispatch heartbeat |
| 14:26:59.509 | supervisor heartbeat 仍成功，说明 worker 进程当时仍存活 |
| 14:26:59.723 | 6 条 quota lease 被标记为 `uncertain` |
| 14:26:59.762 | StageAttempt 被标记为 `in_doubt` |
| 14:26:59.768 | supervisor lease 被释放，worker 退出 |

Worker 日志只保留了结果，没有保留最初的 heartbeat 错误分类：

```text
durable job lease lost while handling 019f7076-76b3-71b4-91fb-4ccd284f1366: job.execution_failed
```

### 2.3 不是正常超时

冻结预算为：

```text
turn_timeout    = 20m
attempt_timeout = 70m
max_elapsed     = 70m
```

本阶段约 3 分钟后因 `context canceled` 退出，时间点与 dispatch heartbeat 中断吻合，不符合 stage budget timeout。

### 2.4 本阶段可以安全重建

`generate_task_files` 是 `content_producer`，不是 `external_side_effect`。现场同时满足：

- 没有 artifact manifest 或 artifact ref；
- 没有 side-effect operation；
- checkpoint 不可恢复；
- output submission quota 未消费。

因此旧 attempt 不能继续执行，但可以在 reconciliation 后标记为 `interrupted`，将 Run 置为 `failed_recoverable`，再由 `run recover` 断点恢复创建新的 retry attempt。旧 Job 继续保留为 `in_doubt` 审计事实。

## 3. 五个主要原因

### 1. P0：lease 丢失后仍存在 worker 写入路径

`dispatchLeaseHeartbeats` 在 heartbeat 失败后关闭 `LeaseLost` 并取消 handler context。随后 Node 终态写入因 canceled context 失败，代码进入 `failAdmittedStageIntegrity`；该函数使用 `context.Background()`：

- 将 quota 标记为 `uncertain`；
- 将 Stage 标记为 `in_doubt`；
- 但没有完成 Node、Run 和 Job 的一致投影。

同时 `DurableWorker` 检测到 `heartbeats.wasLost()` 后拒绝提交 Job 终态。最终一个旧 worker 写了一半，另一个恢复者又尚未接管。

更深层的问题是执行终态写入只依赖内存中的 `LeaseLost` 判断，Store mutation 没有在同一事务内校验 dispatch lease ID、owner、fencing token 和有效期，存在检查与写入之间的竞态。

### 2. P0：发现 lease loss 后 supervisor 立即退出

`RunWorkerSession` 将没有 `FinalState` 的 cycle error 视为进程级失败并退出。过期扫描原本位于下一次 `DurableWorker.RunOnce` 开头，但本次没有下一次 cycle。

这解释了为什么 expired lease、claim 和 Job 可以长期保持原状态：清理代码存在，触发者退出了。

### 3. P0：结构回收和语义恢复没有形成闭环

`ScanExpiredDurableJobsForReconcile` 能原子完成：

- dispatch lease -> `expired`；
- Job -> `in_doubt`；
- claim -> `expired`；
- 释放 job/capacity lease；
- control -> `reconcile_required`。

但应用层恢复仍有两个缺口：

- `ReconcileDurableJobRecoveries` 只补偿已经终态的 Stage；本次 Stage 为 `in_doubt`，会直接跳过；
- `LocalRuntimeService.ReconcileRun` 只回收本地结构事实，不会把普通 content stage 对账为 `interrupted/failed_recoverable`。

因此当前系统能识别“不可信”，却不能把无外部副作用的“不可信”收敛为“可恢复失败”。

### 4. P1：heartbeat 失败策略过于粗糙

当前 heartbeat 对任意一次 Store error 都立即关闭 `LeaseLost`，没有区分明确失权错误与可在 lease 安全窗口内重试的瞬时错误，也没有保存首次错误分类。

因此一次 SQLite busy 或短暂 I/O/optimistic-lock 争用也可能直接取消长任务。当前日志只剩最终的 `lease lost`，无法确认本次最初错误属于哪一类，所以这一项是确定存在的放大器，但不能断言为本次最初故障的具体类型。

### 5. P2：活性判断和 TUI 同时隐藏了真实故障

`eligibleQueuedRunWork` 对普通 `running` Run 无条件返回 ready，不要求存在 queued job、有效 attachable job 或有效 dispatch lease。重新 detach 后可能只进行空轮询，`Run=running` 与“没有任何可执行工作”可以长期共存。

`taskBoardCurrentStage` 只展示 `running/waiting/queued/reconciling`，遗漏 `in_doubt`，所以界面显示 Run 仍在 running，却没有阶段名。

## 4. 必须保持的不变量

### 4.1 执行权

```text
有效 dispatch fence -> worker 可以写执行结果
无效 dispatch fence -> worker 不得写 Node/Stage/Run/Job/quota 终态
```

内存信号用于快速取消，Store 中的 transaction-local fence check 才是最终写入权限判断。

### 4.2 恢复权

lease-loss 之后只有一个应用层 recovery applicator 可以推进状态。worker 自动恢复和 `run reconcile` 必须调用同一逻辑。

### 4.3 状态真相

当不存在有效 worker/dispatch lease，也不存在 queued 或 attachable job 时，Run 不得无限保持 `running`。

### 4.4 副作用安全

- 无 artifact、无 external side effect：允许对账为 `interrupted`；
- 已存在 started/unknown external side effect：保持 `in_doubt`，只交给领域 reconciler；
- 已终态 Stage：保留其结果，只补齐缺失的 Run/coordinator 投影。

最后一条非常重要：不能把所有 expired Job 的父 Run 无条件改成 `in_doubt`，否则会破坏“Stage 已完成但 Job 回执丢失”这一确定性恢复路径。

## 5. 最小正确架构

不新增数据库表、生命周期状态、daemon、scheduler、消息队列或通用 GC 框架。

只保留一个恢复流程：

```text
worker 发现 heartbeat 失败
        |
        | 立即停止执行写入，返回 typed lease-loss error
        v
RunWorkerSession 保持 supervisor lease，等待 dispatch lease 到期
        |
        v
ScanExpiredDurableJobsForReconcile
        |
        | 只收敛 job/lease/claim/control 结构事实
        v
同一个 application recovery applicator
        |
        +-- Stage 已终态 ----------------> 补齐现有 coordinator/Run 投影
        +-- 无外部副作用、无提交结果 ----> Node/Stage interrupted
        |                                  Quota reconcile canceled
        |                                  Run failed_recoverable
        +-- 外部结果未知 ----------------> 保持 in_doubt
```

若进程被直接杀死，没有任何本地进程可以在墙钟时间内自动工作。此时由下一次 worker activation 或显式 `run reconcile` 执行同一恢复流程。为本地 CLI 系统新增常驻 daemon 只为扫 lease，收益不足以抵消架构复杂度。

### 为什么不把 sweep 放进 Store backup loop

该方案看似简单，实际不正确：

1. Store 只能判断 lease/Job 结构事实，不能判断 Stage effect、artifact 和领域副作用；
2. scanner 当前返回一次性的 recovery facts，Store 后台先消费后，应用层 handler 可能看不到本批恢复；
3. backup 生命周期与执行恢复无业务关系，会形成隐式耦合；
4. 多个 writable CLI/TUI 进程会各自启动 ticker，增加无意义争用。

因此自动触发应留在已有 RunWorkerSession；Store 保持确定性的原子状态操作，不承担调度职责。

## 6. 修复方案

### P0-A：在 Store mutation 中校验 dispatch fence

涉及：

- `internal/app/durable_worker.go`
- `internal/app/frozen_execution_runtime.go`
- `internal/harbor/store/v2_jobs.go`
- Node/Stage/Run terminal mutation 所在 Store 文件

最小设计：

1. `DurableJobExecution` 携带不可变 `DispatchFence{LeaseID, Owner, FencingToken}`；
2. 一个小型 `DispatchFenceGuard` 串行化 heartbeat 状态更新和 terminal mutation 准入，消除内存检查与写入之间的竞态；
3. Store 提供 transaction-local `assertActiveDispatchFenceTx`；
4. worker 发起的 Node、Stage、Run、Job 和相关 quota 终态写入必须在同一事务内验证 fence 仍 active、token 匹配且未过期；
5. in-memory `LeaseLost` 关闭后，runtime 在进入任何终态路径前立即返回 typed error；
6. `failAdmittedStageIntegrity` 只有在 dispatch fence 仍有效时才可使用独立 cleanup context；lease-loss 不再进入该函数。

不要求把所有业务写入合并成一个巨型事务；只要求每个 worker-owned terminal mutation 都不能绕过同一 fence 事实。

### P0-B：lease-loss 后继续到恢复 cycle

涉及：

- `internal/app/durable_worker.go`
- `internal/app/run_worker_session.go`

改动：

1. 增加稳定的 `ErrDurableJobLeaseLost`；
2. 明确失权类错误立即关闭 fence；瞬时 Store error 只允许在最后成功 lease 的安全窗口内做 bounded retry；
3. 保存首次和最终 heartbeat error class，不保存任意敏感原文；
4. `RunWorkerSession` 遇到 typed lease-loss error 时不释放 supervisor lease 并退出；
5. 若 Store 已证明 dispatch fence 无效则立即扫描，否则等待最后一次成功 heartbeat 返回的 `expires_at`；
6. 下一次 cycle 由 scanner 获得 recovery authority；
7. 若 supervisor 自身 lease 丢失，则立即退出，不再尝试恢复。

这样不增加 ticker：复用现有 session、supervisor heartbeat 和 `RunOnce` scanner。

### P0-C：建立一个可重复执行的语义恢复入口

涉及：

- `internal/app/frozen_execution_runtime.go`
- `internal/app/local_runtime_service.go`
- `pkg/workflowkit/recovery.go`（原则上只复用，不扩状态）

要求：

1. worker 自动恢复与 `run reconcile` 调用同一个 applicator；
2. applicator 不只依赖 scanner 当次返回值，还能从 `Job=in_doubt + failure_code=job.lease_lost + StageAttemptID` 重建待恢复事实；
3. 重复调用必须幂等；
4. applicator 只读取持久化事实，不调用 provider 或启动外部工作；
5. 复用 `workflowkit.DecideRecovery` 和现有 `in_doubt -> reconciling` 状态迁移。

处理矩阵：

| 事实 | 收敛结果 |
| --- | --- |
| Stage 已 `completed/infra_failed/interrupted/canceled` | 保留 Stage，调用现有 terminal recovery 补齐 Run/coordinator |
| Stage `running/in_doubt`，无 artifact、无 external effect | Node -> `interrupted`；若 Stage 仍 `running`，先转 `in_doubt`，再 `reconciling -> interrupted`；Run -> `failed_recoverable` |
| quota `uncertain/expired`，确定本次未完成 | `ReconcileQuotaLease(outcome=canceled)`，保留 consumed，释放 remaining |
| external effect 为 started/unknown | Run/Stage 保持 `in_doubt`，等待领域 reconciliation |
| control 仍为 reconcile_required | 不自动越过 control，等待显式决定 |

对本次事故，期望结果为：

```text
DurableJob   = in_doubt          # 保留 lease-loss 事实
NodeAttempt  = interrupted
StageAttempt = interrupted
WorkflowRun  = failed_recoverable
QuotaLeases  = settled/canceled
```

### P1：修正空转判断和展示

`running` Run 只有满足以下任一条件才继续轮询：

- 存在 eligible queued job；
- 存在 attachable running job 且 dispatch lease 有效；
- 当前 session 正在等待一个已知 lease-loss job 到期并恢复。

否则执行一次 bounded run-scoped recovery；仍无工作则返回一致性错误或恢复后的 Run 状态，不能无限空轮询。

Task Board 的当前阶段候选增加 `in_doubt`，并显示稳定的 `job.lease_lost` 与 `reconcile` action。不得展示任意 provider 原始错误或敏感路径。

## 7. 当前事故修复步骤

修复版本部署前不要：

- 直接修改 SQLite；
- 删除 WAL、lease 或 claim 行；
- 重放旧 Job；
- 重新启动旧 worker 进程；
- 只运行当前版本 `run reconcile` 后就认为恢复完成。

修复版本部署后：

1. 使用现有 verified backup 创建恢复前快照；
2. 执行统一后的 `run reconcile --run 019f7069-0858-74a5-a421-1f650532248a`；
3. 验证 Job `in_doubt`、claim/lease expired；
4. 验证 Node/Stage `interrupted`、Run `failed_recoverable`；
5. 验证 6 条 quota lease 已以 canceled outcome 对账；
6. 执行 `run recover --run <run-id> --dry-run`，确认起点为 `generate_task_files`；
7. 使用新 idempotency key 执行 `run recover` 确认恢复；
8. 验证新 StageAttempt 的 `RetryOfStageAttemptID` 指向旧 attempt。

## 8. 实施顺序

### Phase 0：正确性

1. typed lease-loss error 和 heartbeat 错误分类；
2. Store transaction-local dispatch fence check；
3. lease-loss 不再进入 Background terminal write；
4. session 等待 lease 到期并进入下一 recovery cycle。

### Phase 1：恢复闭环

1. 一个幂等 semantic recovery applicator；
2. worker 和 `run reconcile` 共用；
3. content stage 收敛到 `failed_recoverable`；
4. external side effect 保持领域对账边界。

### Phase 2：可观察性

1. 修正 running 空转判断；
2. TUI 展示 `in_doubt` Stage；
3. 日志记录稳定 heartbeat error class。

## 9. 必测场景

1. heartbeat 失败后，旧 worker 不再产生 Node/Stage/Run/Job/quota terminal mutation；
2. 瞬时 heartbeat error 在安全窗口内恢复时，不误判 lease loss；
3. supervisor 仍有效时，系统在 dispatch TTL 到期后自动完成 recovery cycle；
4. 进程硬退出后，新 session 或 `run reconcile` 可以恢复同一 Job；
5. scanner 已先运行的情况下，semantic recovery 仍能从持久化 Job failure 重建事实；
6. Stage 已 completed、Job lease 后丢失时，保留完成结果并补齐后继 coordinator；
7. content stage 无 artifact/effect 时，收敛为 `interrupted/failed_recoverable`；
8. external side effect unknown 时始终保持 `in_doubt`，绝不自动重试；
9. 重复 reconciliation 不产生重复 quota settlement、audit 或 continuation；
10. `running` 且无 eligible work 时不再无限轮询，TUI 显示真实阶段和 recovery action。

## 10. 验收标准

- 检测到 heartbeat failure 后，旧 worker 的写权限立即终止；
- supervisor 存活时，Run 在 `dispatch TTL + bounded recovery time` 内离开假 `running`；
- 无常驻进程时，下一次 worker activation 或显式 reconcile 能确定性恢复；
- 本次事故形态最终收敛为 Job `in_doubt`、Stage `interrupted`、Run `failed_recoverable`；
- 已终态 Stage 的恢复路径不受通用 Run 投影破坏；
- external side effect 未对账前绝不重放；
- 不新增表、状态、daemon、scheduler 或平行 recovery framework。

## 11. 最终决策

本次重构只做四件事：

1. **Store 校验执行权**：终态写入必须携带并验证 dispatch fence；
2. **session 保持恢复触发**：发现 lease loss 后等待到期并进入下一 cycle；
3. **应用层统一对账**：结构扫描后按 Stage/副作用事实收敛，且可幂等重放；
4. **复用现有恢复出口**：本次 content stage 回到 `failed_recoverable -> run recover`。

这能修复当前卡死，同时保持现有 Store、worker、workflowkit 和 authoring continuation 边界，不增加新的系统层。

## 12. 实施结果

本方案已完整落地：

- worker-owned Store mutation 在进入事务前串行化 dispatch admission，并在事务内校验 lease ID、owner、fencing token、active state 和 expiration；
- heartbeat 对瞬时 Store 错误在 lease 安全窗口内重试，明确失权后返回 typed lease-loss error，旧 worker 不再写终态；
- RunWorkerSession 在 supervisor lease 有效时等待 dispatch expiry，并进入下一 scanner/recovery cycle；
- worker 自动恢复与 `run reconcile` 共用持久化事实驱动的 semantic recovery applicator；
- 无 artifact/effect 的 content stage 收敛为 Node/Stage `interrupted`、Run `failed_recoverable`、quota canceled；未知外部结果保持 `in_doubt`；
- running 空转判断与 Task Board `in_doubt`/`job.lease_lost` 展示已修正；
- dispatch guard 与 SQLite 事务统一采用 guard-before-transaction 锁顺序，避免单连接池下 heartbeat/mutation 环形等待。

验收命令：

```bash
go test ./internal/harbor/store ./internal/app -count=1
go test ./... -count=1
go test -race ./internal/app -run 'LeaseLoss|Heartbeat|Recovery' -count=1
git diff --check
git diff --cached --check
```

以上验证均通过。
