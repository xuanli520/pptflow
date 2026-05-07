# p2r QA 扫描、时间展示与产物归档修复计划

Date: 2026-05-07
Status: 已审查修订，待实现

## 1. 背景

现场质检画面暴露了三个相关问题：

1. **最后运行时间不符合业务时区**：TUI 展示的最后运行时间比上海时区少 8 小时，且时间内容在表格右侧被截断。
2. **`p2r scan` 扫描边界过宽**：当前扫描会把运行产物中的 `script_input_snapshot` 识别为项目，例如 `TASK-.../qa/runs/run-.../script_input_snapshot`。
3. **质检运行产物目录不利于人工查找**：产物直接堆在 `projects-qa/<task-id>/qa/runs/<run-id>` 下，缺少统一入口，也没有按 batch 分类。

这三个问题需要一起修复。扫描规范决定项目路径，项目路径决定产物归档位置，运行元数据又决定 TUI 的最后运行时间和人工查找路径。

本次复核后的关键修正：

- 扫描后的 canonical `TaskID` 必须来自 `<batch>/<task-id>/<task-id>` 目录名，不能再被 `metadata.json.task_id` 覆盖，否则历史快照里的 metadata 会继续污染 DB。
- 如果接受 `docs/original-session/` 作为原始会话材料目录，Stage A 和内置 Python 校验脚本也必须同步接受同一套 marker，不能只改 scanner。
- 仅 upsert 新扫描项目不足以清理已误扫入 DB 的 `script_input_snapshot` 记录；本轮需要提供显式、安全的 artifact prune 入口用于受污染环境。
- run ID 当前使用 `UnixNano()%1000000`，同一秒内相隔整数毫秒的运行可能碰撞；本轮必须改为上海时间目录名前缀加零填充微秒或更强唯一后缀。

## 2. 目标

### 2.1 输入任务目录规范

`p2r scan` 只识别如下结构中的任务：

```text
<scan-root>/<batch>/<task-id>/<task-id>/
```

示例：

```text
projects-qa/
  batch-2/
    TASK-20260318-3CC794/
      TASK-20260318-3CC794/
        metadata.json
        repo/
        docs/
        original_sessions/
```

用户确认的关键路径示例：

```text
batch-2/TASK-20260318-3CC794/TASK-20260318-3CC794
```

有效项目目录必须满足：

- 第一层是 batch 目录，默认严格匹配 `^batch-[A-Za-z0-9][A-Za-z0-9_.-]*$`。
- 第二层是任务 ID 目录，目录名必须匹配 `^TASK-[A-Za-z0-9][A-Za-z0-9_.-]*$`。
- 第三层必须与第二层任务 ID 完全同名。
- 第三层目录才是 `project.Path`。
- 第三层目录必须包含 `metadata.json`、`repo/`、`docs/` 和原始会话材料目录。
- `project.TaskID` 必须使用第二层/第三层目录名，不再调用旧的 `taskID(path)` 从 `metadata.json` 覆盖；`metadata.json.task_id` 只作为一致性诊断输入。
- 原始会话材料目录先兼容当前现场数据的 `original_sessions/`，并兼容 `docs/original-session/`、`docs/original_sessions/`。scanner、Stage A 和内置校验脚本必须使用同一套 marker。
- batch、task ID、run ID 进入路径前必须经过同一套 `safePathSegment`，拒绝空值、`.`、`..`、路径分隔符和控制字符；空 batch 只允许历史 DB fallback 为 `unbatched`。

必须明确排除：

- `<scan-root>/result/`
- `<scan-root>/.qa-control/`
- `<scan-root>/task-docs/`
- 任意位置的 `qa/`
- 任意位置的 `runs/`
- 任意位置的 `script_input_snapshot/`
- 非 batch 顶层目录
- 不满足 `<batch>/<task-id>/<task-id>` 同名结构的目录

如果未来要支持非 `batch-*` 命名，应新增显式配置项和测试，不在本轮把“建议匹配”留成实现自由度。

### 2.2 质检产物目录规范

新运行产物统一写入：

```text
<scan-root>/result/<batch>/<task-id>/<run-id>/
```

示例：

```text
projects-qa/
  result/
    batch-2/
      TASK-20260318-3CC794/
        run-20260507-133618-301874/
          run_manifest.json
          preflight.json
          logs/
          script_input_snapshot/
          ...
```

