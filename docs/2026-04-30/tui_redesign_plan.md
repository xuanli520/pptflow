# TUI 重设计计划

Date: 2026-04-30
Status: Ralph 修订版，待实现

## 1. 背景与目标

当前 TUI 已能列出项目、进入执行详情、触发重跑，但还没有达到“质检工作台”的标准。它既有明显的交互问题，也有证据流缺口：

1. **界面层级弱**：全白文字为主，状态、严重度、选中项、证据区域缺乏视觉区分。
2. **键盘分发混乱**：`up`/`down` 被顶层逻辑截获，table、阶段列表、ref run 列表、viewport 无法按焦点稳定响应。
3. **日志和详情滚动不可靠**：viewport 尺寸固定，内容在 `View()` 中临时生成，滚动位置容易被刷新打断。
4. **`q` 误退出**：搜索框输入普通字母时可能触发全局命令，输入和控制没有清晰边界。
5. **窄屏降级粗糙**：当前通过整列删除处理，容易丢失 failed/docs/cleanup 等关键质检信号。
6. **中文化不完整**：标题、表头、状态、提示、错误消息和阶段名称混用中英文。
7. **证据工作台能力不足**：docs、preflight、cleanup、artifact、unavailable-review 等信息没有作为一等信息展示。
8. **复检和重跑影响不清晰**：ref run 选择、补充文档、cleanup、keep-runtime、将被重生成的报告文件没有在确认前讲清楚。

目标不是只把界面“变好看”，而是让质检员在 TUI 内快速回答前三个问题：

- 什么失败了？
- 为什么失败？
- 证据在哪里？

## 2. Ralph 循环约束

本轮按 oh my codex Ralph 循环推进：

1. **读取契约**：对照 `docs/p2r_tui_qa_iteration_plan_2026-04-30.md`、`docs/MVP_FIX_PLAN.md` 和现有 `internal/tui/app.go`。
2. **切片实现**：先修 key/focus/state，再做 layout/render，再补中文化和颜色，最后补测试。
3. **验证回归**：每个切片至少跑 `go test ./...`，最终跑 `go vet ./...` 和 `go build ./...`。
4. **记录学习**：若实现中发现 DB、pipeline、taskdocs 契约不一致，回写到本计划或单独 regression note。

不得引入以下回退：

- 不删除 docs、preflight、cleanup 在 TUI 中的可见性。
- 不让 TUI 自动设置 PASS/REWORK/FAIL；TUI 只辅助人工判断。
- 不让搜索、重跑、复检选择互相抢键。
- 不在 `View()` 中执行 DB 查询、文件读取或昂贵计算。

## 3. 信息架构

TUI 保留两个主面板，但每个面板必须围绕证据流组织：

| 面板 | 目的 | 必须回答 |
|------|------|---------|
| 项目总览 | 快速筛选和排序项目 | 哪个任务风险最高？最新 run 状态如何？是否有 docs/cleanup/preflight 信号？ |
| 执行详情 | 分析一次 run | 哪个阶段失败？原因是什么？证据和日志在哪里？重跑会影响什么？ |

整体框架：

```text
┌─ p2r QA 工作台 ───────────────────────────────────────────┐
│ [项目总览]  [执行详情]              模式: 首次质检          │
├────────────────────────────────────────────────────────────┤
│ 面板内容区域                                                │
├────────────────────────────────────────────────────────────┤
│ Ctrl+C 退出  Tab/Shift+Tab 面板  Ctrl+R 重跑  Ctrl+M 模式   │
└────────────────────────────────────────────────────────────┘
```

## 4. 焦点模型与快捷键

### 4.1 焦点对象

TUI 必须显式维护焦点，而不是靠 `tab` 或当前面板隐式判断。

```go
type focusArea int

const (
    focusSearch focusArea = iota
    focusOverviewTable
    focusStageList
    focusRefRunList
    focusDetailViewport
)
```

面板和焦点的默认关系：

| 场景 | 默认焦点 |
|------|----------|
| 启动项目总览 | `focusSearch` |
| 搜索提交或按 `↓` | `focusOverviewTable` |
| Enter 进入执行详情 | `focusStageList` |
| 复检模式且打开 ref run 区 | `focusRefRunList` |
| 查看长日志/证据 | `focusDetailViewport` |

### 4.2 全局快捷键

