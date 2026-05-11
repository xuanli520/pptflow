# Codex Delta 日志压缩与 TUI 流式预览修复方案

## 审查结论

本轮复核对象为 `internal/pipeline/codex_app_server_session.go`、`internal/pipeline/codex_session.go`、`internal/pipeline/stage_a.go`、`internal/pipeline/stage_b.go`、`internal/pipeline/stage_c.go`、`internal/pipeline/stage_codex.go`、`internal/pipeline/stage_f.go`、`internal/pipeline/pipeline.go`、`internal/pipeline/run_lifecycle.go`、`internal/executor/cmd.go`、`internal/scheduler/scheduler.go`、`internal/tui/app.go`、`internal/tui/viewmodel.go` 与 `internal/tui/render.go`。

原方案方向正确：`item/agentMessage/delta` 逐条落日志会把 Codex 的流式回复拆散成大量元数据行，既看不到真实文字，也污染 TUI 的日志尾部预览。但对照现有代码后，还存在几处需要先修正的实现风险：

1. **只压缩日志不足以支撑 TUI 实时预览**：当前 TUI 详情页通过 `reload()` 周期性重读 DB 和日志尾部，最多看到压缩后的聚合日志，无法 token/delta 级展示 Codex 正在生成的回复。
2. **聚合输出时机需要避免锁内 I/O**：`appendLog` 会重新取锁并做文件 I/O，聚合函数必须先在锁内复制快照与标记状态，解锁后再写日志。
3. **`prefix10` 必须按 rune 截取而不是 byte 截取**：Codex 回复可能包含中文或 emoji，按 byte 截断会产生非法 UTF-8。
4. **`total_bytes` 与 `text_prefix` 语义需要固定**：`total_bytes` 应为 UTF-8 字节数 `len(deltaText)`，`text_prefix` 应为前 10 个 rune 经 `truncateAppServerLogValue` 转义后的可见字符串。
5. **`complete()` 兜底需要在关闭 `done` 前完成聚合日志，并在复制 `ArtifactWarnings` 前完成日志写入**：否则 `Wait()` 返回后可能读不到最后一批聚合行；若聚合日志写入失败，warning 也可能漏进最终 `CodexReviewResult`。
6. **`readStdout()` 只能跳过无 `id` 的 delta notification 日志**：不能用 `message.Method != "item/agentMessage/delta"` 粗略判断，否则理论上会吞掉同名 server request 的诊断日志。
7. **`item/completed` 之后仍到达 delta 的边界要明确**：这属于协议乱序或异常情况，聚合日志只输出一次，最终报告仍以 `items[itemID]` 的 completed 文本优先，后续 late delta 不应重新打开 TUI 的“生成中”预览。
8. **B/C 追加流不能只存在 TUI 本地缓冲**：`scheduler.NotifyCh()` 是合并通知，TUI 通过 `Snapshot()` 拉取状态；如果 scheduler 只保存最后一条 append event，快速输出会丢中间行。追加模式必须由 scheduler 持有环形缓冲并在快照中暴露。
9. **executor 接口描述必须贴合现有代码并一次性迁移**：当前没有 `executor.RunArgs`，B/C 使用的是 `CommandRunner.RunStreaming(...)`；方案应直接替换为带输出回调的新 streaming 接口，并删除旧接口签名。
10. **执行详情右侧信息层级需要重排**：当前右侧把状态、证据、路径、日志、报告摘要混在一个滚动文本里；加入流式回复后会更拥挤，必须先拆分出稳定的显示区域。
11. **测试期望必须同步更新**：现有 `TestAppServerSessionCompactsDeltaLogsAndKeepsDeltaReport` 仍期望逐条 delta 元数据行，改造后应断言只出现一条 aggregated 行，且不包含 raw JSON envelope。

## 目标

- 同一 `itemId` 的多条 `item/agentMessage/delta` 不再逐条写日志。
- 每个有 delta 文本的 item 最多写一条聚合日志，包含总字节数、短 hash、contract marker 计数与前 10 个可见字符。
- Codex 最终报告组装逻辑不变：优先使用 `item/completed` 的完整文本，缺失时使用累积 delta 文本兜底。
- TUI 执行详情页在 Codex D/E/F 阶段运行中实时展示正在流式生成的回复，B/C 阶段运行中实时展示进程 stdout/stderr 输出，且追加输出在 scheduler 合并通知时不丢行。
- 执行详情页右侧重新设计为三层结构：状态栏（viewport 外）+ 主内容区（实时输出 / 发现报告）+ 证据区。砍掉与左侧面板重复的 A-F 阶段轨迹区域。
- 实时预览仅保留内存态短窗口，不写入 DB，不改变正式 QA 产物契约。

## 涉及文件

日志压缩：

- `internal/pipeline/codex_app_server_session.go`
- `internal/pipeline/codex_app_server_session_internal_test.go`

流式预览：

- `internal/pipeline/codex_session.go`
- `internal/pipeline/stage_a.go`
- `internal/pipeline/stage_b.go`
- `internal/pipeline/stage_c.go`
- `internal/pipeline/stage_codex.go`
- `internal/pipeline/stage_f.go`
- `internal/pipeline/pipeline.go`
- `internal/pipeline/run_lifecycle.go`
- `internal/executor/cmd.go`
- `internal/scheduler/scheduler.go`
- `internal/tui/app.go`
- `internal/tui/viewmodel.go`
- `internal/tui/render.go`
- `internal/tui/pipelineview.go`
- `internal/tui/testhooks.go`
- `tests/internal/executor/*`
- `tests/internal/pipeline/*`
- `tests/internal/scheduler/*`
- `tests/internal/tui/*`

## 一、Delta 日志压缩修复

### 1. 结构体新增字段

在 `appServerCodexReviewSession` 中增加：

```go
deltaLogged map[string]bool
```

放在 `deltas map[string]string` 下方。用途是标记某个 `itemId` 的聚合 delta 日志是否已经输出，防止 `item/completed`、`turn/completed` 和 `complete()` 兜底重复写同一 item。

### 2. `Start()` 初始化

在 `s.deltas = map[string]string{}` 之后初始化：

```go
s.deltaLogged = map[string]bool{}
```

测试中手工构造 `appServerCodexReviewSession` 的用例可以同步补上 `deltaLogged: map[string]bool{}`，但生产 helper 仍必须对 nil map 做防御：

```go
if s.deltaLogged == nil {
    s.deltaLogged = map[string]bool{}
}
```

这样旧测试、临时同包测试或未来局部构造 session 时不会因为漏初始化而 panic。

### 3. `readStdout()` 跳过 delta 即时日志

现状：