这会把旧路径：

```text
projects-qa/TASK-20260327-7817C9/qa/runs/run-...
```

调整为：

```text
projects-qa/result/batch-1/TASK-20260327-7817C9/run-...
```

也就是保留 `task-id`，去掉面向实现细节的 `qa/runs/`，并把 batch 放到人工查找路径的第一层。

### 2.3 时间展示规范

时间存储和展示分离：

- DB 中的 `started_at`、`finished_at`、`last_run_at` 继续存 RFC3339 UTC 时间。
- TUI 展示统一转换为 `Asia/Shanghai`。
- 总览表格展示格式固定为 `YYYY-MM-DD HH:mm`，例如 `2026-05-07 13:36`。
- 详情页可以展示更完整的 `YYYY-MM-DD HH:mm:ss CST`。
- 表格中只要显示最后运行时间列，就必须保证内容完整；终端宽度不足时隐藏该列，而不是显示半截。
- 新 run 目录名使用上海时间生成，便于质检员按目录名判断运行时间，例如 `run-20260507-133618-301874`。后缀必须用零填充微秒或更强唯一值，不能继续使用 `UnixNano()%1000000`。
- `run_manifest.json` 保留现有 `started_at` UTC 字段，同时新增 `started_at_utc`、`started_at_local`、`timezone` 和 `artifact_root`。
- TUI 和 CLI 展示时间必须从 DB/manifest 的 RFC3339 字段解析，不能从 run ID 反推。

## 3. 非目标

本轮不做以下事情：

- 不迁移历史运行产物的物理目录。
- 不改变历史 DB 中已有 run 的 `artifact_root`。
- 不改质检阶段 A-F 的业务逻辑。
- 不引入新的 UI 页面。
- 不改变补充文档的存储模型，除非实现过程中发现它强依赖旧的 `scan-root/<task-id>` 结构。
- 不把项目唯一键从 `task_id` 升级为 `(batch, task_id)`。本轮仍假设 TASK ID 全局唯一。
- 不静默删除普通历史项目；只允许通过显式 `--prune-artifacts` 清理确定来自 p2r 产物目录的误扫项目记录。

历史运行只要 DB 中仍有 `artifact_root`，详情页仍应能读取。新运行统一进入 `result/`。

## 4. 现状诊断

### 4.1 扫描

当前 `internal/scanner/scanner.go` 使用 `filepath.WalkDir` 全树遍历。只要某个目录同时包含：

```text
docs/
repo/
original_sessions/
metadata.json
```

就会被 `isProject` 识别为项目。

运行产物里的 `script_input_snapshot` 是原始输入包快照，里面也可能包含同样结构，所以当前逻辑会把它错误识别为任务。这不是单个目录排除能彻底解决的问题，应该改为严格的 batch/task/task 结构扫描。

### 4.2 产物路径

当前 `internal/pipeline/pipeline.go` 中的 `runArtifactRoot` 生成：

```text
<scan-root>/<task-id>/qa/runs/<run-id>
```

这个路径缺少 batch 分类，也把 `qa/runs` 暴露给质检员。截图中旧产物出现在 scan root 下，也会加剧扫描误识别。

### 4.3 时间

当前运行开始时间使用 `time.Now().UTC()`，DB 写入 RFC3339 UTC 是合理的。但 TUI 的 `shortTime` 只是把字符串中的 `T` 替换为空格，没有做时区转换，也保留到秒：

```text
2026-05-07T05:36:18Z -> 2026-05-07 05:36:18
```

表格列宽目前在宽屏下是 16，而展示内容是 19 个字符，所以会截断。

## 5. 实施计划

### Phase 1: 固化路径与时间 helper

新增或集中以下 helper，避免扫描、Stage A、产物、TUI 各自拼路径或各自解释时间：

- `projectlayout.IsBatchDir(name string) bool`
- `projectlayout.IsTaskID(name string) bool`
- `projectlayout.ExpectedProjectPath(root, batch, taskID string) string`
- `projectlayout.HasOriginalSessionMarker(projectPath string) (bool, string)`
- `projectlayout.MetadataTaskID(path string) string`
- `projectlayout.SafePathSegment(value, fallback string) string`
- `pipeline.runArtifactRoot(scanPath string, project scanner.Project, runID string) string`
- `displaytime.LoadLocation() *time.Location`
- `displaytime.FormatMinute(value string) string`
- `displaytime.FormatSecond(value string) string`
- `displaytime.RunID(start time.Time) string`

