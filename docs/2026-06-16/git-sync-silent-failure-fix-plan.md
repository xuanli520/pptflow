# Git 同步静默失败修复方案

日期：2026-06-16

## 结论

针对 `TASK-20260421-649AA3` 这类 “已进入 git fetch/reset/clean，但 TUI 未显示同步失败” 的问题，反馈中的核心判断属实：当前最危险的静默失败路径不是 `git clean` 之后的错误分类，而是 `runGitSync` 在真正开始 git 同步前先调用 `SetGitSyncError(Err:nil)` 清除旧错误。一旦该清错步骤因为任务状态漂移失败，git 同步不会开始，DB 不会写入新的 `sync_error`，TUI 也没有可靠提示。

需要立即修复的是 git sync 错误持久化和 UI 可见性。新增专门状态、独立运行 workspace 等方案有价值，但不应作为本轮 P0 的前置条件。

## 反馈判定

1. **前置清错导致静默失败：属实。**
   `internal/scheduler/scheduler.go:666` 在 `runGitSync` 开头调用 `SetGitSyncError(... Err:nil ...)`。当前 `SetGitSyncError` 只更新 `current_run_id IS NULL` 的 task。若任务残留过时 `current_run_id`，该调用返回错误，`runGitSync` 直接失败返回，后续 `gitSyncer.Sync`、`writeGitSyncErrorLog`、实际 git progress 都不会发生。

2. **`git clean Permission denied` 不是 terminal sync error：属实。**
   `internal/git/progress.go:48` 只把 `DeliveryPackageError` 和 clone/fetch 上的远端仓库不存在视为 terminal。`git clean -fdx` 权限失败是普通 `CommandError`，按现有意图应保留在 `inspecting` 并写 `sync_error`，等待重试。因此 “terminal -> completed -> archived” 不是这次 `git clean` 权限失败的主链路。

3. **`RecordTaskTerminalGitError` 也有状态守卫风险：属实。**
   `internal/db/store.go:402` 要求 `state=inspecting AND current_run_id IS NULL`。如果 terminal git error 发生时 task 已状态漂移，这条路径同样会记录失败失败，并把 DB 写入错误追加到 job error。它不是当前 `git clean` 案例的主因，但应一起修。

4. **git sync log 是重要诊断工具：部分属实，需要补充边界。**
   `writeGitSyncErrorLog` 会把真实 git 命令错误写到 `.qa-control/git-sync/<taskID>.log`，对 `git clean` 的 stderr 很有价值。但它位于 `gitSyncer.Sync` 返回之后；如果前置清错失败，git sync 根本没开始，也不会生成这份日志。因此修复后应同时保留 git 命令错误日志，并为 pre-sync DB/状态错误增加 task event 或独立 scheduler log。

5. **“rm -rf + 重新克隆比隔离更简单”：不完全属实。**
   对只读文件或普通残留目录，`chmod -R u+rwX` 后 `rm -rf` 可行。但本例是 `Permission denied` 删除运行日志，常见根因是 Docker/root-owned 文件。TUI 进程若不是 owner，`rm -rf` 仍会失败。把整个 task/clone 目录安全 rename 到 quarantine 再 fresh clone，通常只要求对父目录有写权限，反而比删除内部 root-owned 文件更可靠。最终实现可以先尝试轻量修复和删除，失败后 fallback 到 quarantine。

## 根因链路

### 链路 A：pre-sync 清错失败

1. 用户提交或重试 Git 同步。
2. `runGitSync` 先调用 `SetGitSyncError(Err:nil)` 清除旧错误。
3. `SetGitSyncError` 使用 `WHERE id = ? AND current_run_id IS NULL`。
4. 如果任务残留旧 `current_run_id`、状态漂移或 task 行不满足该条件，清错返回 `requireAffected` 错误。
5. `runGitSync` 立即返回，实际 git 同步未开始。
6. 没有新的 `sync_error`，没有 git-sync 日志，TUI 只能看到短暂 failed job；刷新后表现为静默失败。

### 链路 B：git clean 权限失败

1. 现有 clone 被识别为可 force-pull。
2. 执行 `git stash push -u`、`git fetch origin --force --prune`、`git reset --hard <remote>`。
3. 执行 `git clean -fdx` 删除未跟踪文件。
4. Docker/Laravel 运行时生成的 `repo/storage/logs/laravel.log` 权限不属于 TUI 用户，`git clean` 返回 `Permission denied`。
5. 该错误应写入 `.qa-control/git-sync/<taskID>.log` 和 `tasks.sync_error`，但如果 DB 写入再被状态守卫挡住，TUI 仍可能无法可靠显示。

## 修复优先级

### P0：消除静默失败

目标：任何 git sync 失败都必须留下 DB 可见状态或明确诊断事件；清除旧错误失败不得阻止本次同步启动。

实施要点：