```go
s.appendLog(formatAppServerRPCLogLine(message))
```

改为只跳过无 `id` 的 delta notification：

```go
isDeltaNotification := len(message.ID) == 0 && message.Method == "item/agentMessage/delta"
if !isDeltaNotification {
    s.appendLog(formatAppServerRPCLogLine(message))
}
```

`delta` 仍通过 `handleNotification` -> `recordDelta` 进入 `s.deltas`，只是不再逐条写入日志。响应分发、服务端请求拒绝、普通 notification 处理顺序保持不变；如果未来 app-server 发来带 `id` 的同名 server request，也仍会留下 compact 诊断日志。

### 4. 新增 `prefixRunes`

不要实现 byte 级 `prefix10`。应新增 rune 安全 helper：

```go
func prefixRunes(value string, limit int) string {
    if limit <= 0 {
        return ""
    }
    for index := range value {
        if limit == 0 {
            return value[:index]
        }
        limit--
    }
    return value
}
```

调用处使用 `prefixRunes(deltaText, 10)`。这样中文、多字节符号不会被截坏。

### 5. 新增聚合日志快照类型

为避免锁内 I/O，先定义内部快照类型：

```go
type aggregatedDeltaLog struct {
    turnID string
    itemID string
    text   string
}
```

聚合日志格式：

```text
JSON-RPC notification item/agentMessage/delta aggregated turn=<compactID> item=<compactID> total_bytes=<N> delta_sha256=<hash12> contract_starts=<N> contract_ends=<N> text_prefix="<escaped prefix>"
```

字段语义：

- `total_bytes`: `len(text)`，即 UTF-8 字节数。
- `delta_sha256`: 聚合后完整 delta 文本的短 hash，便于和最终报告排查关联。
- `contract_starts` / `contract_ends`: 对聚合后完整文本调用 `staticReviewMarkerCounts(text)`。
- `text_prefix`: `truncateAppServerLogValue(prefixRunes(text, 10))`，外层仍用 `%q` 输出。

### 6. 新增单 item 聚合函数

`(s *appServerCodexReviewSession) aggregatedDeltaLogForItem(itemID string) (aggregatedDeltaLog, bool)`

实现要求：

1. `itemID` 为空时直接返回 false。
2. 持锁读取 `s.deltas[itemID]`、`s.deltaLogged[itemID]`、`s.turnID`。
3. 若 delta 为空或已 logged，解锁返回 false。
4. 若 `s.deltaLogged == nil`，先初始化 map，再标记 `s.deltaLogged[itemID] = true`。
5. 复制 `turnID`、`itemID`、`text` 到快照后解锁。

`(s *appServerCodexReviewSession) logAggregatedDelta(itemID string)` 调用上述快照函数，若返回 true，再调用纯格式化 helper 后 `appendLog`。不得在持有 `s.mu` 时调用 `appendLog`。

### 7. 新增残余聚合函数

`(s *appServerCodexReviewSession) remainingAggregatedDeltaLogs() []aggregatedDeltaLog`

实现要求：

1. 持锁遍历 `s.deltas`。
2. 跳过空文本和 `s.deltaLogged[itemID] == true` 的 item。
3. 若 `s.deltaLogged == nil`，先初始化 map；对待输出 item 立即设置 `s.deltaLogged[itemID] = true`。
4. 复制快照到 slice。
5. 解锁后按 `itemID` 排序，保证日志稳定。

`(s *appServerCodexReviewSession) logRemainingAggregatedDeltas()` 遍历快照并写日志。仍然不得锁内 I/O。

### 8. 三个调用点

| 触发场景 | 位置 | 调用 |
|---|---|---|
| 单个 item 完成 | `handleNotification` -> `item/completed` 分支，`recordCompletedItem` 之后 | `s.logAggregatedDelta(params.Item.ID)` |
| turn 完成兜底 | `handleNotification` -> `turn/completed` 分支，遍历 items 并 `recordCompletedItem` 之后、`completeTurn` 之前 | `s.logRemainingAggregatedDeltas()` |
| 进程异常退出最终兜底 | `complete()` 内，标记 `completed=true` 并释放 `s.mu` 之后、复制 `s.warnings` 到 `s.result.ArtifactWarnings` 之前、`close(done)` 之前 | `s.logRemainingAggregatedDeltas()` |

第三个调用点覆盖 Codex 崩溃、超时、stdout 提前结束、没有收到 `turn/completed` 的场景。必须放在 `close(done)` 前，否则 `Wait()` 可能先返回，调用方读取日志时聚合行还没写完。也必须放在最终复制 `s.warnings` 之前，否则聚合日志 append 失败产生的 `ArtifactWarning` 不会进入返回结果。`cancel()` 可以在聚合前调用，用于停止仍在运行的子进程；关键约束是先把 `completed` 标记好、不要锁内 I/O、最后再 close `done`。

推荐将 `complete()` 调整为两段式收口：

```go
func (s *appServerCodexReviewSession) complete(result executor.Result, err error) {
    s.mu.Lock()
    if s.completed {
        s.mu.Unlock()
        return
    }
    s.completed = true
    cancel := s.cancel
    done := s.done
    s.responses = map[int]chan appServerRPCMessage{}
    s.mu.Unlock()

    if cancel != nil {
        cancel()
    }
    s.logRemainingAggregatedDeltas()

    s.mu.Lock()
    s.result.Result = result
    s.result.ArtifactWarnings = append(s.result.ArtifactWarnings, s.warnings...)
    s.err = err
    s.mu.Unlock()

    if done != nil {
        close(done)
    }
}
```

这样能同时保证：不重复 complete、不锁内 I/O、warning 不丢、`Wait()` 返回时日志已经稳定。

### 9. 保持不变

- `formatAppServerRPCLogLine` 中 `item/agentMessage/delta` 的格式化分支可以保留，用于单元测试或临时调试，但正常 `readStdout` 路径不再调用它。
- `recordDelta` / `recordCompletedItem` / `finalReportLocked` 的报告组装语义保持不变；但 `recordDelta` 和 `recordCompletedItem` 在 `s.completed == true` 时应直接返回，避免 `complete()` 两段式收口期间还有 late notification 改写内存状态。
- `item/completed` 与 `turn/completed` 现有 compact 元数据日志保留。
- 聚合 delta 行是补充诊断信息，不替代 final report artifact。

### 10. 日志压缩测试更新

新增或更新测试：