建议新增轻量内部包，例如 `internal/projectlayout` 和 `internal/displaytime`。不要把时区 helper 放在 `internal/tui`，因为 `pipeline` 也需要用它生成 run ID，`cmd/status.go` 也可能复用展示格式；把它放在 TUI 会造成反向依赖或重复实现。

`displaytime.RunID(start)` 规则：

- 入参仍使用 UTC `start := time.Now().UTC()`，DB 存储不变。
- 目录名使用 `start.In(displaytime.LoadLocation())` 格式化为 `run-YYYYMMDD-HHmmss-ffffff`。
- 微秒后缀使用 `start.Nanosecond()/1000` 并 `%06d` 零填充；如果后续需要更强保证，可追加短随机/单调序列，但不能回退到 `%1000000`。

验收点：

- UTC `2026-05-07T05:36:18Z` 展示为 `2026-05-07 13:36`。
- 无法解析的历史时间字符串不 panic；总览表格降级值必须宽度安全，详情页可以展示原始字符串。
- `Asia/Shanghai` 加载失败时用固定 `UTC+8` fallback。
- 连续构造两个同一秒但不同微秒的时间，生成的 run ID 不碰撞且都使用上海日期/时间。

### Phase 2: 重写 `p2r scan` 边界

修改 `internal/scanner/scanner.go`：

1. 不再对整个 scan root 做无约束 `WalkDir`。
2. 只遍历 scan root 的直接子目录，筛选 batch 目录。
3. 对每个 batch，只遍历直接子目录作为 task ID 候选。
4. 只检查 `<batch>/<task-id>/<task-id>` 这个确定路径。
5. 符合项目材料要求才加入 `Result.Projects`。
6. `Project.Batch` 必须来自 batch 目录名。
7. `Project.TaskID` 必须来自 task 目录名，不能再由 `metadata.json.task_id`、`metadata.json.id` 或第三层 basename 以外的值覆盖。
8. `Project.Path` 必须是第三层同名 task 目录。
9. `metadata.json.task_id` 与目录 task ID 不一致时，不改变 canonical TaskID；本轮建议继续索引，并在 Stage A 产生 Blocker finding，避免 scan 阶段过度拒绝现场材料，同时不让不一致静默通过。
10. 排除目录使用显式 helper，不依赖路径字符串碰巧不匹配。

伪代码：

```go
func Scan(root string) (Result, error) {
    root = filepath.Clean(root)
    for _, batchEntry := range os.ReadDir(root) {
        if !batchEntry.IsDir() || excludedTopLevel(batchEntry.Name()) || !projectlayout.IsBatchDir(batchEntry.Name()) {
            continue
        }
        batch := batchEntry.Name()
        batchPath := filepath.Join(root, batch)
        for _, taskEntry := range os.ReadDir(batchPath) {
            if !taskEntry.IsDir() || !projectlayout.IsTaskID(taskEntry.Name()) {
                continue
            }
            taskID := taskEntry.Name()
            projectPath := projectlayout.ExpectedProjectPath(root, batch, taskID)
            if valid := isValidProject(projectPath, taskID); valid {
                append Project{
                    TaskID: taskID,
                    Batch: batch,
                    Path: filepath.Clean(projectPath),
                    MetadataPromptMissing: promptMissing(filepath.Join(projectPath, "metadata.json")),
                    // 如果需要记录 metadata task_id mismatch，新增字段或交给 Stage A 复查。
                }
            }
        }
    }
}
```

保留 `VisitedDirs`，但语义改为“扫描过的候选目录数量”，建议计数 batch 候选和 task 候选，而不是任意子目录。CLI 文案必须从 `visited directories` 改成 `visited candidate directories` 或中文化为“候选目录”。

测试新增：