| 功能 | 键位 | 说明 |
|------|------|------|
| 退出 | `Ctrl+C` | 主退出键，避免依赖可能受终端流控影响的 `Ctrl+Q` |
| 退出别名 | `Ctrl+Q` | 可选保留；不作为唯一退出方式 |
| 切换面板 | `Tab` / `Shift+Tab` | 前进/后退；后退不能和前进同逻辑 |
| 重跑 | `Ctrl+R` | 打开确认对话框 |
| 切换模式 | `Ctrl+M` | initial/recheck |
| 附加文档 | `Ctrl+A` | 显示 attach 命令或进入路径输入流程 |
| 取消/返回 | `Esc` | 关闭确认/返回总览/退出子焦点 |
| 确认 | `Enter` | 总览进入详情；确认框中确认 |

`q` 不再作为全局强制退出键。可选行为：仅当搜索框未聚焦、未处于输入/确认状态时，`q` 返回总览或退出。

### 4.3 局部按键矩阵

| 焦点 | 普通字符 | `↑↓` | `←→` | `PgUp/PgDn` | `Enter` |
|------|----------|------|------|-------------|---------|
| 搜索框 | 输入搜索内容 | `↓` 转到表格 | 光标移动 | 无 | 转到表格 |
| 总览表格 | 不输入 | 选择任务 | 无 | 翻页 | 进入执行详情 |
| 阶段列表 | 不输入 | 选择阶段 | 可切 detail/ref 焦点 | 无 | detail 焦点 |
| ref run 列表 | 不输入 | 选择参考 run | 返回阶段列表 | 翻页 | 确认参考 run |
| 详情 viewport | 不输入 | 小步滚动 | 返回阶段列表 | 翻页 | 无 |

### 4.4 按键分发顺序

1. `tea.WindowSizeMsg`、reload/tick/run 消息先更新 model。
2. 确认框优先处理 `y`/`Y`/`enter`/`n`/`N`/`esc`，其余键忽略。
3. 全局 `Ctrl+` 快捷键优先处理。
4. `Tab` / `Shift+Tab` 切换主面板并设置默认焦点。
5. `Esc` 根据上下文关闭确认、退出输入焦点、返回总览。
6. 剩余按键按 `focusArea` 分发给 search/table/stage/ref/viewport。

## 5. 视觉与颜色体系

颜色不能是唯一信息来源；每个关键状态都同时使用符号或文本。

| 元素 | 颜色 | 色号 | 文本/符号 |
|------|------|------|----------|
| Blocker 发现 | 红色 | `#FF4444` | `[阻断]` |
| High 发现 | 橙色 | `#FFAA00` | `[严重]` |
| Medium 发现 | 灰色 | `#888888` | `[中等]` |
| Low 发现 | 暗灰 | `#666666` | `[低]` |
| done 状态 | 绿色 | `#00CC66` | `✓` |
| failed 状态 | 红色 | `#FF4444` | `✗` |
| blocked 状态 | 黄色 | `#DDAA00` | `⊘` |
| running 状态 | 蓝色 | `#4488FF` | `▶` |
| pending 状态 | 暗灰 | `#666666` | `○` |
| skipped 状态 | 暗灰 | `#666666` | `-` |
| 标题/面板名 | 青色加粗 | `#00DDDD` | 文本 |
| 面板边框 | 暗灰色 | `#555555` | 边框 |
| 选中行 | 蓝底白字 | terminal safe | `>` 前缀 |
| 提示/快捷键栏 | 灰色 | `#888888` | 文本 |

窄屏或不支持颜色的终端仍必须通过 `✓/✗/⊘/▶/○/-`、`[阻断]`、`[严重]` 等文本表达状态。

## 6. 响应式布局

布局计算必须使用终端实际宽高，扣除标题、边框、padding、消息栏、footer，并使用 `lipgloss.Width` 或等价能力处理中文宽字符。

### 6.1 断点

| 终端宽度 | 布局 |
|----------|------|
| `>= 120` | 执行详情左右分栏：阶段/引用 run 35%，详情 65% |
| `90-119` | 左右分栏保留，但左侧压缩为阶段摘要，右侧优先显示原因/发现/证据 |
| `< 90` | 上下布局：阶段列表在上，详情 viewport 在下 |
| `< 72` | 最小摘要模式：隐藏低优先级列，详情区域只显示状态、原因、证据路径、提示可滚动 |

### 6.2 高度预算

```go
chromeHeight := titleHeight + tabsHeight + messageHeight + footerHeight + borderHeight
contentHeight := max(6, m.height-chromeHeight)
```