- 多个 delta 后 `turn/completed`：日志只出现 1 条 `item/agentMessage/delta aggregated`，不出现逐条 `delta_bytes=...`。
- `item/completed` 后输出聚合行，再 `turn/completed` 不重复。
- 没有 `turn/completed` 但调用 `complete()`：仍输出残余聚合行。
- 中文前缀：`text_prefix` 不产生非法 UTF-8，按 10 个 rune 截断。
- 空 delta 或只有 completed text：不输出 aggregated 行。
- 乱序场景 `item/completed` 后又收到 delta：不重复聚合；最终报告仍优先 completed text。

## 二、TUI 实时预览加入流式输出回复

### 1. 当前问题

当前 TUI 执行详情页的数据路径是：

```text
pipeline stage 写日志/DB
-> scheduler 只保存阶段状态快照
-> TUI 收到 scheduler notify 后 reload()
-> buildExecutionViewModel() 从 DB 读 stage，再 readLogTail()
-> render detail
```

这个路径适合阶段状态刷新，但不适合 Codex 流式回复：

- `readLogTail()` 只能读日志文件尾部，不知道哪一段是 agent 正文。
- delta 压缩后，日志只保留前 10 个字符和 hash，更不能作为正文预览来源。
- DB 只有 `StageRecord`，没有运行中的 delta 文本字段；把每个 delta 写 DB 会造成高频写入和表结构污染。
- `scheduler.NotifyCh()` 目前只传“有更新”，没有 delta payload；但 scheduler 可以在内存快照里保留运行中预览。

因此实时预览应走内存事件通路，而不是解析日志或写 DB。

### 2. 数据模型新增

在 `pipeline.RunProgress` 增加可选字段：

```go
Stream *StreamUpdate
```

新增类型：

```go
type StreamMode int

const (
    StreamModeCumulative StreamMode = iota  // Codex D/E/F：Text 是完整累积文本，每次推送替换
    StreamModeAppend                         // B/C 进程输出：pipeline 发 Delta，scheduler 聚合 Lines
)

type StreamLine struct {
    Source string // `stdout` / `stderr` / `p2r`
    Text   string
}

type StreamUpdate struct {
    Stage     string
    Mode      StreamMode  // 输出模式：累积型（Codex）或追加型（进程 stdout/stderr）
    ItemID    string      // 累积模式专用：Codex itemId，多 item 去重
    Text      string      // 累积模式：当前 item 完整累积文本；追加模式：scheduler 拼好的尾部文本 fallback
    Delta     string      // 追加模式：本次新增行；累积模式：本次增量（测试/动效用）
    Source    string      // 追加模式本次新增行来源：`stdout` / `stderr` / `p2r`；累积模式留空
    Lines     []StreamLine // 追加模式：scheduler 维护的最近 N 行快照，TUI 直接渲染
    Done      bool        // 对应 item/turn 或进程已完成
    Truncated bool        // 内存预览超过上限后被截断
}
```

字段语义：

- `Stage`: 阶段字母 A-F。
- `Mode`: `StreamModeCumulative` 用于 Codex（TUI 直接显示 `Text`，按详情宽度 wrap）；`StreamModeAppend` 用于 B/C 进程输出（pipeline 发送单行 `Delta`，scheduler 维护环形行缓冲区并在快照中暴露 `Lines`）。
- `ItemID`: Codex `itemId`，多 item 场景去重与替换。追加模式下忽略。
- `Text`: 累积模式下当前 item 完整累积后的预览文本。追加模式下作为 `Lines` 的纯文本 fallback（例如测试断言或未来无样式渲染）。
- `Delta`: 追加模式下为单行新增输出；累积模式下为本次增量，仅用于测试或未来 UI 动效。
- `Source`: 追加模式下区分本次新增行的 `stdout` / `stderr` / `p2r`，scheduler 写入 `Lines`；累积模式留空。
- `Lines`: 追加模式下由 scheduler 深拷贝的最近 200 行。不能只让 TUI 本地累积，因为 notify 是合并信号，快照只保留最后一条会丢行。
- `Done`: 对应 item/turn 已完成（Codex）或进程已退出（B/C）。
- `Truncated`: 累积模式下内存预览超过 64 KiB 后被截断（只保留尾部）；追加模式下表示 scheduler 环形缓冲已经淘汰过旧行。

新增 `ProgressEvent`：

```go
EventStageStream ProgressEvent = "stage_stream"
```

该事件不写 DB，不调用 `PutStage`，只用于运行中 UI。

**B/C 阶段流式输出的设计考量：** 虽然 B/C 阶段的进程 stdout/stderr 会写入日志文件、TUI 可通过 `readLogTail()` 轮询看到尾部，但轮询有延迟（受 `RefreshIntervalMS` 控制），用户在长时间运行的 Docker 启动或测试执行期间会感觉"卡住了"。走同一套 `EventStageStream` 推送机制可以获得实时体验，且与 Codex 流式输出共用 TUI 渲染基础设施。日志文件写入不变——stream 事件是内存态的补充通道，不替代文件日志。

### 3. Codex session 增加 delta 回调

在 `CodexReviewRequest` 增加：

```go
OnDelta func(update CodexDeltaUpdate)
```

新增内部类型：

```go
type CodexDeltaUpdate struct {
    TurnID    string
    ItemID    string
    Delta     string
    Text      string
    Done      bool
    Truncated bool
}
```

`appServerCodexReviewSession.recordDelta` 在成功累积 `s.deltas[itemID] += delta` 后，更新单独的预览文本并复制快照，解锁后调用 `s.req.OnDelta`。不要在锁内调用外部回调，避免回调进入 scheduler/TUI 后形成锁链。

`item/completed` 或 `turn/completed` 到达时，应对对应 item 再发一次 `Done=true` 的回调，用于 TUI 停止显示“仍在生成”的状态。`Text` 优先使用 completed item 文本经过同一 64 KiB 预览上限处理后的尾部窗口；如果 completed text 为空，才回退到 preview 文本。该 done 回调同样必须先在锁内复制快照，解锁后触发。正式报告仍使用 `s.items` / `s.deltas` 的完整文本，不受预览上限影响。

`item/completed` 后又收到同 item delta 时，仍可把 late delta 记录到 `s.deltas` 以便诊断，但不要再触发普通 `OnDelta` 去刷新 TUI，也不要把 UI 状态从 Done 重新变成生成中。最终报告仍由 `finalReportLocked()` 优先取 `s.items[itemID]`。

预览内存上限建议：

- 单 item 最多保留 `64 KiB` 文本用于 TUI 预览。
- 超限时保留尾部文本，并设置 `Truncated=true`。尾部截断必须保证 UTF-8 有效，可以从截断点向后移动到下一个 rune 边界，或用 `strings.ToValidUTF8` 做最后防线。
- `finalReportLocked()` 仍使用完整 `s.deltas`，不能因为 TUI 预览上限截断正式结果。

可以用单独字段保存预览文本，例如：