- `TestScanFindsBatchTaskTaskPackage`
- `TestScanUsesDirectoryTaskIDWhenMetadataDiffers`
- `TestScanIndexesMetadataTaskIDMismatchUnderDirectoryTaskID`
- `TestScanRejectsRootLevelTaskPackage`
- `TestScanRejectsBatchTaskWithoutNestedTask`
- `TestScanRejectsNonBatchTopLevelPackage`
- `TestScanSkipsResultArtifacts`
- `TestScanSkipsQARunSnapshot`
- `TestScanExtractsBatchFromTopLevelBatchDir`
- 更新现有 `TestScanFindsValidPackages`、`TestScanIndexesMissingPrompt` 的 fixture，从旧的 `root/batch-1/TASK-001` 或 `root/TASK-002` 改成 `root/batch-1/TASK-001/TASK-001`。

### Phase 3: 新运行产物写入 `result/<batch>/<task-id>/<run-id>`

修改 `internal/pipeline/pipeline.go`：

1. 将 `runArtifactRoot` 的主路径改为：

```text
filepath.Join(scanPath, "result", safeBatch, safeTaskID, runID)
```

2. `safeBatch` 来自 `project.Batch`，空值 fallback 为 `unbatched`，但正常扫描不应为空；如果 batch 被清洗后为空，也必须 fallback。
3. `safeTaskID` 继续使用现有安全化逻辑，但建议改为复用 `projectlayout.SafePathSegment(project.TaskID, "TASK-UNKNOWN")`，避免 scanner 和 pipeline 规则分叉。
4. 新路径不应落在 `project.Path` 内，因此旧的 `.qa-control/runs/<task>/qa/runs/...` fallback 不应继续作为目标形态。若防御式 fallback 仍保留，应使用 `.qa-control/runs/<safeBatch>/<safeTaskID>/<runID>`，并只在 `result/...` 意外落入项目目录时触发。
5. `os.MkdirAll(filepath.Join(artifactRoot, "logs"))` 仍保持。
6. `run.ArtifactRoot` 写 DB 时使用新路径。
7. `runID` 由 `displaytime.RunID(start)` 生成，`start` 仍为 UTC；`run.StartedAt` 继续写 `start.Format(time.RFC3339)`。

`run_manifest.json` 增加字段：

```json
{
  "batch": "batch-2",
  "project_path": ".../batch-2/TASK-.../TASK-...",
  "artifact_root": ".../result/batch-2/TASK-.../run-...",
  "started_at": "2026-05-07T05:36:18Z",
  "started_at_utc": "2026-05-07T05:36:18Z",
  "started_at_local": "2026-05-07T13:36:18+08:00",
  "timezone": "Asia/Shanghai"
}
```

其中 `started_at` 保留给旧读者，语义仍是 UTC；`started_at_utc` 是显式新字段，避免人工阅读 manifest 时误判。

测试更新：

- 将 `TestRunArtifactRootUsesTaskQAOutsideOriginalPackage` 改名为 `TestRunArtifactRootUsesResultBatchTask` 并更新为新期望。
- 将旧 fallback 测试更新为 `result` 优先，防御 fallback 为 `.qa-control/runs/<batch>/<task>/<run-id>`。
- 新增 batch 为空 fallback 测试。
- 新增 batch/task ID 含非法路径字符时的清洗测试，确认不会产生 `..` 或路径分隔符。
- 新增 run ID 使用上海时间且微秒后缀零填充的测试。
- 新增 manifest 包含 `batch`、`started_at_utc`、`started_at_local`、`timezone` 的测试。

### Phase 4: 同步 Stage A 校验并避免快照复制旧产物

本阶段有两个容易漏掉的联动点：scanner 接受的原始会话 marker 必须与 Stage A 一致，快照复制也不能把旧产物带进新产物。

先同步结构校验：

- `internal/pipeline/stage_a.go` 的 `required["original_sessions"]` 不能再只检查 `original_sessions/`，应调用共享的 `projectlayout.HasOriginalSessionMarker(project.Path)`。
- `stage_a.go` 应读取 `projectlayout.MetadataTaskID(metadata.json)` 并与 `project.TaskID` 比较；非空且不一致时产生 Blocker finding，Evidence 同时包含目录 task ID 和 metadata task ID。
- `structuralFindings()` 的 Rule、Evidence、MinimumFix 需要说明可接受的 marker 列表，Evidence 指向项目根和实际缺失项，而不是固定拼 `original_sessions`。
- `assets/scripts/check_required_artifacts.py` 的 `ROOT_REQUIRED_DIRS` 不能继续硬编码只接受 `original_sessions`；应把 `original_sessions/`、`docs/original-session/`、`docs/original_sessions/` 作为等价 marker。
- `assets/scripts/run_acceptance.py` 中提示文案也要同步，否则脚本 finding 会与 Go fallback finding 互相矛盾。
- 如果本轮不准备修改 Python 脚本，则 scanner 也不能在本轮放开 `docs/original-session/`，只能把它列为后续兼容项。二者必须同进同出。