table 和 viewport 的高度来自 `contentHeight`，不能写死 `New(80, 10)`。

### 6.3 执行详情布局

宽屏：

```text
┌──────────────────────────┬──────────────────────────────────────┐
│ 阶段 / 复检参考             │ 阶段详情 / 发现 / 日志 / 证据           │
│ ✓ A 结构与规则检查          │ 阶段 B - Docker运行时证据               │
│ ✗ B Docker运行时证据        │ 状态: 失败   耗时: 1200ms               │
│ ⊘ C 测试运行时证据          │ 原因: docker compose up failed          │
│ ▶ D 测试有效性静态审查      │                                      │
│ - E 静态验收审计            │ 本阶段发现:                            │
│ - F 标注员修复静态审查      │ [阻断] readme-misalignment             │
│                            │ 证据: acceptance.json:15               │
│ 参考 run:                  │                                      │
│ > run-20260430-120000      │ 证据入口:                              │
│   run-20260429-110000      │ artifacts / preflight / cleanup / docs │
└──────────────────────────┴──────────────────────────────────────┘
```

窄屏：

```text
┌────────────────────────────────────────────────────────────┐
│ 阶段: ✓A ✗B ⊘C ▶D -E -F                                    │
├────────────────────────────────────────────────────────────┤
│ 阶段 B - Docker运行时证据                                   │
│ 状态: 失败  原因: docker compose up failed                  │
│ 证据: artifacts / preflight / cleanup / docs                │
│ ... 可滚动详情 ...                                           │
└────────────────────────────────────────────────────────────┘
```

## 7. 项目总览面板

### 7.1 必备列

总览表格必须优先保留质检判断所需信号。列的显示通过优先级收缩，不允许简单按宽度整组删除。

| 优先级 | 列名 | 中文标题 | 说明 |
|--------|------|----------|------|
| P0 | `task_id` | 任务ID | 永远保留，可省略中部 |
| P0 | `run_status` | 状态 | 中文状态，必要时缩写 |
| P0 | `failed_stage` | 失败 | 首个 failed/blocked 阶段 |
| P0 | `blocker` | 阻断 | 数字 |
| P0 | `high` | 严重 | 数字 |
| P1 | `manual_verdict` | 判定 | 人工判定状态 |
| P1 | `docs` | 文档 | 补充文档数量/状态 |
| P1 | `cleanup` | 清理 | cleanup 结果 |
| P2 | `batch` | 批次 | 宽度不足时隐藏 |
| P2 | `last_run` | 最后运行 | 宽度不足时缩短 |
| P2 | `mode` | 模式 | initial/recheck/static-only/runtime |

### 7.2 列宽策略

| 列名 | 宽屏 | 中屏 | 窄屏 | 极窄屏 |
|------|------|------|------|--------|
| task_id | 24 | 22 | 18 | 14 |
| run_status | 12 | 10 | 8 | 6 |
| failed_stage | 8 | 6 | 5 | 4 |
| blocker | 6 | 5 | 4 | 3 |
| high | 6 | 5 | 4 | 3 |
| manual_verdict | 8 | 6 | 0 | 0 |
| docs | 6 | 5 | 4 | 0 |
| cleanup | 10 | 8 | 0 | 0 |
| batch | 12 | 8 | 0 | 0 |
| last_run | 16 | 12 | 0 | 0 |
| mode | 10 | 0 | 0 | 0 |

宽度为 `0` 表示该断点隐藏。隐藏前必须确认 P0 列仍完整存在。

### 7.3 搜索和刷新稳定性

搜索应同时匹配 raw 值和中文显示值：

- task_id、batch、run_status、manual_verdict
- 中文状态：通过、有发现、运行中、崩溃、返工等
- 阶段名：A/B/C/D/E/F 和中文阶段名

后台 tick 刷新 rows 后必须按 `task_id` 保持选中项稳定。若选中任务被过滤掉，则选择过滤结果中的第一行，并给出短消息。

## 8. 执行详情面板

执行详情由 view model 驱动，不在 `View()` 中直接查 DB 或读文件。

```go
type executionViewModel struct {
    TaskID         string
    Run           model.RunRecord
    Stages        []model.StageRecord
    Findings      []model.Finding
    RefRuns       []model.RunRecord
    DocsSummary   docsSummary
    PreflightPath string
    PreflightText string
    CleanupPath   string
    CleanupStatus string
    ArtifactRoot  string
    SelfTestState string
    DetailContent string
}
```