```go
deltaPreview map[string]string
deltaPreviewTruncated map[string]bool
```

这些字段在 `Start()` 与同包测试手工构造 session 时初始化；helper 内仍做 nil map 防御。不要截断 `deltas` 本身。

### 3b. B/C 阶段 executor 层增加输出回调

B/C 阶段的进程 stdout/stderr 目前通过 `CommandRunner.RunStreaming(...)` 写入日志文件，没有回调机制。为支持实时流式输出，应一次性替换现有 streaming API，而不是引入当前代码中不存在的 `executor.RunArgs`；旧 `RunStreaming(...)` 方法和接口项直接删除。

在 `internal/executor/cmd.go` 中将现有 `RunStreaming(...)` 替换为：

```go
type OutputCallback func(line string, source string)

func (Runner) RunStreamingWithOutput(
    ctx context.Context,
    timeout time.Duration,
    dir string,
    env []string,
    writer io.Writer,
    onOutput OutputCallback,
    name string,
    args ...string,
) Result
```

同步替换 `internal/executor/cmd.go` 和 `internal/pipeline/pipeline.go` 中的 `CommandRunner` 接口，并补齐所有测试 fake runner：

```go
RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result
```

实现要求：

- stdout 和 stderr 仍分别进入 `Result.Stdout` / `Result.Stderr`，同时写入传入的 `writer`，保持现有日志文件行为。
- `onOutput` 按行触发，`source` 固定为 `stdout` 或 `stderr`。阶段自身写入的 step marker 可用 `source="p2r"` 通过 progress 直接发送，不需要经过 executor。
- stdout/stderr 写同一个 `writer` 时必须加互斥，避免两路 goroutine 把日志行写穿插。
- 读取 stdout/stderr 不能被慢回调阻塞。推荐 stdout/stderr 各自 scanner goroutine 只负责写内部 channel，另一个轻量 goroutine 顺序调用回调；或约定回调只做非阻塞 `progress(...)` 并在 scheduler 内 O(1) 入缓冲。
- scanner buffer 上限沿用现有命令输出容量策略，或至少不低于当前 streaming 路径能处理的常见长行，避免长日志行直接中断进程读取。
- 删除旧 `RunStreaming(...)` 方法和接口项；所有生产调用点、测试 fake runner、`internal/preflight` / `internal/codex` 相关接口适配必须一次性编译到新签名。

### 4. `runCodexReviewWithLog` 透传回调

把 `runCodexReviewWithLog` 签名扩展为：

```go
func (r Runner) runCodexReviewWithLog(
    ctx context.Context,
    timeout time.Duration,
    projectPath, logPath string,
    env []string,
    prompt string,
    capability codex.Capability,
    args []string,
    onDelta func(CodexDeltaUpdate),
) CodexReviewResult
```

构造 `CodexReviewRequest` 时设置 `OnDelta: onDelta`。

所有调用点必须显式传入回调；没有进度需求的单元测试可以传 nil，生产路径统一走新接口。

### 5. 各 stage 发出 stream progress

#### 5a. stage D/E/F（Codex 累积模式）

`stageCodex` 和 `stageF` 需要拿到 `ProgressReporter`。推荐做法是把 `runState` 的 `progress` 注入到 Codex 执行路径，而不是让 `Runner.executeStage` 直接知道 TUI。

最小改造路径：

1. 将 `executeStage` 签名扩展为：

```go
func (r Runner) executeStage(
    ctx context.Context,
    run model.RunRecord,
    project scanner.Project,
    stage string,
    prior map[string]model.StageRecord,
    opts RunOptions,
    preflightResult preflight.CheckResult,
    progress func(RunProgress),
) model.StageRecord
```

2. `runState.executeStageLoop` 调用时传入 `s.progress`。
3. `stageA`、`stageB`、`stageC`、`stageCodex` 与 `stageF` 签名统一增加 `progress func(RunProgress)`；`stageA` 直接忽略该参数，避免 `executeStage` 里出现两套调用形态。
4. 构造 `onDelta`：

```go
onDelta := func(update CodexDeltaUpdate) {
    if progress == nil {
        return
    }
    progress(RunProgress{
        RunID: run.RunID,
        Stage: stage,
        Event: EventStageStream,
        Stream: &StreamUpdate{
            Stage:     stage,
            Mode:      StreamModeCumulative,
            ItemID:    update.ItemID,
            Text:      update.Text,
            Delta:     update.Delta,
            Done:      update.Done,
            Truncated: update.Truncated,
        },
    })
}
```

#### 5b. stage B/C（进程输出追加模式）

`stageB` 和 `stageC` 调用 `r.exec.RunStreamingWithOutput(...)` 时传入 `onOutput` 回调：

```go
onOutput := func(line string, source string) {
    if progress == nil {
        return
    }
    progress(RunProgress{
        RunID: run.RunID,
        Stage: stage,
        Event: EventStageStream,
        Stream: &StreamUpdate{
            Stage:  stage,
            Mode:   StreamModeAppend,
            Delta:  line,
            Source: source,
            Done:   false,
        },
    })
}
```

进程退出后（`executor.Result` 返回），再发一次 `Done=true` 的 stream 事件，通知 TUI 该阶段进程输出已结束（可停止显示"运行中"动画）：

```go
progress(RunProgress{
    RunID: run.RunID,
    Stage: stage,
    Event: EventStageStream,
    Stream: &StreamUpdate{
        Stage: stage,
        Mode:  StreamModeAppend,
        Done:  true,
    },
})
```

阶段自身写入的可读 step marker（例如 `=== B1 docker compose pull start ===`、`=== C host run_tests.sh start ===`）如果希望也出现在实时预览中，可以直接发送 `StreamModeAppend` 且 `Source: "p2r"` 的 progress 事件；进程 stdout/stderr 仍由 executor 回调负责。

### 6. scheduler 保存内存态预览

在 `scheduler.Job` 和 `scheduler.JobSnapshot` 中增加对外快照字段；在 `Job` 内部额外保存追加模式的行缓冲：

```go
type Job struct {
    // ... 现有字段 ...
    StreamByStage map[string]pipeline.StreamUpdate

    // 仅 scheduler 内部使用：追加模式最近 N 行，避免 notify 合并导致 TUI 丢行。
    streamLinesByStage map[string][]pipeline.StreamLine
}

type JobSnapshot struct {
    // ... 现有字段 ...
    StreamByStage map[string]pipeline.StreamUpdate
}
```

`Snapshot()` 必须深拷贝 map 和每个 `StreamUpdate.Lines`：

