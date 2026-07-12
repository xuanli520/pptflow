# Harbor Flow TUI 中文化与人性化改造方案

> **实施跟踪（2026-07-11）：** 逐项代码位置与测试证据见 [`TUI_IMPLEMENTATION_AUDIT.md`](./TUI_IMPLEMENTATION_AUDIT.md)，用户键位说明见 [`TUI_USAGE.md`](./TUI_USAGE.md)。

> 基于对 Harbor Flow 现有 TUI 源码、p2r_tui 参考项目源码、Bubble Tea 生态最佳实践的全面分析
>
> 分析深度：逐行阅读 `model.go`（2924 行）、`p2r_tui/internal/tui/*.go`（28 文件，~9700 行）

---

## 1. 现状分析

### 1.1 架构概览

当前 TUI 实现在 **`internal/tui/model.go`**（2924 行）中，采用**单文件巨型扁平模型**。整个代码库的 TUI 部分仅 4 个文件：

| 文件 | 行数 | 职责 |
|------|------|------|
| `internal/tui/model.go` | 2924 | 全部：model struct、6 个视图渲染、所有键位处理、表单输入、数据流 |
| `internal/tui/run.go` | 85 | TUI 入口：工作区检测 → 5 种启动模式分发 |
| `internal/tui/styles.go` | 17 | 仅 6 个 lipgloss Style 变量 |
| `cmd/tui.go` | 30 | Cobra 命令注册（调用 `tui.Run()`） |

#### Model Struct 全貌（`model.go:94-123`）

```go
type model struct {
    ctx    context.Context
    cancel context.CancelFunc
    runner *app.Runner         // 后端工作流引擎（可能为 nil，如快照模式）
    opts   app.RunnerOptions   // 80+ 配置字段（表单直接绑定到此）

    width  int
    height int
    view   viewMode            // 6 种视图：start/overview/gate/nodeDetail/logs/done

    events   []domain.RunnerEvent          // 工作流事件流
    nodes    map[string]domain.RunnerEvent  // 按节点 ID 索引的最新事件
    summary  domain.RunSummary             // 运行摘要
    err      error                          // 当前错误（与 notice 竞争显示）
    notice   string                         // 当前通知
    done     bool                           // 运行是否完成
    readOnly bool                           // 是否只读快照模式

    activeGate       *domain.GateRequest  // 当前活跃的审查关卡
    gateNotes        string               // 审查备注（手写输入累加）
    gateEditingNote  bool                 // 是否在编辑备注模式
    editedFiles      map[string]string    // 编辑过的文件追踪
    selectedNode     string               // 当前选中的节点 ID
    selectedArtifact int                  // 当前选中的工件索引
    selectedLogFile  int                  // 当前日志文件索引
    logFileScroll    int                  // 日志滚动偏移
    logTail          bool                 // 是否自动跟踪日志尾部
    startMode        startMode            // 运行模式：existing task / generate
    startField       startField           // 表单当前焦点字段（39 个枚举值之一）
}
```

#### 自定义消息类型（`model.go:125-150`）

```go
type runnerEventMsg domain.RunnerEvent           // Runner 发出事件 → TUI 更新
type runnerDoneMsg struct { ... }                 // Runner 完成 → TUI 显示完成页
type workspaceRefreshMsg struct { ... }           // 轮询刷新（1 秒间隔）
type editorDoneMsg struct { ... }                 // 外部编辑器完成 → 追踪变更
type gateDecisionWrittenMsg struct { ... }        // Gate 决策写入磁盘 → 标记已决策
```

#### 39 个 StartField 枚举（`model.go:47-92`）

```go
startFieldMode, startFieldTaskDir, startFieldRepoURL, startFieldCommit,
startFieldWorkspace, startFieldTaskOutput, startFieldTestsAnalysis,
startFieldQwenResult, startFieldOpusResult, startFieldQwenScreenshot,
startFieldOpusScreenshot, startFieldQualityCheck, startFieldQualityAgent,
startFieldSimilarityCheck, startFieldSimilarityGitHub, startFieldSimilarityThreshold,
startFieldHistoryDirs, startFieldTB3Dirs, startFieldOutput, startFieldVerifyDocker,
startFieldRunHarbor, startFieldHarborAgent, startFieldQwenModel, startFieldOpusModel,
startFieldQwenHarborBaseURL, startFieldOpusHarborBaseURL, startFieldHarborTimeout,
startFieldHarborSetupTimeout, startFieldHarborPreflight, startFieldHarborConcurrency,
startFieldHarborAttempts, startFieldHarborInfraRetries, startFieldPackage,
startFieldTaskName, startFieldCodeLang, startFieldTaskType, startFieldApplication,
startFieldAHT, startFieldDescription, startFieldZeroToOne, startFieldCodexModel,
startFieldCodexReasoning, startFieldCodexPath, startFieldAgentTimeout
```

**关键问题：** 39 个字段全部平铺在一个列表中，通过 `m.activeStartFields()` 方法遍历。用户必须用 Tab 逐个穿越所有字段才能到达 Enter 提交。

#### 手写文本输入模式（`model.go:1173-1252`）

表单的"文本输入"是通过**巨大的 switch 语句做字符串拼接**实现的。每个按键直接追加到对应的 `opts.*` 字段：

```go
// model.go:1173 - appendStartInput (72 行 switch)
func (m *model) appendStartInput(value string) {
    switch m.startField {
    case startFieldTaskDir:
        m.opts.TaskDir += value        // 简单字符串拼接！无光标概念
    case startFieldRepoURL:
        m.opts.RepoURL += value
    case startFieldHarborTimeout:
        m.setStartInt(startFieldHarborTimeout,
            currentStartFieldInput(m.opts, startFieldHarborTimeout) + value)
    // ... 共 35 个 case 分支
    }
}

// 删除：trimLastRune，仅支持逐字符退格
func (m *model) backspaceStartInput() {
    m.opts.TaskDir = trimLastRune(m.opts.TaskDir)
    // ... 同样 35 个 case
}
```

**核心缺陷：** 这种模式没有光标位置、无法插入编辑、不支持 IME（中文输入法完全不可用）、`Space` 键在文本字段会追加空格字符。

#### 视图分发（`model.go:388-408`）

```go
func (m model) View() string {
    if m.width == 0 { return "Starting Harbor Factory...\n" }
    var body string
    switch m.view {
    case viewStart:      body = m.startView()
    case viewGate:       body = m.gateView()
    case viewNodeDetail: body = m.nodeDetailView()
    case viewLogs:       body = m.logsView()
    case viewDone:       body = m.doneView()
    default:             body = m.overview()
    }
    return lipgloss.JoinVertical(lipgloss.Left, m.header(), body, m.footer())
}
```