### 8.1 阶段列表

阶段列表必须显示：

- 阶段字母
- 中文阶段名
- 状态符号
- 耗时
- 失败/blocked 原因摘要

阶段数量不能硬编码为 6 后直接索引。允许 partial historical runs，但 UI 应补齐 A-F 的占位行，并清楚标注 `未记录` 或 `已跳过`。

### 8.2 阶段详情

选中阶段右侧必须显示：

1. 阶段标题：`阶段 B - Docker运行时证据`
2. 状态、耗时、blocked_by
3. 原因：自动换行，不截掉核心错误
4. 本阶段发现：至少 Blocker/High 优先，Medium/Low 可折叠或后置
5. 证据路径：artifact、log、report、source_path
6. 日志尾部：遵守 `cfg.TUI.LogMaxLines`，但允许 viewport 滚动
7. unavailable-review artifact：Codex 不可用或静态审查不可运行时也要作为证据显示

### 8.3 docs / preflight / cleanup

这三类信息不能只通过底部快捷键提示存在，必须在详情区域可见：

| 信息 | 展示要求 |
|------|----------|
| docs | 文档数量、manifest 路径、跳过/二进制/超限原因摘要 |
| preflight | preflight 路径、关键 unavailable/blocked 原因 |
| cleanup | cleanup_summary 路径、状态、是否 keep-runtime、手动清理命令 |

若文件不存在，显示 `未生成`，而不是空白。

## 9. 重跑与复检

### 9.1 模式

| 模式 | 中文 | 要求 |
|------|------|------|
| `initial` | 首次质检 | 不需要 ref run |
| `recheck` | 打回重检 | 必须选择一个已完成 ref run |

复检模式下必须显示 ref run 列表，并排除当前 running run。`↑↓` 选择 ref run 只在 `focusRefRunList` 生效。

### 9.2 affected stages

默认重跑范围沿用 pipeline 规则，但 UI 必须明确展示：

| 当前阶段 | 默认重跑 |
|----------|----------|
| A | A, F |
| B | B, C, F |
| C | C, F |
| D | D, F |
| E | E, F |
| F | F |

后续可增加“运行时链”选项，但不得让 A 默认隐式触发 B/C。

### 9.3 重跑确认框

确认框必须包含：

- 任务 ID
- 模式：首次质检/打回重检
- ref run：复检模式必填
- 受影响阶段
- 会包含的补充文档数量和 manifest 路径
- preflight 将重新生成或复用的说明
- cleanup 是否会运行
- `--keep-runtime` 是否启用
- 将被重生成的报告/产物类型

示例：

```text
确认重新运行 TASK-20260327-6A5EE0？
模式: 打回重检
参考运行: run-20260429-110000
阶段: B, C, F
补充文档: 3 个，manifest: .../manifest.json
清理: 运行后清理 p2r 管理的 Docker 资源
keep-runtime: 否
将更新: runtime evidence, run_tests evidence, annotator repair report

Enter/y 确认，Esc/n 取消
```

## 10. 中文化对照

### 10.1 标题和提示

| 英文/原文 | 中文 |
|-----------|------|
| p2r QA CLI | p2r QA 工作台 |
| filter task id, batch, or status | 搜索任务ID、批次、状态或阶段... |
| Search: | 搜索: |
| Task: | 任务: |
| Run: | 运行: |
| Mode: | 模式: |
| Ref run: | 参考运行: |
| Ref runs: | 参考运行列表: |
| Stages: | 阶段: |
| Selected stage | 选中阶段 |
| Status: | 状态: |
| Duration: | 耗时: |
| Reason: | 原因: |
| Log: | 日志: |
| Artifacts: | 产物: |
| Stage findings: | 本阶段发现: |
| Findings: | 总发现: |
| Docs: | 文档: |
| Manifest: | 文档清单: |
| Preflight: | 预检: |
| Cleanup: | 清理: |
| Attach docs with: | 附加文档命令: |
| No indexed project selected. | 未选择已索引的项目 |
| Run `p2r scan --path <projects-qa>` first. | 请先执行 `p2r scan --path <projects-qa>` |
| No run yet. Press r to start a pipeline run. | 暂无运行记录，按 Ctrl+R 启动流水线 |
| running pipeline... | 流水线运行中... |
| rerun cancelled | 已取消重跑 |
| recheck mode requires selecting a ref run | 打回重检模式需要选择一个参考运行 |
| No row stored for this run. | 本次运行未记录该阶段 |
| No Blocker/High findings | 无阻断/严重发现 |