```go
func cloneStreamByStage(input map[string]pipeline.StreamUpdate) map[string]pipeline.StreamUpdate {
    if len(input) == 0 {
        return nil
    }
    output := make(map[string]pipeline.StreamUpdate, len(input))
    for stage, update := range input {
        update.Lines = append([]pipeline.StreamLine(nil), update.Lines...)
        output[stage] = update
    }
    return output
}
```

`applyProgress` 在更新 `RunID` 后优先处理 stream 事件：

```go
if update.Event == pipeline.EventStageStream && update.Stream != nil {
    if update.RunID != "" {
        job.RunID = update.RunID
    }
    applyStreamUpdateLocked(job, update)
    job.mu.Unlock()
    s.notify()
    return
}
```

`applyStreamUpdateLocked` 负责按模式落内存：

```go
const maxStreamLines = 200

func applyStreamUpdateLocked(job *Job, update pipeline.RunProgress) {
    if job.StreamByStage == nil {
        job.StreamByStage = map[string]pipeline.StreamUpdate{}
    }
    stage := strings.TrimSpace(update.Stream.Stage)
    if stage == "" {
        stage = strings.TrimSpace(update.Stage)
    }
    if stage == "" {
        return
    }

    stream := *update.Stream
    stream.Stage = stage

    if stream.Mode == pipeline.StreamModeAppend {
        if job.streamLinesByStage == nil {
            job.streamLinesByStage = map[string][]pipeline.StreamLine{}
        }
        previous := job.StreamByStage[stage]
        stream.Truncated = stream.Truncated || previous.Truncated
        lineText := strings.TrimRight(stream.Delta, "\r\n")
        if stream.Source != "" || stream.Delta != "" {
            source := strings.TrimSpace(stream.Source)
            if source == "" {
                source = "stdout"
            }
            job.streamLinesByStage[stage] = append(job.streamLinesByStage[stage], pipeline.StreamLine{
                Source: source,
                Text:   lineText,
            })
        }
        if len(job.streamLinesByStage[stage]) > maxStreamLines {
            drop := len(job.streamLinesByStage[stage]) - maxStreamLines
            job.streamLinesByStage[stage] = append([]pipeline.StreamLine(nil), job.streamLinesByStage[stage][drop:]...)
            stream.Truncated = true
        }
        stream.Lines = append([]pipeline.StreamLine(nil), job.streamLinesByStage[stage]...)
        stream.Text = streamLinesText(stream.Lines)
    }

    job.StreamByStage[stage] = stream
}
```

注意：

- stream 事件不改变 `job.State`，不覆盖 `CurrentStage`，不更新 `Stages`，不写 DB。
- `StreamModeCumulative` 只保存最新 `Text`，因为每次都是完整预览快照。
- `StreamModeAppend` 必须在 scheduler 中聚合 `Lines`。TUI 不再维护第二套本地行缓冲，避免快照合并时出现双写、重复或丢行。
- job 结束时快照保留最后一版 stream，TUI 端负责在 DB reload 后根据 stage 状态决定继续显示还是替换为正式报告。
- `StreamByStage` 是内存态；程序重启后不恢复，这是预期行为。
- TUI 端 stream 生命周期：阶段 running 期间 stream 持续更新；阶段 done/failed 后，下一轮 DB reload 触发 `buildExecutionViewModel` 重建 `detailVM`，此时若 stage 状态非 running，TUI 不再合并该 stage 的 stream，改为显示正式报告/发现。详见 2.7 节。

### 7. TUI view model 合并预览与生命周期管理

#### 7a. executionViewModel 新增字段

```go
type executionViewModel struct {
    // ... 现有字段 ...
    StreamByStage map[string]pipeline.StreamUpdate
}
```

`buildExecutionViewModel()` 初始化 `StreamByStage: map[string]pipeline.StreamUpdate{}`，但不从 DB 填充。它只作为 TUI 合并 scheduler 内存快照的容器。

#### 7b. 追加模式缓冲区

对于 `StreamModeAppend`（B/C 阶段），TUI 不再维护独立的 `liveOutputLines` 环形缓冲，也不根据单条 `Delta` 自己补历史。追加模式的唯一事实来源是 scheduler 快照里的 `StreamByStage[stage].Lines`。这样即使 `NotifyCh()` 合并了多次通知，TUI 下一次 `Snapshot()` 也能拿到 scheduler 已聚合的最近 200 行，不会只剩最后一行。

对于 `StreamModeCumulative`（Codex D/E/F 阶段），TUI 直接使用 `StreamByStage[stage].Text`，无需额外缓冲。

#### 7c. 合并逻辑（含生命周期管理）

`detailMsg` 仍从 DB 构建基础 VM。TUI 收到 `schedulerJobsMsg` 后，如果当前选中 task 有 queued/running job，则把该 job 的 `StreamByStage` 合并到 `m.detailVM.StreamByStage`。TUI 只在 stage 状态为 running 时显示 stream，这是生命周期管理的核心；job 结束后若 DB reload 尚未返回，上一帧 stream 可以短暂留在页面，reload 后按非 running 状态清理。

新增 helper：

```go
func (m *app) mergeActiveStreamPreview() {
    changed := false

    // 先检查是否有需要清理的 stream：stage 不再是 running 状态
    for stage := range m.detailVM.StreamByStage {
        sv := stageForKey(m.detailVM.Stages, stage)
        if sv.Status != model.StageRunning {
            delete(m.detailVM.StreamByStage, stage)
            changed = true
        }
    }

    // 从 queued/running job 合并 stream；追加模式的 Lines 已由 scheduler 聚合完成。
    job, ok := m.activeJobForTask(m.selectedTaskID())
    if ok && len(job.StreamByStage) > 0 {
        if m.detailVM.StreamByStage == nil {
            m.detailVM.StreamByStage = map[string]pipeline.StreamUpdate{}
        }
        for stage, update := range job.StreamByStage {
            sv := stageForKey(m.detailVM.Stages, stage)
            // 只在阶段 running 时接受 stream 更新
            if sv.Status != model.StageRunning {
                continue
            }
            m.detailVM.StreamByStage[stage] = update
            changed = true
        }
    }
    if !changed {
        return
    }
    m.updateDetailContent(false)
}
```

生命周期规则：

- **阶段 running 且有 stream**：显示实时输出区域，内容来自 `StreamByStage`。
- **阶段变成 done/failed**：DB reload 返回后 `detailVM.Stages` 变为非 running，`mergeActiveStreamPreview` 删除旧 stream，`buildDetailContent` 转而渲染发现/报告区域。不要假设 `detailMsg` 和 `schedulerJobsMsg` 会在同一个 Update 周期内按固定顺序到达；为减少闪烁，在 DB 状态还没确认前保留上一帧 stream，确认后一次性切换正式结果。
- **job 完全消失**（scheduler 清理）：下一轮 DB reload 后 stage 状态非 running，stream 不再显示；如果 reload 尚未到达，则保留上一帧，避免主内容区闪空。
- **用户切换 stage**：重新计算目标 stage 状态，若非 running 则不显示 stream。
- **B/C 追加模式缓冲区**：只存在 scheduler 的 `streamLinesByStage` 内部字段。TUI 删除 `StreamByStage` key 即可，不需要额外清理本地行缓存。