再检查 `internal/pipeline/artifact_io.go` 中的快照复制排除规则。

当前已经排除 `.qa-control` 和 `qa`，本轮应补充：

- `result`
- `.git`
- `task-docs`
- 其他确认为产物或控制目录的路径

目的：

- 新产物目录不会进入 `script_input_snapshot`。
- 如果历史包里有 `qa/runs`，仍不会被复制到新快照。
- 不要把任意名为 `runs` 的业务目录一概排除；快照复制只排除顶层 `qa`、`result`、`.qa-control`、`task-docs` 等 p2r 控制/产物目录，避免误删用户仓库里的测试数据。

测试新增：

- `TestStageAAcceptsAlternativeOriginalSessionMarkers`
- `TestStageAReportsMetadataTaskIDMismatch`
- `TestRequiredArtifactScriptsAcceptAlternativeOriginalSessionMarkers`
- `TestCopyPackageSnapshotExcludesResultArtifacts`
- `TestCopyPackageSnapshotExcludesTaskDocsControlDir`
- 保留并更新现有 `TestCopyPackageSnapshotExcludesPriorQAArtifacts`

### Phase 5: 最后运行时间转换与列宽修复

修改 `internal/tui/viewmodel.go` 和 `internal/tui/layout.go`：

1. 将 `shortTime` 改为调用 `displaytime.FormatMinute`，解析 RFC3339 并转为上海时间。
2. 总览表格 `last_run` 列输出 `YYYY-MM-DD HH:mm`。
3. `last_run` 列宽固定为 16，不允许 medium 断点继续使用 12。
4. 如果当前断点或总列宽预算无法给到 16 列，则隐藏 `last_run`，不显示被截断内容。
5. 列标题保持 `最后运行`，不要改成更长的 `最后运行时间`，否则会增加宽度预算压力。
6. `overviewDisplayRow()` 对 `last_run` 不应再依赖省略号兜底；可在测试中断言可见时 `lipgloss.Width(value) <= 16` 且不包含 `…`。

建议列策略：

| 断点 | last_run |
|------|----------|
| `>= 120` | 显示，宽度 16 |
| `90-119` | 默认隐藏；若实现动态预算，只有预算充足时才显示，宽度仍为 16 |
| `< 90` | 隐藏 |

如果实现动态预算，应在 `overviewColumnSpecs(width)` 中先计算必选列宽度，再按优先级加入可选列。不要仅把 medium 宽度从 12 改成 16 后直接返回，否则在 90-119 宽度下仍可能整体溢出。

测试新增：

- `TestShortTimeConvertsUTCToShanghai`
- `TestShortTimeFormatsMinutePrecision`
- `TestShortTimeInvalidInputIsWidthSafe`
- `TestOverviewLastRunColumnIsNeverTruncatedWhenVisible`
- `TestOverviewColumnsHideLastRunWhenWidthInsufficient`
- `TestOverviewMediumDoesNotShowTwelveColumnLastRun`

### Phase 6: DB 与历史兼容

DB schema 默认不需要迁移：

- `projects.batch` 已存在。
- `projects.path` 可以更新为新的第三层 task 目录。
- `runs.artifact_root` 已是普通字符串，可以保存新 result 路径。
- `projects.last_run_at` 继续保存 UTC RFC3339。

兼容策略：

- 旧 runs 的 `artifact_root` 保持不动，详情页继续按 DB 路径读取。
- 新 runs 写入 `result/`。
- 重新执行 `p2r scan` 后，旧 root-level `TASK-.../qa/runs/...` 不会再被识别为项目。
- 如果 DB 中存在历史误扫入的 `script_input_snapshot` 项目，单纯 upsert 不一定能清理 TUI 列表，因此必须提供显式 prune。

本轮明确行为：