### 10.2 状态

| 原值 | 中文 |
|------|------|
| completed_clean | 通过 |
| completed_with_findings | 有发现 |
| running | 运行中 |
| aborted | 已中止 |
| crashed | 崩溃 |
| done | 完成 |
| failed | 失败 |
| blocked | 已阻塞 |
| pending | 等待中 |
| skipped | 已跳过 |
| unset | 未判定 |
| pass | 通过 |
| rework | 返工 |
| fail | 不通过 |
| none | 未生成 |
| unknown | 未知 |
| ok | 正常 |

### 10.3 严重度

| 原值 | 中文 |
|------|------|
| Blocker | 阻断 |
| High | 严重 |
| Medium | 中等 |
| Low | 低 |

### 10.4 阶段名称

| 阶段 | 英文 | 中文 |
|------|------|------|
| A | structure and rules check | 结构与规则检查 |
| B | Docker runtime evidence | Docker运行时证据 |
| C | run_tests runtime evidence | 测试运行时证据 |
| D | tests effectiveness static review | 测试有效性静态审查 |
| E | static acceptance audit | 静态验收审计 |
| F | annotator repair static review | 标注员修复静态审查 |
| unknown | unknown | 未知阶段 |

### 10.5 底部状态栏

底部状态栏必须根据焦点动态变化，不能展示当前不可用的命令。

| 场景 | 状态栏 |
|------|--------|
| 总览搜索 | `Ctrl+C 退出  Tab 执行详情  ↓ 表格  Ctrl+R 重跑  Ctrl+M 模式` |
| 总览表格 | `↑↓ 选择  Enter 执行详情  / 搜索  Ctrl+R 重跑  Ctrl+C 退出` |
| 阶段列表 | `↑↓ 阶段  Enter 详情  Ctrl+R 重跑  Ctrl+A 文档  Ctrl+M 模式` |
| ref run 列表 | `↑↓ 参考运行  Enter 选择  Esc 返回阶段  Ctrl+R 重跑` |
| 详情滚动 | `↑↓ 滚动  PgUp/PgDn 翻页  Esc 返回阶段  Ctrl+A 文档` |
| 确认框 | `Enter/y 确认  Esc/n 取消` |

## 11. 实现拆分

不要把所有逻辑继续堆在 `internal/tui/app.go`。建议拆分如下：

| 文件 | 责任 |
|------|------|
| `internal/tui/app.go` | Bubble Tea model、Update 主流程、commands |
| `internal/tui/keymap.go` | key/focus 分发、快捷键说明 |
| `internal/tui/layout.go` | 响应式尺寸、表格列策略、断点 |
| `internal/tui/localize.go` | 状态、阶段、严重度、清理状态中文化 |
| `internal/tui/render.go` | overview/execution/detail/footer 渲染 |
| `internal/tui/viewmodel.go` | 从 DB/artifact/taskdocs 生成轻量 view model |
| `tests/internal/tui/*_test.go` | model/key/layout/localize/render 单元测试 |

### 11.1 状态更新原则

- `View()` 只渲染已有 model，不查 DB、不读文件。
- reload command 负责读取 projects、runs、stages、findings、docs/preflight/cleanup 摘要。
- detail content 变化时才调用 `viewport.SetContent`，避免每次 tick 重置滚动。
- 记录 `selectedTaskID`、`selectedStageKey`、`selectedRefRunID`，刷新后按 ID 恢复选择。
- 所有文件读取错误以中文消息进入 detail，不静默吞掉。

### 11.2 辅助函数

需要新增或完善：

```go
func localizeRunStatus(status string) string
func localizeStageStatus(status string) string
func localizeManualVerdict(verdict string) string
func localizeSeverity(severity string) string
func localizeStageName(stage, name string) string
func localizeCleanupStatus(status string) string
func stageStatusIcon(status string) (string, lipgloss.Color)
func severityStyle(severity string) lipgloss.Style
func truncateDisplay(value string, width int) string
func buildOverviewColumns(width int) []table.Column
func buildExecutionViewModel(...) executionViewModel
```

`truncateDisplay` 必须按显示宽度截断，不能按 byte 长度截断中文。

## 12. 自动化测试计划

当前 TUI 测试覆盖不足。本轮必须补以下测试：

### 12.1 Key/focus 测试