所有 6 个视图函数都在同一个文件中（`startView`, `overview`, `gateView`, `nodeDetailView`, `logsView`, `doneView`），总计约 1500 行的视图渲染逻辑。

#### 键位处理（`model.go:242-385`）

Update 方法内联了所有键位处理，无键位抽象层：

```go
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    switch msg := msg.(type) {
    case tea.WindowSizeMsg: /* 仅存储 width/height */
    case tea.KeyMsg:
        if m.view == viewStart   { return m.updateStartKey(msg) }
        if m.view == viewGate    { return m.updateGateKey(msg) }
        if m.view == viewLogs    { return m.updateLogsKey(msg) }
        // 其余视图使用内联 switch msg.String():
        switch msg.String() {
        case "q", "ctrl+c": /* 退出 */
        case "tab":           /* 7 个分支的 Tab 行为 */
        case "1":             /* 跳到 Overview */
        case "2":             /* 跳到 Gate 或 NodeDetail（二义性） */
        // ...
        }
    case runnerEventMsg:        /* 工作流事件 */
    case editorDoneMsg:         /* 编辑器完成 */
    case gateDecisionWrittenMsg: /* Gate 决策写入 */
    case runnerDoneMsg:          /* 工作流完成 */
    case workspaceRefreshMsg:    /* 轮询刷新 */
    }
}
```

#### 样式系统（`styles.go:1-17`）

```go
var (
    titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))   // 标题
    subtleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))              // 次要文字/footer
    sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))   // 小节标题
    passStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))               // 成功
    warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))              // 警告/运行中
    failStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))              // 失败
    panelStyle   = lipgloss.NewStyle().
                    Border(lipgloss.NormalBorder()).
                    BorderForeground(lipgloss.Color("240")).
                    Padding(1, 2)
)
```

仅 6 个颜色 + 1 个面板样式。无主题抽象（无 Theme struct），无选中样式，无高亮焦点样式。

#### Footer 系统（`model.go:2056-2061`）

```go
func (m model) footer() string {
    if m.readOnly {
        return subtleStyle.Render(
            "[1] Overview  [2] Gate/Node  [3] Logs  [4] Done  [d] Detail  [q] Quit  (read-only)")
    }
    return subtleStyle.Render(
        "[1] Overview  [2] Gate/Node  [3] Logs  [4] Done  [d] Detail  [x] Cancel model  [q] Quit")
}
```

**仅两个变体**（只读/活跃），不随当前视图变化。无论用户在 Overview、Gate 还是 Logs 视图，footer 始终显示相同的文本。

#### 状态图标（`model.go:2063-2076`）

```go
func statusIcon(status string) string {
    switch status {
    case "succeeded", string(domain.CheckPass):   return passStyle.Render("OK")
    case "failed", "canceled", string(domain.CheckFail): return failStyle.Render("!!")
    case string(domain.CheckWarn):                 return warnStyle.Render("!!")  // 注意：与 fail 共用 "!!"
    case "running":                                return warnStyle.Render("..")
    default:                                       return subtleStyle.Render("--")
    }
}
```

文本图标（"OK", "!!", "..", "--"），无 Unicode 符号，`warn` 和 `fail` 都显示 "!!"（仅颜色不同）。

#### 运行入口（`run.go:22-53`）

5 种启动路径，基于工作区状态自动选择：

```
无 workspace 且无 task → initialStartModel（配置表单）
workspace 被其他进程持有 → initialWorkspaceModel（只读快照）
workspace 有 run_options.json → 恢复 initialModel
workspace 有 state.json → 快照 initialWorkspaceModel
有 --task 或 --generate → 直接 initialModel
```

### 1.2 已识别的 25 个痛点

#### P0 - 数据安全与基本可用性
| # | 位置 | 问题 | 影响 |
|---|------|------|------|
| 18 | `updateGateKey` (model.go) | a/r/v 单键即执行，**零确认**——误触直接批准/拒绝 Gate | 数据安全 |
| 24 | View() (model.go:388) | **零进度指示器**——长时间 Harbor 运行看起来像程序冻结 | 用户焦虑 |
| 17 | `gateEditingNote` (model.go:116) | Notes 编辑无光标、无 IME、无多行——逐字符累加字符串 | 无法输入中文 |
| 8 | `appendStartInput` (model.go:1173-1239) | 手写文本输入，`Space` 在文本字段追加空格，布尔字段切换 | 混淆 |
| 20 | 全文 | **零组件复用**——所有 UI（列表/表单/滚动/选择）手写字符串拼接 | 维护噩梦 |

#### P1 - 用户体验核心
| # | 位置 | 问题 | 影响 |
|---|------|------|------|
| 4 | `model.go:316-323` | `x` 键的语义是"取消模型阶段"，但对非 Qwen/Opus 节点静默失败 | 误导 |
| 7 | `activeStartFields()` | 39 字段平铺遍历，无分组跳转 | 效率低下 |
| 1 | `model.go:262-279` | Tab 在 5 个视图中行为各有不同，用户需记忆 | 困惑 |
| 2 | `model.go:294-298` | 键 `4`（Done）在运行完成前按下无反应 | 沮丧 |
| 5 | `model.go:302-306` | 键 `g`（Gate）在无活跃 Gate 时静默忽略 | 迷惑 |
| 21 | `CancelNode()` | 取消模型仅 Qwen/Opus 有效，但 footer 始终显示 `[x] Cancel model` | 欺骗 |

#### P2 - 工作流效率
| # | 位置 | 问题 |
|---|------|------|
| 3 | `model.go:324-337` | `e`（编辑工件）不在主 footer 中 |
| 6 | `updateGateKey` | `v`（修订）在非 Final/Result Review 时静默禁用 |
| 12 | `overview()` | 节点列表超视口时无滚动指示器 |
| 14 | `model.go:2056-2061` | 只读模式仅靠灰色 `(read-only)` 标记 |
| 19 | `gateView()` | 只读/活跃 Gate 操作列表仅有无操作键区分 |
| 15 | `model.go:107-108` | `err` 和 `notice` 共享语义槽位，互相覆盖 |