- `p2r scan` 只 upsert 新识别项目，不自动删除 DB 项目，避免误删历史数据。
- 新增 `p2r scan --prune-artifacts`，只清理确定落在 p2r 产物/控制目录中的误扫项目记录，例如路径位于当前 scan root 下，且相对路径组件命中顶层 `result`、顶层 `.qa-control`、`script_input_snapshot`，或连续组件 `qa/runs`。
- prune 判断必须用 `filepath.Rel` 后的路径组件判断，不要用硬编码 `/` 字符串，以免 Windows 路径或相对路径下误判。
- `--prune-artifacts` 只删除 `projects` 里的误扫项目行，不删除任何 artifact 文件和 run 目录；命令输出必须报告删除了哪些 task/path。
- 如果误扫项目已经有关联 runs，默认跳过并报告 `blocked_by_runs=<n>`，避免制造孤儿 run 记录；后续如确需清理，另做 `admin prune-projects --force --yes` 并在事务中处理关联数据。
- 不做“本次扫描结果之外的所有项目都删除”的通用 prune。通用 prune 如需支持，另开 `p2r scan --prune-missing --yes` 或 `p2r admin prune-projects`。

受污染 DB 的验收必须使用：

```bash
p2r scan --path ./projects-qa --prune-artifacts
```

否则修复后的 scanner 虽然不会新增快照项目，但 TUI 仍可能显示历史 DB 中的旧误扫记录。

测试新增：

- `TestScanPruneArtifactsRemovesSnapshotProjectWithoutRuns`
- `TestScanPruneArtifactsSkipsProjectWithRuns`
- `TestScanPruneArtifactsDoesNotRemoveNormalProject`
- `TestScanPruneArtifactsRequiresPathUnderScanRoot`

### Phase 7: CLI 与 TUI 回归

命令层检查：

- `cmd/scan.go` 输出中 batch、项目数量、候选目录数量应符合新扫描规则；如果带 `--prune-artifacts`，还要输出 prune 数量和被清理路径。
- `cmd/run.go` 不需要改变参数，但输出的 artifact root 应变为 `result/<batch>/<task-id>/<run-id>`。
- `cmd/status.go` 如果新增或已有 started/finished/last run 展示，应复用 `displaytime`；不需要为了本轮强行新增时间字段输出。
- `cmd/docs.go` 和 `internal/taskdocs` 暂不改变路径模型，但因为仍按 `task_id` 存储，跨 batch 重名仍不支持。

TUI 检查：

- fresh DB 下总览不再出现 `script_input_snapshot`；受污染 DB 下，对没有关联 runs 的误扫项目运行 `scan --prune-artifacts` 后不再出现；有关联 runs 的误扫项目会被报告为 skipped，需要后续 admin force 清理。
- batch 列展示 `batch-1`、`batch-2`。
- 最后运行时间显示为上海时间。
- 时间列不出现 `2026-05-07 05:...` 这种 UTC 误导。
- 时间列不出现右侧省略号或截断。

## 6. 验收用例

准备测试目录：

```text
projects-qa/
  batch-2/
    TASK-20260318-3CC794/
      TASK-20260318-3CC794/
        metadata.json
        repo/
        docs/
        original_sessions/
  result/
    batch-2/
      TASK-20260318-3CC794/
        run-20260507-133618-301874/
          script_input_snapshot/
            metadata.json
            repo/
            docs/
            original_sessions/
  TASK-20260327-7817C9/
    qa/
      runs/
        run-20260507-053618-301874/
          script_input_snapshot/
            metadata.json
            repo/
            docs/
            original_sessions/
```

期望：

- `p2r scan --path projects-qa` 只识别 `TASK-20260318-3CC794`。
- `result/.../script_input_snapshot` 不识别。
- `TASK-.../qa/runs/.../script_input_snapshot` 不识别。
- 识别出的项目 batch 是 `batch-2`。
- 识别出的项目 path 是 `projects-qa/batch-2/TASK-.../TASK-...`。
- 新运行产物写入 `projects-qa/result/batch-2/TASK-.../run-...`。
- TUI 总览最后运行时间显示 `2026-05-07 13:36`，不是 `2026-05-07 05:36`。
- 如果 DB 预先插入了 path 指向 `script_input_snapshot` 且没有关联 runs 的误扫项目，`p2r scan --path projects-qa --prune-artifacts` 后该项目不再出现在 TUI 总览。
- 如果 `metadata.json.task_id` 与目录 task ID 不一致，scan 仍以目录 task ID 入库，Stage A 产生明确 finding，而不是把项目重命名成 metadata 中的值。