调用点：

- `schedulerJobsMsg` 更新 `m.activeJobs` 后，调用 `m.mergeActiveStreamPreview()`。
- `detailMsg` 更新 `m.detailVM` 后，调用 `m.mergeActiveStreamPreview()`（DB reload 可能覆盖 VM，重新合并）。
- `applyLayout()` 后不需要重新合并，只需 `updateDetailContent(false)`。

### 8. 执行详情右侧面板重设计

#### 8a. 设计原则

右侧不要继续渲染为”当前状态一行 + 大段拼接文本”。新的右侧详情面板拆分为**三层结构**：

| 层 | 位置 | 内容 | 特征 |
|---|---:|---|---|
| 状态栏 | viewport 外（永远可见） | 当前阶段、状态、耗时、run ID、job ID、模式 | 1-2 行，不可滚动 |
| 主内容区 | viewport 内，上部 | 运行中 = 实时输出；完成后 = 发现列表 + 报告路径 | 随阶段状态切换 |
| 证据区 | viewport 内，下部 | 文档摘要、预检、清理、guidance deadline、日志尾部、路径警告、产物入口 | 始终存在，可滚动 |

**与原始设计的关键差异：**

- **砍掉”阶段轨迹”独立区域。** A-F 状态条在左侧面板的阶段列表中已有完整展示（`renderStageSection`），在宽屏顶部的 pipeline bar 中也有缩略展示，右侧不需要第三次重复。唯一独有信息是 guidance deadline 状态，归入证据区。
- **”交付摘要”不再作为独立区域。** 完成后主内容区直接展示发现列表（按严重度排序，Blocker/High 优先，最多 8 条）和报告路径，不需要”摘要”这个中间抽象层。失败时错误原因置顶。
- **B/C 阶段也有实时输出**（追加模式），不再只有 Codex 阶段。追加模式和累积模式共用”主内容区”。
- **辅助信息（文档/预检/清理）放证据区顶部**，按需滚动查看，不降级到底部。

#### 8b. 区域命名和职责（最终版）

| 区域 | 高度策略 | 内容 |
|---|---:|---|
| 状态栏 | viewport 外，固定 1-2 行 | 当前阶段字母、中文名称、状态图标、耗时、run ID、job ID、模式 |
| 主内容区 | 运行中优先，最少 8 行，最多 50% | **Codex 累积模式**: 流式文本（`Text`，wrap 后显示）、截断提示、item 状态指示；**B/C 追加模式**: 进程输出环形缓冲区（最后 N 行，tail -f 风格）、stderr 行标红；**完成后**: 错误原因（失败时）/ 发现列表（Blocker + High，最多 8 条）/ 报告路径 / 产物路径 |
| 证据区 | 填充剩余空间 | 文档摘要、预检结果、清理状态、guidance deadline events、artifact warning、路径警告、日志尾部 |

**状态栏不进入 viewport：** 现有 `renderDetailContext` 已渲染一行状态文本，保持其在 viewport 外的位置；内容需要补充当前 job ID 与模式（宽度不足时截断）。viewport 只管理主内容区和证据区。

**非 Codex 非 B/C 阶段（如 A 阶段）：** 主内容区不显示实时输出（因为 A 阶段无长时间进程且不产生 stream），直接显示阶段结果（若有）。证据区正常显示。

#### 8c. 布局和内容优先级

| 阶段状态 | 主内容区显示 | 证据区内容排序 |
|---|---|---|
| running + 有 stream（Codex 累积） | Codex 累积文本，wrap 后由上至下显示，新内容在底部 | 文档 → 预检 → 清理 → guidance → warning → log tail |
| running + 有 stream（B/C 追加） | 进程输出环形缓冲区，底部对齐，stderr 标红 | 同上 |
| running + 无 stream | “等待阶段输出...” 占位提示 | 同上 |
| done/failed + 有发现 | 错误原因（失败时置顶）+ Blocker/High 发现列表 + 报告路径 | 同上 |
| done + 无发现 | “本阶段无阻断/严重发现” + 报告路径 | 同上 |
| skipped/blocked | 阻塞原因 + “阶段未运行” | 同上 |

#### 8d. 视觉样式规则

- **Codex 累积文本（StreamModeCumulative）**：正常样式渲染，与当前 detail 文本一致。`Truncated=true` 时第一行显示 `...（预览已截断，仅显示尾部 64 KiB）`。
- **B/C stdout（StreamModeAppend, Source=”stdout”）**：muted 样式（灰色），表示机器日志。
- **B/C stderr（StreamModeAppend, Source=”stderr”）**：error 样式（红色），表示需要关注。
- **Done 标记**：运行中阶段在状态栏显示 `▶` 图标和耗时；`Done=true` 后不立即切换（状态栏的 stage status 仍为 running 直到 DB reload），但实时输出区底部可显示”进程已退出，等待阶段确认...” 的 muted 提示。

#### 8e. 宽屏执行页示意图（修正版）