#### P3 - 细节问题
| # | 位置 | 问题 |
|---|------|------|
| 9 | `appendStartInput` | 仅 ctrl+u 清空，无其他编辑快捷键 |
| 10 | `run.go:50` | 无 `tea.WithMouseCellMotion()` |
| 11 | 全局 | 无路径补全 |
| 13 | `readPreview()` | 截断条件 off-by-one |
| 16 | `renderStartField()` | 数字字段无单位标注 |
| 22 | `openEditorCmd()` | 通过 `sh -c` 调用编辑器 |
| 23 | `redactUI()` | 全局脱敏影响调试 |
| 25 | `refreshWorkspace()` | 固定 1 秒轮询无背压 | |

---

## 2. 参考案例：p2r_tui 设计分析

[p2r_tui](https://github.com/xuanli520/p2r_tui) 是同一技术栈（Go + Bubble Tea v1.3.10 + lipgloss v1.1.0 + Cobra v1.10.2 + SQLite）的 **QA 工作台 TUI**。代码组织成熟，是我们改造的直接参考模板。

### 2.1 架构对比

| 维度 | Harbor Flow (当前) | p2r_tui (目标) |
|------|-------------------|----------------|
| 文件组织 | 4 文件, ~4300 行（单文件 2924 行） | 28 文件, ~9700 行，按关注点拆分 |
| 组件模型 | 扁平 `model` struct，无子模型 | `Page`/`Overlay` 接口 + `pageRouter` |
| 焦点管理 | 无形式化焦点系统 | 栈式 `focusManager` + 7 个 `focusArea` |
| 文本输入 | 手写字符串拼接（`+= value`） | `bubbles/textinput`（光标/IME/粘贴/验证） |
| 滚动内容 | 手写行偏移计算 | `bubbles/viewport`（LineUp/LineDown/PageUp/GotoTop） |
| 数据表格 | 手写 `%-22s` 格式化对齐 | `bubbles/table`（排序/翻页/自适应列宽） |
| 帮助系统 | 固定 2 变体 footer | 上下文感知 footer（每个 focusArea 不同） |
| 国际化 | **零**（全英文硬编码） | **完整 `localize.go`**（10+ 函数, 70+ 翻译对） |
| 确认对话框 | **零**（单键即执行） | 退出/取消/清理均需 y/Enter 确认 |
| 响应式布局 | `width - 4` | 4 断点自适应（≥120/90/72/<72） |

### 2.2 p2r_tui 的 app struct（`app.go`）

```go
type app struct {
    store        appStore
    cfg          config.Config
    scheduler    schedulerClient
    lifecycle    *tasklifecycle.Manager
    poller       *schedulerPoller

    router      *pageRouter       // ★ Page/Overlay 路由
    taskBoard   *TaskBoardModel   // 题目管理页
    taskInput   *TaskInputModel   // 任务输入（textinput）
    overview    *OverviewModel    // 总览页（bubbles/table）
    focusManager focusManager     // ★ 焦点栈

    tab     int                   // panelTaskBoard / panelOverview / panelExecution
    focus   focusArea             // ★ 当前焦点区域

    width   int
    height  int

    message     string            // ★ Toast 消息（底部一行，自动消失）
    qaMode      string            // initial / recheck
    runConfig   runConfig         // 重跑配置对话框
    diagnostics taskDiagnosticsState
    activeJobs  []scheduler.JobSnapshot

    confirmCancelTaskID string    // ★ 确认状态机
    confirmQuit         bool
    taskTypePrompt      taskTypePrompt
    verdictPrompt       verdictPrompt
}
```

**与 Harbor Flow 的关键差异：**
- `router` + `focus` 替代了 `view` 枚举 —— 支持页面/覆盖层的层级路由和焦点追踪
- `taskBoard`/`taskInput`/`overview` 是独立子模型，各自实现 Page 接口
- `message` 是轻量级 toast，不参与数据层（Harbor Flow 的 `err`/`notice` 与数据混合）
- 确认状态机（`confirmCancelTaskID`, `confirmQuit` 等）是 model 的显式字段

### 2.3 Page/Overlay 路由系统（`router.go:1-136`）

```go
type pageID int
const (
    pageTaskBoard pageID = iota  // 题目管理
    pageOverview                  // 总览
    pageExecution                 // 执行详情
)

type Page interface {
    Init() tea.Cmd
    Update(tea.Msg) (bool, tea.Cmd)  // bool: handled
    View(width, height int) string
    Focus()
    Blur()
    HandleKey(tea.KeyMsg) tea.Cmd
    Destroy() tea.Cmd
}

type Overlay interface {
    Page
    ZIndex() int              // 覆盖层叠顺序
    InterceptsAllKeys() bool  // 模态：拦截所有按键
}

type pageRouter struct {
    descriptors []pageDescriptor  // [{id, name: "题目管理"}, {name: "总览", key: "Ctrl+O"}, ...]
    pages       map[pageID]Page
    overlays    []Overlay         // 覆盖层栈
    active      pageID
}
```

**关键方法：**
- `SwitchTo(id)` — 切换页面，自动调用旧页面的 `Blur()` 和新页面的 `Focus()`
- `PushOverlay(overlay)` — 推送覆盖层（设置/确认框）
- `PopOverlay()` — 弹出覆盖层
- `Dispatch(msg)` — 优先路由到顶层 Overlay，未处理则到当前 Page

### 2.4 焦点管理系统（`focus.go:1-53` + `keymap.go:22-36`）

```go
type focusArea int
const (
    focusSearch         focusArea = iota  // 搜索框
    focusOverviewTable                      // 总览表格
    focusTaskBoard                         // 任务看板
    focusTaskInput                         // 任务输入框
    focusStageList                         // 阶段列表
    focusRefRunList                        // 参考运行列表
    focusDetailViewport                    // 详情视口
)

type focusManager struct {
    stack   []focusTarget  // page / inputBox / overlay
    current focusTarget
}
```

焦点栈支持 `Push`/`Pop`/`SetCurrent` 操作。当打开输入框或设置覆盖层时，当前焦点被推入栈中，关闭时恢复。

### 2.5 上下文感知 Footer（`keymap.go` 中的 `footerFor()` 函数）

每个 focusArea 有完全不同的帮助文本。这是 p2r_tui 最核心的人性化设计：

| 焦点区域 | Footer 文本 |
|---------|------------|
| `focusTaskBoard` | `/ 输入题目  Ctrl+S 启动服务  Ctrl+E 判定完成  Ctrl+R 重检  Ctrl+W 重试Git  Ctrl+D 诊断  Ctrl+O 总览  Ctrl+/ 设置  Q 退出` |
| `focusTaskInput` | `Enter 开始质检  Esc 清空  ←→ 光标  Ctrl+E/Ctrl+R/Ctrl+D/Ctrl+O 全局  Ctrl+/ 设置  Q 退出` |
| `focusSearch` | `Ctrl+C 退出  Tab 执行详情  ↓ 表格  Ctrl+R 重跑  Ctrl+D 诊断  Ctrl+X 终止` |
| `focusOverviewTable` | `↑↓选择 Enter详情 /搜索 s排序 S反向 PgUp/PgDn翻页 z条数 Ctrl+R重跑 Ctrl+D诊断 Ctrl+X终止 m模式` |
| `focusStageList` | `↑↓ 阶段  Enter 详情  Ctrl+R 重跑  Ctrl+D 诊断  Ctrl+X 终止  m 模式` |
| `focusDetailViewport` | `↑↓ 滚动  PgUp/PgDn 翻页  Esc 返回阶段  Ctrl+D 诊断  Ctrl+X 终止` |
| 确认对话框 | `y/Enter 确认终止  n/Esc 取消` |
| 设置面板 | `Esc/Q/Ctrl+/ 关闭设置  Tab/Shift+Tab 字段  ↑↓ 字段  Space 开关  Enter 执行` |

### 2.6 中文翻译系统（`localize.go:1-289`）

10 个翻译函数，覆盖所有状态值：

```go
localizeRunStatus():    "通过" / "有发现" / "运行中" / "已中止" / "崩溃"
localizeTaskState():    "开始质检" / "待处理" / "结束质检"
localizeStageStatus():  "完成" / "失败" / "已阻塞" / "运行中" / "等待中" / "已跳过"
localizeStageName():    A→"结构与规则检查" B→"Docker运行时证据" C→"测试运行时证据"
                        D→"测试有效性静态审查" E→"静态验收审计" F→"标注员修复静态审查" G→"浏览器前端 E2E"
localizeManualVerdict():"未判定" / "通过" / "返工" / "不通过"
localizeSeverity():     "阻断" / "严重" / "中等" / "低"
localizeMode():         "打回重检" / "静态质检" / "运行时" / "首次质检"
localizeJobState():     "排队中" / "运行中" / "已完成" / "已终止" / "失败"
localizeSummary():      翻译 English summary → 中文（含正则匹配 "N validation finding(s)"）
stageStatusIcon():      ✓(绿) ✗(红) ⊘(黄) ▶(蓝) ○(灰)  — Unicode + 颜色
```

### 2.7 标准组件使用

| 组件 | 用途 | 文件 |
|------|------|------|
| `bubbles/textinput` | 任务 ID 输入（Prompt + Placeholder + CharLimit + Width） | `taskinput.go:15-24` |
| `bubbles/table` | 总览数据表（排序/翻页/滚动/自适应列宽/选中高亮） | `overview.go` |
| `bubbles/viewport` | 详情内容滚动（LineUp/Down, PageUp/Down, GotoTop/Bottom） | `app.go` 中的 `detail viewport.Model` |

### 2.8 确认状态机

```go
// app.go handleKey 中的确认处理
if m.confirmCancelTaskID != "" {
    switch key {
    case "y", "Y", "enter":  /* 确认取消 */   // → cancelTaskCmd
    case "n", "N", "esc":    /* 取消操作 */   // → 清理确认状态
    }
}
if m.confirmQuit {
    switch key {
    case "y", "Y", "enter":  /* 确认退出 */   // → quitCleanupCmd
    case "n", "N", "esc":    /* 取消退出 */   // → 清理确认状态
    }
}
```

### 2.9 响应式布局（`layout.go`）

```go
func layoutFor(width, height int, execution bool) appLayout {
    contentWidth  := max(24, width - 2)
    contentHeight := max(6, height - 7)
    switch {
    case width < 72:  mode = layoutMinimal   // 单列，极简
    case width < 90:  mode = layoutStacked   // 上下排列
    case width < 120: mode = layoutMedium    // 较窄侧栏
    default:          mode = layoutWide      // 宽侧栏
    }
    // execution 视图在 wide/medium 下左右分栏，stacked/minimal 下上下分栏
}
```

列可见性随宽度自适应（`overviewColumnSpecs`），窄屏隐藏批次、最后运行、模式等列。

---

## 3. 改造总览

### 3.1 指导原则

1. **中文优先**: 所有用户界面文本中文化，状态值中文化，键位标签中文化
2. **可发现性**: 所有操作在 footer 中可见，无隐藏快捷键
3. **容错性**: 破坏性操作必须确认，误操作可撤销
4. **渐进增强**: 先修复关键痛点，再添加高级功能
5. **组件化**: 使用 Bubble Tea 标准组件，停止手写基础控件

### 3.2 数据流全貌（供实现参考）

改造需要理解 TUI 接收和操作的核心数据结构（`domain/types.go`）：

```
Runner (runner.go:120)
  │
  ├─ Events() → <-chan RunnerEvent (cap 64, buffered)
  │   ├── Type: "run_started" / "node_started" / "node_succeeded" / "node_failed"
  │   ├── Type: "gate_requested"  → Gate field populated (*GateRequest)
  │   └── Type: "run_succeeded" / "run_failed" / "run_recovered"
  │
  ├─ SubmitGateDecision(decision)  ← TUI 发送 GateDecision 回 Runner
  │
  └─ Run() return → (RunSummary, error)  ← 最终聚合结果

25 个节点 (nodes/nodes.go:37-66):
  repo_prepare → repo_analyze → task_design → [task_review GATE]
  → generate_task_files → ... → content_review → [content_review GATE]
  → codeedge_lint → harbor_verify → quality_check → similarity_check
  → harbor_run_qwen → harbor_run_opus → [result_review GATE]
  → submission_lint → [final_review GATE] → package

4 个 Gate 关卡 (runner.go:1601-1672):
  TaskReview (phase1) → ContentReview (phase1) → ResultReview (phase3) → FinalReview (phase2)
```

### 3.3 优先级矩阵

| 优先级 | 范围 | 对应痛点 | 用户影响 |
|--------|------|---------|---------|
| **P0** | 确认对话框、textinput 替换、进度指示器(24)、架构拆分(20)、表单(7,8,17) | #18, #8, #17, #24, #20, #7 | 数据安全+基本可用 |
| P1 | 中文化、键位重设计、Tab 统一 | #1-6, #21 | UX 核心 |
| P2 | footer 系统、滚动指示器、视觉增强 | #3, #12, #14, #15, #19 | 工作流效率 |
| P3 | 鼠标(10)、响应式、路径补全(11) | #9-11, #13, #16, #22-25 | 细节优化 |

---

## 4. 架构重构方案

### 4.1 新文件结构

```
internal/tui/
├── app.go              # 顶层 app struct + Init/Update/View + tea.Model 合规
├── router.go           # Page/Overlay 接口 + pageRouter (参考 p2r_tui/router.go)
├── focus.go            # focusArea 枚举 + focusManager 栈 (参考 p2r_tui/focus.go)
├── keymap.go           # 全局/上下文键位绑定 + handleKey 分发 (参考 p2r_tui/keymap.go)
├── layout.go           # 响应式布局计算 + clamp/max/min 工具函数
├── shared.go           # 共享类型 + domain 类型适配
├── render.go           # 共享渲染函数 (renderHeader, renderPanel, renderFooter)
├── styles.go           # Theme struct + 所有 lipgloss Style 定义
├── localize.go         # 中文本地化函数（所有翻译集中在此 - 参考 p2r_tui/localize.go）
├── messages.go         # 自定义 tea.Msg 类型（复用现有 5 个 + 新增）
│
├── pages/
│   ├── start.go        # 启动/配置表单页 ← 对应现有 startView()
│   ├── overview.go     # 工作流概览页（25 节点状态列表） ← 对应 overview()
│   ├── gate.go         # Gate 审查页 ← 对应 gateView()
│   ├── detail.go       # 节点详情页 ← 对应 nodeDetailView()
│   ├── logs.go         # 日志查看页 ← 对应 logsView()
│   └── done.go         # 完成摘要页 ← 对应 doneView()
│
├── components/
│   ├── confirm.go      # 确认对话框覆盖层（用于 Gate 操作 + 退出 + 取消）
│   ├── toast.go        # Toast 通知系统（替代 err/notice 竞争槽位）
│   ├── artifact.go     # 工件预览组件（文件内容显示 + 截断处理）
│   ├── checklist.go    # Gate 检查清单组件（Critical/Passed 渲染）
│   └── statusbar.go    # 状态栏/进度条（spinner + 运行时间）
│
└── run.go              # TUI 入口（工作区检测 + 5 模式分发 - 保持现有逻辑）
```

### 4.2 核心接口

```go
// Page 表示一个全屏页面
type Page interface {
    Init() tea.Cmd
    Update(msg tea.Msg) (bool, tea.Cmd)  // bool: 是否已处理
    View(width, height int) string
    Focus()   // 页面获得焦点
    Blur()    // 页面失去焦点
    HandleKey(tea.KeyMsg) tea.Cmd
}

// Overlay 表示一个模态覆盖层（对话框、设置等）
type Overlay interface {
    Page
    ZIndex() int              // 层叠顺序
    InterceptsAllKeys() bool  // 是否拦截所有按键
}
```

### 4.3 顶层 App Model

```go
type app struct {
    router    *pageRouter
    focus     focusArea
    focusMgr  focusManager

    width     int
    height    int

    // 运行时状态
    runner    *app.Runner
    events    []domain.RunnerEvent
    summary   domain.RunSummary

    // 通知
    message   string          // 底部 toast 消息
    notice    string          // 持久通知

    // 模态
    confirm   *ConfirmDialog
}
```

---

## 5. 中文化方案

### 5.1 i18n 模块设计

创建 `internal/tui/localize.go`，结构参考 p2r_tui 的 `localize.go`（289 行，10+ 翻译函数）。Harbor Flow 需要翻译的内容更多——涉及 25 个节点名、4 个 Gate 名、39 个表单字段标签、以及大量运行时消息。

### 5.2 完整翻译表（基于 model.go 实际字符串）

#### 视图标题（`model.go:888-907`）
| English（来源） | 中文 |
|---------|------|
| "Harbor Task Factory" (header) | Harbor 出题工坊 |
| "Start Workflow" (section in startView) | 启动工作流 |
| "Overview" | 总览 |
| "No events yet." | 暂无事件 |
| "No active gate." (gateView) | 暂无活跃审查关卡 |
| "Node Detail" | 节点详情 |
| "No node events yet." | 暂无节点事件 |
| "Logs" | 日志 |
| "No logs yet." | 暂无日志 |
| "Done" | 完成 |
| "Run is still in progress." | 运行仍在进行中 |
| "Failed: " | 失败： |
| "Completed with failing checks." | 已完成，但有检查未通过 |
| "Completed successfully." | 已成功完成 |
| "Read-only snapshot" (gateView footer) | 只读快照 |
| "Starting Harbor Factory..." | 正在启动 Harbor 出题工坊... |

#### 状态值（`model.go:2063-2076` statusIcon + RunnerEvent.Type）
| English | 中文 | 新 Unicode 图标 |
|---------|------|----------------|
| "succeeded" / "node_succeeded" / "run_succeeded" | 成功 | ✓ (绿) |
| "failed" / "node_failed" / "run_failed" | 失败 | ✗ (红) |
| "canceled" / "node_canceled" | 已取消 | ⊗ (红) |
| "running" / "node_started" | 运行中 | ◌ (蓝, 动画 spinner) |
| "pending" (default) | 等待中 | ○ (灰) |
| "gate_requested" | 等待审查 | ⚷ (黄) |
| CheckPass | 通过 | ✓ (绿) |
| CheckWarn | 警告 | ⚠ (黄) |
| CheckFail | 失败 | ✗ (红) |

#### 25 个节点名称（`nodes/nodes.go:8-35`）
| 节点 ID | 中文名称 | 阶段 |
|---------|---------|------|
| repo_prepare | 仓库准备 | Phase 0 |
| repo_analyze | 仓库分析 | Phase 1 |
| task_design | 任务设计 | Phase 1 |
| task_review | 任务审查 [关卡] | Phase 1 |
| generate_task_files | 生成任务文件 | Phase 1 |
| instruction_generate | 生成指令文档 | Phase 1 |
| task_toml_generate | 生成任务配置 | Phase 1 |
| dockerfile_generate | 生成 Docker 配置 | Phase 1 |
| solve_generate | 生成解答脚本 | Phase 1 |
| test_generate | 生成测试脚本 | Phase 1 |
| tests_analysis | 测试分析 | Phase 1 |
| materialize_task | 物化任务文件 | Phase 1 |
| content_review | 内容审查 [关卡] | Phase 1 |
| codeedge_lint | 代码检查 | Phase 2 |
| harbor_verify | Harbor 验证 | Phase 2 |
| docker_build | Docker 构建 | Phase 2 |
| initial_verify | 初始验证 | Phase 2 |
| oracle_verify | Oracle 验证 | Phase 2 |
| quality_check | 质量检查 | Phase 2 |
| similarity_check | 相似度检查 | Phase 2 |
| harbor_run_qwen | Qwen 模型运行 | Phase 3 |
| harbor_run_opus | Opus 模型运行 | Phase 3 |
| submission_lint | 提交检查 | Phase 2 |
| result_review | 结果审查 [关卡] | Phase 3 |
| final_review | 最终审查 [关卡] | Phase 2 |
| package | 打包 | 最终 |

#### 4 个 Gate 名称（`runner.go` reviewGate）
| Gate ID | 中文名称 | 阶段 | 支持的操作 |
|---------|---------|------|-----------|
| TaskReview | 任务审查 | Phase 1 | approve / reject |
| ContentReview | 内容审查 | Phase 1 | approve / reject |
| FinalReview | 最终审查 | Phase 2 | approve / reject / revise |
| ResultReview | 结果审查 | Phase 3 | approve / reject / refresh |

#### Gate 操作
| English（来源 model.go） | 中文 |
|---------|------|
| "approve" | 批准 |
| "reject" | 拒绝 |
| "revise" / "revise and rerun checks" | 修订并重新运行检查 |
| "refresh screenshot evidence" | 刷新截图证据 |
| Gate notes editing | 编辑审查备注 |
| "edit artifact" | 编辑工件 |
| "next artifact" | 下一个工件 |
| "critical" (checklist tag) | 严重 |

#### 39 个表单字段标签（`model.go:909-1057` renderStartField）
| English (hardcoded label) | 中文 | 类型 | 单位 |
|---------|------|------|------|
| "Mode" | 模式 | 切换 | - |
| "Run existing task" | 运行已有任务 | 模式值 | - |
| "Generate from repo" | 从仓库生成 | 模式值 | - |
| "Task" | 任务路径 | 文本 | - |
| "Repo" | 仓库地址 | 文本 | - |
| "Commit" | 提交哈希 | 文本 | - |
| "Workspace" | 工作区路径 | 文本 | - |
| "Task output" | 任务输出目录 | 文本 | - |
| "Tests analysis" | 测试分析路径 | 文本 | - |
| "Qwen result" | Qwen 结果路径 | 文本 | - |
| "Opus result" | Opus 结果路径 | 文本 | - |
| "Qwen screenshot" | Qwen 截图路径 | 文本 | - |
| "Opus screenshot" | Opus 截图路径 | 文本 | - |
| "Package output" | 打包输出目录 | 文本 | - |
| "Docker verify" | Docker 验证 | 布尔 | - |
| "Quality check" | 质量检查 | 布尔 | - |
| "Quality agent" | 质量检查代理 | 布尔 | - |
| "Similarity check" | 相似度检查 | 布尔 | - |
| "GitHub similarity" | GitHub 相似度 | 布尔 | - |
| "Similarity threshold" | 相似度阈值 | 浮点 | (0-1) |
| "History dirs" | 历史目录 | 列表 | - |
| "TB3 dirs" | TB3 目录 | 列表 | - |
| "Run Harbor" | 运行 Harbor | 布尔 | - |
| "Harbor agent" | Harbor 代理 | 文本 | - |
| "Qwen model" | Qwen 模型 | 文本 | - |
| "Opus model" | Opus 模型 | 文本 | - |
| "Qwen Harbor base URL" | Qwen Harbor 地址 | 文本 | - |
| "Opus Harbor base URL" | Opus Harbor 地址 | 文本 | - |
| "Harbor timeout" | Harbor 超时 | 整数 | 秒 |
| "Harbor setup timeout" | Harbor 启动超时 | 整数 | 秒 |
| "Harbor preflight" | Harbor 预检 | 布尔 | - |
| "Harbor concurrency" | Harbor 并发数 | 整数 | - |
| "Harbor attempts" | Harbor 尝试次数 | 整数 | - |
| "Harbor infra retries" | Harbor 基础设施重试 | 整数 | - |
| "Package" | 打包 | 布尔 | - |
| "Task name" | 任务名称 | 文本 | - |
| "Code lang" | 编程语言 | 文本 | - |
| "Task type" | 任务类型 | 文本 | - |
| "Application" | 应用领域 | 文本 | - |
| "AHT" | 预估耗时 | 文本 | 分钟 |
| "Description" | 描述 | 文本 | - |
| "Zero to one" | 从零到一 | 布尔 | - |
| "Codex model" | Codex 模型 | 文本 | - |
| "Codex reasoning" | Codex 推理 | 文本 | - |
| "Codex path" | Codex 路径 | 文本 | - |
| "Agent timeout" | 代理超时 | 整数 | 秒 |

#### Footer 文本（`model.go:2056-2061`）
| English | 中文 |
|---------|------|
| "Overview" | 总览 |
| "Gate/Node" | 审查/节点 |
| "Detail" | 详情 |
| "Cancel model" | 取消模型运行 |
| "Quit" | 退出 |
| "(read-only)" | （只读） |
| "[Tab] field  [Space] toggle  [Enter] start" | [Tab] 切换字段  [Space] 切换  [Enter] 开始 |

#### 运行时消息
| English（来源 model.go） | 中文 |
|---------|------|
| "workspace snapshot is read-only..." | 工作区快照为只读，另一 Factory 进程持有该运行 |
| "Cancel requested for %s; the other model stage may continue." | 已请求取消 %s；其他模型阶段可能继续运行 |
| "cannot approve gate with failing critical checks: %s" | 无法批准：存在未通过的关键检查项：%s |
| "revise/refresh is available at Final Review and Result Review" | 修订/刷新仅在最终审查和结果审查中可用 |
| "artifact path is outside allowed TUI roots" | 工件路径超出允许的 TUI 根目录 |
| "artifact path is not an editable Harbor artifact" | 工件路径不是可编辑的 Harbor 工件 |

### 5.3 CJK 宽度处理

必须添加 `go-runewidth` 依赖并使用其替换所有 `len()` 调用：

```go
import "github.com/mattn/go-runewidth"

// 当前 model.go 中大量使用 len() + fmt.Sprintf("%-*s") 做对齐
// 例如 renderStartField 中的 %-22s 格式化
// 必须全部替换为 runewidth.StringWidth() 计算宽度后手动填充

func padRightDisplay(s string, width int) string {
    w := runewidth.StringWidth(s)
    if w >= width { return s }
    return s + strings.Repeat(" ", width - w)
}
```

---

## 6. 交互优化方案

### 6.1 新键位系统

**全局键位（所有视图统一）：**

| 键位 | 功能 | 替代旧键 |
|------|------|---------|
| `Ctrl+O` | 切换到总览 | 替代 `1` |
| `Ctrl+G` | 切换到审查（如有活跃 Gate） | 替代 `g`, `2` |
| `Ctrl+D` | 切换到节点详情 | 替代 `d` |
| `Ctrl+L` | 切换到日志 | 替代 `l`, `3` |
| `Ctrl+E` | 结束时切换到完成页 | 替代 `4` |
| `Ctrl+Q` / `q` | 退出程序 | 保持不变 |
| `Ctrl+X` | 取消当前运行（需确认） | 替代 `x` |
| `Tab` | 切换到下一个面板 | 增强 |
| `Shift+Tab` | 切换到上一个面板 | 新增 |
| `?` | 显示/隐藏帮助面板 | 新增 |
| `/` | 搜索/过滤 | 新增 |
| `Esc` | 返回上一级/关闭弹窗 | 新增 |

**Gate 审查键位：**

| 键位 | 功能 | 说明 |
|------|------|------|
| `Ctrl+A` 或 `a` | 批准 | 需确认对话框 |
| `Ctrl+R` 或 `r` | 拒绝 | 需确认对话框 |
| `Ctrl+V` 或 `v` | 修订 | 仅在可用时显示 |
| `Ctrl+N` | 编辑备注 | 使用 textarea 组件 |
| `e` | 编辑工件 | 保持不变 |

**日志查看键位：**

| 键位 | 功能 |
|------|------|
| `↑` / `↓` 或 `j` / `k` | 滚动 |
| `PgUp` / `PgDn` | 翻页 |
| `Home` / `g` | 跳到顶部 |
| `End` / `G` | 跳到底部 |
| `t` | 切换自动跟踪 |
| `Tab` | 下一个日志文件 |

### 6.2 上下文感知 Footer

**总览视图 Footer：**
```
[Ctrl+O 总览] [Ctrl+G 审查] [Ctrl+L 日志] [Ctrl+D 详情] [Ctrl+E 完成] [Ctrl+X 取消运行] [q 退出] [? 帮助]
```

**Gate 审查 Footer（活跃）：**
```
[Ctrl+A 批准] [Ctrl+R 拒绝] [Ctrl+V 修订] [Ctrl+N 备注] [e 编辑工件] [Tab 下一工件] [Esc 返回] [? 帮助]
```

**启动表单 Footer：**
```
[Tab/↓ 下一字段] [Shift+Tab/↑ 上一字段] [Space 切换] [Ctrl+U 清空] [Enter 开始] [q 退出]
```

### 6.3 确认对话框

所有破坏性操作必须经过确认：

```go
type ConfirmDialog struct {
    title   string
    message string
    onYes   tea.Cmd
    onNo    tea.Cmd
    focused bool  // true = Yes 高亮, false = No 高亮
}

// 渲染为居中覆盖层
func (d ConfirmDialog) View(width, height int) string {
    // 半透明背景 + 居中弹窗
    // [←→ 选择] [Enter 确认] [Esc 取消]
}
```

需要确认的操作：
- 批准 Gate（`a`）
- 拒绝 Gate（`r`）
- 取消运行（`Ctrl+X`）
- 退出程序（`q`，当有运行中的任务时）
- 编辑工件（`e`）

### 6.4 表单重构

**当前问题：** 40+ 字段平铺，Tab 逐个遍历。

**方案：** 使用分组 + `bubbles/textinput`：

**模式选择（第 1 屏）：**
```
┌ 启动工作流 ──────────────────────────┐
│                                        │
│  模式:  [▶ 运行已有任务]               │
│         [  从仓库生成  ]               │
│                                        │
│  任务路径: [________________________] │
│  仓库地址: [________________________] │
│  提交哈希: [________________________] │
│  工作区:   [________________________] │
│                                        │
│  [Enter] 下一步  [Esc] 返回  [q] 退出  │
└────────────────────────────────────────┘
```

**高级选项（第 2 屏，可折叠）：**
```
┌ 高级选项 ─────────────────────────────┐
│  ▸ Harbor 配置                         │  ← 可折叠
│    Qwen 模型: [qwen3.7-max________]   │
│    Opus 模型: [claude-opus-4-8_____]  │
│    Harbor 超时: [7200] 秒             │
│  ▸ 质量检查                            │
│  ▸ 相似度检查                          │
│  ▸ 打包选项                            │
└────────────────────────────────────────┘
```

### 6.5 进度指示器

添加 `bubbles/spinner` 用于表示后台活动：

```go
// 在 model 中添加
spinner spinner.Model

// 在长时间操作时显示
if m.runner.IsRunning() {
    spinnerView := m.spinner.View() + " 正在运行..."
}
```

### 6.6 鼠标支持

程序初始化时启用：

```go
p := tea.NewProgram(
    m,
    tea.WithAltScreen(),
    tea.WithMouseCellMotion(),  // 新增
    tea.WithReportFocus(),      // 新增
)
```

Gate 审批按钮、节点列表、表单字段均可点击。

---

## 7. 视觉优化方案

### 7.1 新主题系统

```go
type Theme struct {
    Primary    lipgloss.Color  // #00afff 主色调
    Success    lipgloss.Color  // #00d700 成功
    Warning    lipgloss.Color  // #ffaf00 警告
    Error      lipgloss.Color  // #ff5f5f 错误
    Muted      lipgloss.Color  // #808080 次要文字
    Border     lipgloss.Color  // #585858 边框
    Selected   lipgloss.Style  // 白色文字 + 蓝色背景

    Title      lipgloss.Style  // 标题样式
    Section    lipgloss.Style  // 小节标题样式
    Panel      lipgloss.Style  // 面板样式
    Help       lipgloss.Style  // 帮助文字样式
}
```

### 7.2 Unicode 状态图标

| 状态 | 旧图标 | 新图标 | 颜色 |
|------|--------|--------|------|
| 通过 | "OK" | ✓ | 绿色 #00d700 |
| 失败 | "!!" | ✗ | 红色 #ff5f5f |
| 警告 | "!!" | ⚠ | 黄色 #ffaf00 |
| 运行中 | ".." | ◌ 或 spinner | 蓝色 #00afff |
| 等待中 | "--" | ○ | 灰色 #808080 |
| 已阻塞 | - | ⊘ | 黄色 #ffaf00 |
| 已跳过 | - | - | 灰色 #808080 |

### 7.3 响应式布局

```go
func layoutFor(width, height int) appLayout {
    switch {
    case width >= 120:  // 宽屏：侧边栏 + 主内容
    case width >= 90:   // 中屏：较小侧边栏
    case width >= 72:   // 堆叠：上下排列
    default:            // 最小：极简布局
    }
}
```

---

## 8. 实施路线图

### Phase 1: 基础（预计 3-5 天）

**目标：** 建立中文化和基础组件体系，不影响现有功能。

- [x] 创建 `internal/tui/localize.go`，编写所有翻译函数
- [x] 创建 `internal/tui/styles.go` 新主题系统
- [x] 创建 `internal/tui/messages.go`，提取所有自定义消息类型
- [x] 替换所有硬编码英文字符串为 `localize*()` 调用
- [x] 替换所有文本状态图标为 Unicode 图标
- [x] 添加 `tea.WithMouseCellMotion()` 和 `tea.WithReportFocus()`
- [x] 引入 `bubbles/textinput` 替换手写文本输入（启动表单）
- [x] 引入 `bubbles/spinner` 添加运行中进度指示

### Phase 2: 核心重构（预计 5-7 天）

**目标：** 拆分巨型模型，建立 Page/Overlay 架构。

- [x] 提取 `pageRouter` 和 `Page`/`Overlay` 接口
- [x] 提取 `focusManager`
- [x] 将 `model.go` 按视图拆分为独立文件：`start.go`, `overview.go`, `gate.go`, `detail.go`, `logs.go`, `done.go`
- [x] 实现新的上下文感知 footer 系统
- [x] 实现确认对话框组件
- [x] 重设计键位系统（Ctrl+ 快捷键 + 数字键向后兼容）

### Phase 3: UX 打磨（预计 3-5 天）

**目标：** 优化交互体验。

- [x] 启动表单重构：字段分组 + 折叠 + `textinput` 组件
- [x] Gate Notes 编辑使用 `bubbles/textarea`
- [x] 节点详情使用 `bubbles/viewport`
- [x] Overview 使用 `bubbles/table`
- [x] Gate 检查清单组件化
- [x] Toast 通知系统

### Phase 4: 高级特性（预计 3-5 天）

**目标：** 锦上添花的功能。

- [x] 响应式布局（4 断点）
- [x] 搜索/过滤功能
- [x] 帮助面板（`?` 键）
- [x] 路径自动补全（使用 `bubbles/filepicker` 思路）
- [x] Tab 键行为统一和文档化
- [x] 滚动指示器

---

## 9. 风险评估

| 风险 | 概率 | 影响 | 缓解措施 |
|------|------|------|---------|
| 重构引入回归 bug | 中 | 高 | 保留现有 `model_test.go`（1263 行测试），逐步迁移，每步都跑测试 |
| CJK 字符宽度计算错误 | 高 | 中 | 使用 `go-runewidth` 全部替换 `len()`，编写专门的中文渲染测试 |
| 键位变更用户不习惯 | 高 | 中 | Phase 2 保留数字键 1-4 作为向后兼容，过渡期后废弃 |
| huh 表单库学习曲线 | 低 | 低 | Phase 1 先用 `bubbles/textinput`，Phase 3 按需引入 `huh` |
| 性能退化（更多组件） | 低 | 低 | Bubble Tea 组件经过充分优化，拆分后 Update 分发开销可忽略 |
| p2r_tui 参考过度引入不相关功能 | 中 | 中 | 仅采用其架构模式和中文化方案，业务逻辑保持 Harbor Flow 原有实现 |

---

## 附录 A: 文件变更清单

### 新增文件
```
internal/tui/localize.go
internal/tui/messages.go
internal/tui/router.go
internal/tui/focus.go
internal/tui/keymap.go
internal/tui/layout.go
internal/tui/shared.go
internal/tui/render.go
internal/tui/pages/start.go
internal/tui/pages/overview.go
internal/tui/pages/gate.go
internal/tui/pages/detail.go
internal/tui/pages/logs.go
internal/tui/pages/done.go
internal/tui/components/confirm.go
internal/tui/components/toast.go
internal/tui/components/artifact.go
internal/tui/components/checklist.go
internal/tui/components/statusbar.go
```

### 修改文件
```
internal/tui/model.go       → 拆分为 pages/*.go, 保留向后兼容重导出
internal/tui/run.go         → 适配新 app 结构
internal/tui/styles.go      → 扩展为主题系统
cmd/tui.go                  → 可能不需要改动
```

### 删除文件
```
无（渐进重构，保留旧模型直到新系统完全就绪）
```

---

## 附录 B: 测试策略

1. **单元测试:** 每个 Page 组件独立可测试（传入固定 width/height，检查 View 输出）
2. **键位测试:** 使用 `keymap_test.go` 验证所有键位绑定正确映射到操作
3. **中文化测试:** 验证所有 localize 函数覆盖所有已知状态值
4. **CJK 宽度测试:** 验证中文字符串在布局中的列位置计算正确
5. **集成测试:** 保留现有 `model_test.go`，逐步迁移到新组件
6. **快照测试:** 固定终端尺寸下的渲染输出快照

---

## 附录 C: p2r_tui 项目结构参考

```
p2r_tui/internal/tui/          (28 文件, ~9700 行)
├── app.go          1449 行    顶层 model + Init/Update/View
├── viewmodel.go     1112 行    执行详情 ViewModel + 数据查询
├── keymap.go         875 行    键位绑定 + handleKey 分发
├── overview.go       722 行    总览表（bubbles/table）
├── render.go         560 行    渲染函数 + 布局
├── docker_mirror.go  564 行    Docker 镜像管理页
├── taskboard.go      411 行    任务看板（多列布局）
├── taskcard.go       410 行    任务卡片渲染
├── runconfig.go      308 行    运行配置表单
├── localize.go       289 行    中文本地化（核心参考）
├── layout.go         289 行    响应式布局 + 工具函数
├── shared.go         286 行    共享类型 + 工具函数
├── tasktype.go       193 行    题目类型选择
├── taskinput.go      104 行    任务输入组件（textinput）
├── tasklist.go        87 行    任务列表
├── router.go         136 行    Page/Overlay 路由
├── settings.go        50 行    设置面板
├── settings_overlay.go 97 行  设置覆盖层
├── focus.go           53 行    焦点管理
├── stage_plan.go     119 行    阶段计划
├── pipelinebar.go    139 行    流水线进度条
├── pipelineview.go    63 行    流水线视图
├── filepicker.go      12 行    文件选择器
├── diagnostics.go    150 行    诊断面板
├── cleanup.go         87 行    清理面板
├── poller.go         107 行    调度器轮询
└── testhooks.go      913 行    测试钩子
```