1. 调整 `runGitSync` 的前置清错：
   - 将 `SetGitSyncError(Err:nil)` 改成 best-effort。
   - 清错失败时不要 `return`，继续设置 `job.CurrentStage=Git` 并执行 `gitSyncer.Sync`。
   - 将清错失败记录为 task event，例如 `git_sync_error_clear_failed`；如果 task event 因同一状态问题无法写入，则至少写入 scheduler 级日志或 job error detail。

2. 拆分 DB API 语义：
   - `ClearGitSyncErrorForSyncStart`：只清理可安全清理的旧错误，失败不阻塞同步。
   - `RecordGitSyncFailure`：无论 `current_run_id` 是否为空，都应写入 `sync_error` 和 event。
   - 如果写入时发现 `current_run_id` 非空，追加诊断事件 `git_sync_state_drift`，提示该 task 在 git sync 前存在运行状态漂移。

3. 修复 terminal git error 持久化：
   - 干净状态下仍可 transition 到 completed 释放容量。
   - 状态漂移时不要丢错误；至少写 `sync_error`，记录 `git_sync_terminal_error_state_drift`。
   - 不要因为 transition 失败而覆盖原始 git 错误。

4. 保留旧错误直到新同步成功：
   - 重试开始时可以记录 `phase=queued/fetch/reset/clean`，但不应先清空 `sync_error`。
   - 只有 git sync 成功后再清空旧错误。

### P1：处理 `git clean` 权限失败

目标：运行期产物污染不能让任务永久卡住，也不能静默失败。

实施要点：

1. 识别权限型 clean 错误：
   - `CommandError.Args` 为 `clean -fdx`。
   - stderr 包含 `Permission denied`、`failed to remove` 等标记。

2. 分层 fallback：
   - 先尝试 `chmod -R u+rwX <clonePath>`，然后重试 `git clean -fdx`。
   - 若仍失败，尝试在已验证属于 scan root 的 batch/task 边界内 rename 到 `.qa-control/git-sync-quarantine/<taskID>-<timestamp>`，再 fresh clone。
   - 如果 rename 也失败，写明需要人工处理的路径和建议命令。

3. 安全边界：
   - 所有删除、rename、reclone 都必须通过现有 path containment 校验。
   - 不直接对 scan root 或 batch 根执行递归删除。
   - quarantine 目录需要后续 GC 或 admin 命令清理。

### P2：TUI 失败可见性

目标：即使 DB 写入失败或用户未选中该任务，也能看到同步失败。

实施要点：

1. Pipeline bar 显示最近 failed job，不只显示 queued/running。
2. 顶部摘要增加 `Git 同步失败: N`，来源优先为 DB `sync_error`，其次为 scheduler recent terminal jobs。
3. scheduler terminal job 到达时触发 TaskBoard/Overview reload，不只在 selected task terminal 时 reload。
4. TaskBoard 卡片显示：
   - git phase。
   - sync error 摘要。
   - `.qa-control/git-sync/<taskID>.log` 路径。
   - clear-error/state-drift 诊断提示。

### P3：长期结构改造

1. 新增明确状态，例如 `git_failed` 或 `sync_failed`，避免把 git 同步失败混入 `inspecting/completed`。
2. Git 同步阶段进度持久化到 DB。
3. 将 canonical git clone 与运行 workspace 解耦，Docker/runtime 只写入 run workspace，避免污染下次 git sync。

## 测试计划

P0 必测：

1. `SetGitSyncError(Err:nil)` 清错失败时，scheduler 仍会调用 git sync runner。
2. 清错失败会留下 task event 或 scheduler 诊断，不会静默。
3. git sync 失败且 task 有残留 `current_run_id` 时，`sync_error` 仍被写入。
4. terminal git error 在状态漂移时不丢失原始错误。

P1 必测：

1. `git clean -fdx` 返回 `Permission denied` 时触发 fallback。
2. `chmod + retry clean` 成功时，不重新克隆。
3. `chmod` 或删除失败时，rename quarantine + fresh clone 成功。
4. fallback 失败时，错误消息包含原始 git stderr、目标路径和人工修复建议。

P2 必测：

1. failed git sync job 在 Pipeline bar 或顶栏可见。
2. terminal scheduler snapshot 到达时会刷新 TaskBoard/Overview。
3. TUI reload 后仍显示 `sync_error` 和 log path。

## 验收标准

1. 本例 `git clean -fdx: Permission denied` 不再静默；TUI 必须显示 `Git 同步失败` 和日志路径。
2. 如果任务存在过时 `current_run_id`，本次 git sync 不应在清错阶段被阻断。
3. 任意 git sync 失败至少有一个持久诊断入口：`tasks.sync_error`、`task_events` 或 `.qa-control/git-sync/<taskID>.log`。
4. 成功重试后，旧 `sync_error` 被清空，任务进入后续 pipeline。