```text
┌──────────────────────────────────────────────────────────────────────────────────────┐
│ p2r tui  执行详情                                     jobs 2/3  refresh 250ms        │
├──────────────────────────────────────────────────────────────────────────────────────┤
│ 流水线: [██░░] 2/3  排队: 1                                                          │
│ ▶ TASK-042  阶段D 测试有效性静态审查  running  12m18s                                 │
├───────────────────────────────┬──────────────────────────────────────────────────────┤
│ 任务 TASK-042                  │ D 测试有效性静态审查  ▶ 运行中  12m18s                │ ← 状态栏
│ 运行 20260511-151430          │ run 20260511-151430  initial  job-...039             │   (viewport外)
│ 状态 running                   │                                                      │
│                               │ 正在核对 api/user/login 的测试断言与真实路由映射，    │ ← 主内容区
│ 阶段                           │ 已确认 self_test_report 中的 3 个失败用例与实际端点  │   (Codex累积文本)
│ > D 测试有效性静态审查  run   │ 不一致。正在进一步检查 api/user/profile 与             │
│   E 静态验收审计        pend  │ api/order/list 的测试覆盖情况...                      │
│   F 标注员修复静态审查  pend  │                                                      │
│                               │ ── 运行证据 ──                                       │ ← 证据区
│ 参考运行                      │ 文档: 12 个，清单: $PROJECT/task_docs_manifest.json  │
│ 20260510-220011 clean         │ 预检: 正常                                           │
│ 20260509-184500 findings      │ 清理: 正常                                           │
│                               │ guidance 20m: pending  30m: pending                  │
│                               │ JSON-RPC notification turn/completed ...            │
│                               │ STDERR: ...                                          │
├───────────────────────────────┴──────────────────────────────────────────────────────┤
│ ↑↓ stage  tab focus  ctrl+r run  ctrl+x cancel  q quit                               │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

#### 8f. 窄屏执行页示意图（修正版，B/C 追加模式示例）

```text
┌──────────────────────────────────────────────┐
│ p2r tui  执行详情                            │
├──────────────────────────────────────────────┤
│ 流水线: [█░░] 1/3                            │
│ ▶ TASK-042  阶段B Docker运行时证据  running  │
├──────────────────────────────────────────────┤
│ 阶段: B Docker运行时证据  ▶ 运行中  3m42s    │ ← 状态栏 (viewport外)
│ run 20260511-151430  job-...039              │
├──────────────────────────────────────────────┤
│ [+] Running 3/3  Container p2r_test         │ ← 主内容区
│ ✔ Container p2r_db  Healthy                 │   (B/C追加模式,
│ ✔ Container p2r_app  Healthy                │    muted灰色)
│ ⚠ Container p2r_test  Starting...           │
│                                              │
│ [stderr] warning: UFW not available         │ ← stderr行(error红色)
├──────────────────────────────────────────────┤
│ ── 运行证据 ──                               │ ← 证据区
│ 文档: 12 个，已生成                          │
│ 预检: 正常                                   │
│ 清理: 正常（keep-runtime）                   │
│ ...                                          │
├──────────────────────────────────────────────┤
│ ↑↓ scroll  tab focus  q quit                 │
└──────────────────────────────────────────────┘
```

#### 8g. 宽屏执行页示意图（阶段完成后）

```text
┌──────────────────────────────────────────────────────────────────────────────────────┐
│ p2r tui  执行详情                                                                     │
├───────────────────────────────┬──────────────────────────────────────────────────────┤
│ 任务 TASK-042                  │ D 测试有效性静态审查  ✓ 完成  720s                    │ ← 状态栏
│ 运行 20260511-151430          │                                                      │
│ 状态 有发现                    │ [阻断] API端点 /api/user/login 不存在                │ ← 主内容区
│                               │   规则: endpoint-existence                           │   (发现列表)
│ 阶段                           │   证据: self_test_report 引用了不存在的路由           │
│ > D 测试有效性静态审查  done  │                                                      │
│   E 静态验收审计        pend  │ [严重] 测试用例引用了已删除的 /api/legacy             │
│   F 标注员修复静态审查  pend  │                                                      │
│                               │ 报告: QA_4_测试有效性报告_api端点真实性.md            │
│                               │                                                      │
│                               │ ── 运行证据 ──                                       │ ← 证据区
│                               │ 文档: 12 个  预检: 正常  清理: 正常                  │
│                               │ guidance 20m: sent  30m: not triggered               │
│                               │ JSON-RPC notification turn/completed ...            │
├───────────────────────────────┴──────────────────────────────────────────────────────┤
│ ↑↓ stage  tab focus  ctrl+r run  ctrl+x cancel  q quit                               │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

#### 8h. 实现拆分

- `renderDetailContext` 保持在 viewport 外，内容扩展为阶段、状态、耗时、run ID、active job ID、模式；宽度不足时仍用 `truncateDisplay` 收口。
- `buildDetailContent` 拆为两个 builder：

```go
func buildDetailContent(vm executionViewModel, selectedStage string, width, height int) string {
    stage := stageForKey(vm.Stages, selectedStage)
    primaryBudget := primaryContentBudget(stage, vm.StreamByStage[stage.Stage], height)
    primary := renderPrimaryContent(vm, stage, width, primaryBudget) // 实时输出 或 发现/报告
    evidence := renderEvidenceSection(vm, stage, width)              // 辅助信息 + guidance + log tail
    return joinNonEmptySections([]string{primary, evidence})
}
```

- `renderPrimaryContent` 内部按阶段状态分发：
  - 若 `stage.Status == running` 且 `stream, ok := vm.StreamByStage[stage.Stage]; ok`：
    - `stream.Mode == StreamModeCumulative` → 渲染累积文本（wrap，正常样式，截断提示）
    - `stream.Mode == StreamModeAppend` → 直接渲染 `stream.Lines`，stdout/p2r 用 muted 样式，stderr 用 error 样式，底部对齐
  - 若 `stage.Status == done/failed` → 渲染错误原因 + 发现列表 + 报告路径 + 产物路径
  - 若 `stage.Status == skipped/blocked` → 渲染阻塞原因
- `renderEvidenceSection` 内容排序：文档摘要 → 预检结果 → 清理状态 → guidance deadline events → artifact warning → 路径警告 → 日志尾部（`LogTailByStage`）。各小节之间空行分隔。
- `updateDetailContent` 调用 `buildDetailContent(m.detailVM, m.selectedStageKey, width, m.detail.Height)`；`tests/internal/tui` 与 `internal/tui/testhooks.go` 的 helper 签名同步增加 height 或使用合理默认值。
- `primaryContentBudget` 在 running+stream 时至少 8 行、最多 `height/2`；B/C 追加模式按预算取 `stream.Lines` 尾部，Codex 累积模式 wrap 后取尾部以配合 follow-tail。完成态发现列表最多 8 条，证据区继续在后方滚动。

`executionViewModel` 新增 `StreamByStage`，继续复用已有 `GuidanceEventsByStage`、`LogTailByStage`、`DocsSummary`、`PathWarnings` 等字段。无需新增 DB 字段；B/C 追加模式缓冲区放在 scheduler 内存态，不放在 TUI `app` 上。

### 9. 自动滚动策略

当前用户可以在详情 viewport 中滚动。为避免流式刷新抢走阅读位置，第一版实现简化的 followTail 机制：

在 `app` 中增加 `detailFollowTail bool` 字段：

- **设为 true 的时机**：进入执行页（切换到 panelExecution）、切换阶段（`moveStage`）、按 `End` 键（`GotoBottom`）、主内容区首次出现 stream。
- **设为 false 的时机**：用户手动向上滚动（`LineUp`/`PageUp`/`GotoTop`）。
- **恢复 true 的时机**：按 `LineDown`/`PageDown`/`GotoBottom` 滚到底部后。

`updateDetailContent` 行为：