- 搜索框输入 `q` 不退出。
- `Ctrl+C` 退出。
- `Ctrl+R` 打开确认框。
- 确认框中 `Esc/n` 取消，`Enter/y` 确认。
- 总览表格 `↑↓` 移动选择。
- 执行详情阶段列表 `↑↓` 切换阶段。
- 复检 ref run 焦点中 `↑↓` 切换参考 run。
- detail viewport 焦点中 `PgUp/PgDn` 滚动。
- `Shift+Tab` 是后退，不等同于 `Tab`。

### 12.2 Layout/render 测试

- 宽屏 `>=120` 使用左右分栏。
- `90-119` 保留核心列，隐藏低优先级列。
- `<90` 执行详情转上下布局。
- 极窄屏仍保留 task/status/failed/blocker/high。
- 中文宽字符截断不会破坏布局。
- footer 根据焦点显示不同提示。

### 12.3 Localization 测试

- run status、stage status、manual verdict、severity 全部中文化。
- F 阶段显示为 `标注员修复静态审查`。
- unknown/none/error 状态有中文 fallback。

### 12.4 View model 测试

- 旧 DB row 缺 stage name 时能 fallback。
- partial run 不 panic，缺失阶段显示 `未记录`。
- docs manifest 缺失时显示 `未生成`。
- cleanup_summary 缺失/非法 JSON 时显示中文状态。
- unavailable-review artifact 被列为证据。
- 后台刷新后按 task_id 保持选中行。

## 13. 手工验收清单

1. `go test ./...` 通过。
2. `go vet ./...` 通过。
3. `go build ./...` 通过。
4. 启动 TUI，总览搜索框输入 `q` 不退出。
5. `Ctrl+C` 可退出，`Ctrl+Q` 如保留则可退出。
6. 总览表格 `↑↓` 正常移动，刷新后选中 task 不漂移。
7. `Enter` 从总览进入执行详情。
8. 执行详情阶段列表 `↑↓` 切换阶段，右侧详情实时更新。
9. `PgUp/PgDn` 在 detail viewport 中滚动，后台刷新不强制回顶。
10. 复检模式下可选择 ref run，未选择时禁止重跑。
11. 重跑确认框展示 task、mode、ref run、affected stages、docs、cleanup、keep-runtime、将更新产物。
12. docs/preflight/cleanup/artifacts 在执行详情可见；缺失时显示 `未生成`。
13. 阶段状态颜色和符号正确。
14. Blocker/High 发现颜色和中文标签正确。
15. 宽屏 `>=120`、中屏 `~100`、窄屏 `~80`、极窄 `~70` 布局不重叠。
16. 三个真实 QA 项目 smoke：scan、run、打开 TUI、查看失败原因和证据路径，不需要先打开 raw JSON。

## 14. 实施顺序

1. **Iteration 1：状态与焦点**
   - 增加 focus model。
   - 重写 key dispatch。
   - 补 key/focus 单元测试。

2. **Iteration 2：View model**
   - 从 `View()` 移除 DB 查询和文件读取。
   - 增加 execution view model。
   - 保障刷新后选择稳定。

3. **Iteration 3：布局和表格**
   - 增加断点和高度预算。
   - 实现优先级列策略。
   - 修复中文显示宽度截断。

4. **Iteration 4：执行详情证据区**
   - 阶段列表、详情 viewport、findings、logs、artifacts。
   - docs/preflight/cleanup 作为一等信息展示。

5. **Iteration 5：中文化和视觉系统**
   - localize helpers。
   - 状态符号、严重度样式、动态 footer。

6. **Iteration 6：复检与重跑确认**
   - ref run 焦点。
   - 完整确认框。
   - RunOptions 传递校验。

7. **Iteration 7：最终 smoke 和回归记录**
   - `go test ./... && go vet ./... && go build ./...`
   - 三个真实 QA 项目 smoke。
   - 写 regression note。

## 15. Definition of Done

本次 TUI 重设计完成的标准：

- TUI 既美观，也能作为 QA 工作台使用。
- 质检员无需打开 raw JSON，就能完成第一层失败定位。
- 普通输入和控制快捷键不再冲突。
- 宽窄终端都有可用布局。
- docs/preflight/cleanup/artifacts/ref run/rerun impact 都清楚可见。
- 中文化覆盖主流程和错误态。
- 单元测试覆盖 key/focus/layout/localize/viewmodel 的核心行为。
- `go test ./...`、`go vet ./...`、`go build ./...` 全部通过。