## 7. 回归命令

实现后至少运行：

```bash
go test ./...
go vet ./...
go build ./...
```

重点单测：

```bash
go test ./tests/internal/scanner -run Scan
go test ./tests/internal/pipeline -run 'RunArtifactRoot|CopyPackageSnapshot|Manifest'
go test ./tests/internal/tui -run 'ShortTime|Overview|Layout'
```

手工验证：

```bash
./p2r scan --path ./projects-qa
./p2r scan --path ./projects-qa --prune-artifacts
./p2r tui --path ./projects-qa
```

若本地二进制尚未构建，则先执行：

```bash
go build -o p2r .
```

## 8. 风险与决策点

### 8.1 Task ID 是否全局唯一

当前 DB 使用 `task_id` 作为项目主键，`cmd/run.go`、`cmd/docs.go`、`taskdocs.StoreDir()`、TUI 选择和 runs 关联也都只按 task ID 工作。计划默认 `TASK-ID` 全局唯一。如果未来不同 batch 允许重复 task ID，则需要把项目唯一键升级为 `(batch, task_id)`，并同步修改命令参数、补充文档路径、run 关联和 TUI 选择，不建议混入本轮。

### 8.2 是否自动清理历史误扫项目

修复 scan 后，新的扫描不会再引入 `script_input_snapshot`。但历史 DB 中已经存在的误扫项目不会自动消失。

默认不静默删除普通历史项目。本轮只新增 artifact 定向清理：

```bash
p2r scan --path ./projects-qa --prune-artifacts
```

该命令默认只清理没有关联 runs 的误扫项目；有关联 runs 的项目先报告并跳过，避免破坏历史 run 查询。

后续如需通用清理，再做需要显式确认的命令，例如：

```bash
p2r admin prune-projects --path ./projects-qa --yes
```

### 8.3 Run ID 使用 UTC 还是上海时间

DB 时间保持 UTC。目录名使用上海时间，因为目录是质检员直接查找的界面之一。无论 run ID 如何生成，TUI 的最后运行时间必须以 DB 中的 RFC3339 时间为准，不能从 run ID 反推。

实现时要特别避免旧逻辑 `start.UnixNano()%1000000`，它只保留纳秒值的最后 6 位，同一秒内相隔整数毫秒的两次运行可能得到相同后缀。使用 `start.Nanosecond()/1000` 的零填充微秒，或在此基础上追加短随机/单调后缀。

### 8.4 原始会话目录名称

当前现场样例中存在 `original_sessions/`。用户描述中提到 `docs original-session` 语义。实现时应把原始会话判断集中在 `hasOriginalSessionMarker`，首批兼容：

- `original_sessions/`
- `docs/original-session/`
- `docs/original_sessions/`

这样不会把目录命名差异扩散到扫描主流程。

注意：内置 `assets/scripts/check_required_artifacts.py` 和 `assets/scripts/run_acceptance.py` 当前仍写死或提示 `original_sessions/`。如果本轮 scanner 放开替代 marker，必须同步修改这些脚本和 Stage A fallback，否则同一个项目会出现“scan 接受、Stage A 又报缺材料”的矛盾。

## 9. 建议实施顺序

1. 先抽出 `projectlayout` 和 `displaytime` helper，补齐 task/batch/time/run ID 的纯函数测试。
2. 再改 scanner，并补齐 scanner 单测；同步更新旧 fixture 到 `<batch>/<task-id>/<task-id>`。
3. 同步 Stage A 和内置 Python 校验脚本的 original session marker 规则，避免 scan 与 A 阶段互相矛盾。
4. 再改 artifact root 到 `result/<batch>/<task-id>/<run-id>`，更新 pipeline 单测和 e2e fixture。
5. 再改 manifest 时间字段与 run id 生成策略。
6. 再实现 `scan --prune-artifacts`，用于受污染 DB 的显式清理。
7. 再改 TUI 时间格式和表格列宽。
8. 最后跑全量测试和一次真实 `projects-qa` 手工扫描；如果本机 DB 已受污染，再跑一次 `scan --prune-artifacts` 验证总览清理效果。

这个顺序的好处是先切断“扫进产物目录”的源头，再调整新产物归档，最后修展示。每一步都可以单独验证，失败时定位范围比较清楚。