- 若 `detailFollowTail` 为 true 且当前选中阶段正在 running 且有 stream 数据：设置内容后调用 `m.detail.GotoBottom()`。
- 若 `detailFollowTail` 为 false：只更新内容，保持当前滚动位置。

B/C 追加模式下，新行从底部出现时自动跟随底部；Codex 累积模式下，文本增长时自动跟随底部。用户一旦手动向上滚动查看历史输出，即停止自动跟随，不会抢走阅读焦点。

### 10. 流式预览测试

需要覆盖：

**Codex 累积模式：**
- pipeline：Codex delta 回调触发 `EventStageStream`，不触发 DB `PutStage`。
- scheduler：`EventStageStream` 更新 `JobSnapshot.StreamByStage`，不改变 job 状态和 current stage。
- TUI：收到带 `StreamByStage` 的 queued/running job 后，详情主内容区出现累积文本。
- TUI：DB reload 后 stage 非 running 时，stream 不再显示，切换为发现/报告。
- TUI：主内容区按状态切换：running+stream → 累积文本，done+findings → 发现列表，failed → 错误原因 + 发现。
- 宽度换行：长文本不会溢出详情宽度。
- 截断标记：`Truncated=true` 时显示省略提示。
- item/completed 后 Done=true，TUI 停止显示"仍在生成"指示。

**B/C 追加模式：**
- executor：新 `RunStreamingWithOutput` 接口对 stdout 和 stderr 每行触发回调；旧 `RunStreaming` 接口不存在。
- pipeline：stage B/C 将 `OnOutput` 回调转为 `EventStageStream`（Mode=Append）。
- scheduler：追加模式行缓冲区正确维护（上限 200 行，FIFO 淘汰），`JobSnapshot.StreamByStage[stage].Lines` 深拷贝完整尾部窗口。
- TUI：追加模式直接渲染 scheduler 快照中的 `Lines`，不维护第二套本地行缓冲。
- TUI：stdout 行以 muted 样式渲染，stderr 行以 error 样式渲染。
- TUI：进程退出后 Done=true，当前 stream 视图保留到 DB reload 确认阶段已结束，然后自然切换到正式结果。
- TUI：切换 stage 后只显示目标 stage 的 `StreamByStage` 内容，不残留旧 stage 输出。

**生命周期：**
- TUI：stream 从 queued/running job 合并到 `detailVM.StreamByStage`。
- TUI：DB reload 后 `mergeActiveStreamPreview` 清理非 running stage 的 stream。
- TUI：job 完成后 stream 不立即消失——保留直到 DB reload 确认 stage 状态变更。
- TUI：辅助信息（文档/预检/清理）始终在证据区顶部可见。

**自动滚动：**
- detailFollowTail 在进入执行页和切换阶段时重置为 true。
- 用户按 PageUp 后 followTail 变为 false，不再自动滚到底部。
- 用户按 End 后 followTail 恢复 true。

## 边界场景覆盖

| 场景 | 行为 |
|---|---|
| 多 delta -> item/completed | `item/completed` 后输出 1 条 aggregated 日志；TUI 持续显示累积预览 |
| 多 delta -> turn/completed，无 item/completed | `turn/completed` 前兜底输出 aggregated 日志；最终报告使用 delta 文本 |
| Codex 崩溃，无 turn/completed | `complete()` 在 `close(done)` 前输出残余 aggregated 日志；TUI 保留最后内存预览直到 job 终止刷新 |
| 只有 item/completed，无 delta | 不输出 aggregated 日志；TUI 没有流式预览但最终报告正常 |
| item/completed 后又收到 delta | 不重复 aggregated 日志；最终报告优先 completed text；late delta 可记录到 `deltas` 供诊断，但不重新触发生成中预览 |
| 多 item agentMessage | 每个 item 最多 1 条 aggregated 日志；TUI 按 stage 展示最新 item 的预览或后续扩展为 item 列表 |
| 中文/多字节文本 | `text_prefix` 按 rune 截断；TUI 使用完整 UTF-8 文本换行 |
| 大输出 | 正式 `deltas` 不截断；TUI preview 单 item 限制 64 KiB 并显示 `Truncated` |
| B 阶段 docker-compose 输出 | stdout 行通过 `EventStageStream`（Mode=Append）实时推送到 TUI，muted 样式渲染；stderr 行 error 样式渲染；日志文件写入不变 |
| C 阶段测试运行输出 | 同上，进程退出后发 Done=true，scheduler 缓冲区保留最后窗口直到 DB reload 后 TUI 切换正式结果 |
| stream 到达时 TUI 未在执行页 | scheduler 正常保存到 `JobSnapshot.StreamByStage`，TUI 切换到执行页后通过 `reloadDetail` / 下一次 `schedulerJobsMsg` 调用 `mergeActiveStreamPreview` 补上 |
| job 完成后 scheduler 保留最后一帧 stream | TUI 在 DB reload 确认 stage 非 running 后清理 stream；确认前可保留上一帧，避免主内容区闪空 |
| 用户切换 stage 后旧 stage 的 stream | 详情只渲染当前 stage 的 `StreamByStage`；旧 stage 输出不在右侧主内容区残留 |
| A 阶段（无长时间进程） | 不产生 stream 事件，主内容区直接显示阶段结果（若有），证据区正常显示 |

## 验收标准

**日志压缩：**
- Codex delta 日志从 N 条逐条元数据行压缩为每 item 1 条 aggregated 行。
- 日志中不出现 raw JSON params、raw delta 全文或完整报告正文。
- `Wait()` 返回后立即读取日志，也能看到最终兜底 aggregated 行。

**TUI 实时预览：**
- D/E/F 阶段运行中，TUI 执行详情页右侧主内容区能看到 Codex 正在生成的回复文本持续增长（累积模式，正常样式）。
- B/C 阶段运行中，TUI 执行详情页右侧主内容区能看到进程 stdout/stderr 实时输出（追加模式，stdout muted 样式，stderr error 样式）。
- 流式预览不写 DB、不污染 artifact、不影响最终 QA 报告生成。
- 阶段完成后，主内容区自然切换为发现列表 + 报告路径，stream 不残留。
- 证据区始终包含文档摘要、预检、清理、guidance deadline、日志尾部，辅助信息置顶。
- 右侧不再出现与左侧面板重复的 A-F 阶段轨迹区域。
- `detailFollowTail` 机制正确工作：运行中自动跟随底部，用户手动上滚后停止跟随。
- `go test ./internal/executor ./internal/pipeline ./tests/internal/executor ./tests/internal/pipeline ./tests/internal/scheduler ./tests/internal/tui` 通过；最终合并前建议再跑 `go test ./...`。
