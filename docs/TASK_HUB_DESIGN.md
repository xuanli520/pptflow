# 已归档：V1 Workspace Hub 设计

> 状态：历史设计材料，已由 2026-07-13 的 V2 lifecycle refactor 取代。本文描述旧的 workspace-index 与 clone-rerun 方案，不是操作或实现契约。
>
> 当前行为以 [`WORKFLOW_STABILITY_AND_TASK_LIFECYCLE_REFACTOR_PLAN.md`](./WORKFLOW_STABILITY_AND_TASK_LIFECYCLE_REFACTOR_PLAN.md)、[`WORKFLOW_STABILITY_DECISIONS.md`](./WORKFLOW_STABILITY_DECISIONS.md) 和 [`TUI_USAGE.md`](./TUI_USAGE.md) 为准。

## V1 原始文档：Harbor Flow 工作区管理中心（Task Hub）设计文档

> 基于对 Harbor Flow 现有 TUI 源码（27 文件）、p2r_tui 参考项目（28 文件）、Bubble Tea 生态最佳实践的全面分析
>
> 分析深度：逐文件阅读两个项目的全部 TUI 源码 + domain 层 + app 层

---

## 1. 现状与痛点

### 1.1 当前流程：线性单次流水线

```
启动 TUI → StartForm → Overview → Gate/Detail/Logs → Done（终点，死胡同）
```

当前 6 个 `viewMode`（`model.go:36-42`）：

| 页面 | 用途 | 可返回？ |
|------|------|---------|
| `viewStart` | 配置新运行 | 无（入口页） |
| `viewOverview` | 25 节点状态总览 | Esc 从其他页返回此处 |
| `viewGate` | 人工审查关卡 | 可回 Overview |
| `viewNodeDetail` | 单节点详情 | 可回 Overview |
| `viewLogs` | 日志查看 | 可回 Overview |
| `viewDone` | 运行完成/失败摘要 | **不可返回，不可重跑** |

### 1.2 核心痛点

| # | 痛点 | 根因 | 用户影响 |
|---|------|------|---------|
| **P0** | 运行失败后无法重跑 | `viewDone` 是终点，没有返回路径 | 必须退出 TUI 重新从 CLI 启动 |
| **P0** | 无法浏览历史运行 | 没有工作区列表/管理页面 | 用户必须记住工作区路径并在命令行指定 |
| **P0** | 无法从历史运行恢复 | 恢复仅 CLI `--workspace` 支持，TUI 内不暴露 | 崩溃后无法在 TUI 内续跑 |
| **P1** | 无法基于历史配置新建 | 每次新建需重填 39 字段表单 | 重复劳动，易出错 |
| **P1** | 无法对比多次运行结果 | 没有多工作区对比视图 | 调优 Harbor 参数时效率极低 |
| **P2** | 工作区占用磁盘不可见 | 没有磁盘用量/清理功能 | 磁盘可能被历史工作区占满 |

### 1.3 已有基础设施（可直接复用）

好消息是数据层已经就绪：

- **`status.ReadWorkspace(path)`**（`internal/harbor/status/status.go:18-41`）— 返回 `WorkspaceStatus{RunID, Status, Passed, StartedAt, FinishedAt, RunOptions{TaskName, CodeLang, TaskType, ...}, Resumable, Active}`，这是任务列表的完整数据源
- **`run_options.json`** — 每个工作区保存了完整配置快照（`RunnerOptionsSnapshot`），可复用于重跑
- **`state.json`** — 包含 `RunSummary` 全部结果，可渲染完成摘要
- **`event_log.jsonl`** — 追加式事件日志，支持增量恢复

---

## 2. p2r_tui 可借鉴模式分析

### 2.1 架构对比

| 维度 | Harbor Flow (当前) | p2r_tui (参考) | 借鉴策略 |
|------|-------------------|----------------|---------|
| 页面模型 | 6 个线性 viewMode | 3 主页面 + Overlay 栈 | **采纳**：Hub-and-Spoke 架构 |
| 任务数据源 | 文件系统（workspace dir） | SQLite 数据库 | **不适用**：Harbor Flow 保持文件驱动 |
| 任务生命周期 | 单次 run → done | inspecting → waiting → completed 循环 | **部分采纳**：workspace status 作为生命周期状态 |
| 重跑机制 | 无 | `runConfig` 对话框 + stage plan | **适配采纳**：从 `run_options.json` 重建配置 |
| 焦点管理 | `focusManager` 栈（已有！） | `focusManager` 栈 | **已对齐**：无需改动 |
| 页面路由 | `pageRouter` + Page/Overlay 接口（已有！） | `pageRouter` + Page/Overlay 接口 | **已对齐**：需新增一个 Page |
| 确认对话框 | `ConfirmDialog` Overlay（已有！） | 内联确认状态机 | **已对齐**：Harbor Flow 的 Overlay 方案更优 |
| 通知系统 | `toastState`（已有！） | `m.message` 单行 | **已对齐**：Harbor Flow 的 toast 方案更优 |
| 中文化 | `localize.go`（已有！） | `localize.go` | **已对齐**：需补充新页面翻译 |
| 上下文 Footer | 固定 2 变体 | 每个 focusArea 不同 | **需实现**：当前未充分利用 |

### 2.2 p2r_tui 任务管理模块核心组件

| 文件 | 行数 | 职责 | 对 Harbor Flow 的适用性 |
|------|------|------|------------------------|
| `taskboard.go` | 411 | 3 列 Kanban 看板 | **适配**：改为工作区列表（表格优于 Kanban） |
| `tasklist.go` | 87 | 单列（含 cursor/scroll） | **适配**：工作区列表需要类似结构 |
| `taskcard.go` | 410 | 任务卡片渲染 | **简化**：工作区列表行更简单 |
| `taskinput.go` | 104 | 任务 ID 输入 | **采纳**：用于搜索/过滤工作区 |
| `tasktype.go` | 193 | 题目类型选择 | **不适用**：Harbor 无此概念 |
| `runconfig.go` | 308 | 重跑配置对话框 | **适配**：从 `run_options.json` 重建 |
| `stage_plan.go` | 119 | 阶段依赖图 | **不适用**：Harbor Flow 有固定的 25 节点 |
| `pipelinebar.go` | 139 | 流水线进度条 | **不适用**：Harbor Flow 单任务运行 |
| `viewmodel.go` | 1112 | 执行视图 ViewModel | **不适用**：Harbor Flow 用 domain 类型 |

### 2.3 关键 UX 模式（值得采纳）

| 模式 | p2r_tui 实现 | Harbor Flow 适配 |
|------|-------------|-----------------|
| **Hub-and-Spoke** | 任务看板↔执行详情，Esc 返回中枢 | 工作区 Hub↔运行流程，Esc 返回 |
| **Esc 逐级回退** | `handleEscape()` 600+ 行 | 已在 `keymap.go` 有基础，需扩展 |
| **上下文 Footer** | 9 套不同 footer 文本 | 需为 Hub 页设计专属 footer |
| **准入控制** | `evaluateInspectionAdmission()` 6 项检查 | 可简化为：工作区是否 locked、是否可恢复 |
| **确认机制** | Y/N 全部破坏性操作 | 已对齐（`ConfirmDialog` Overlay） |
| **搜索/过滤** | 中英文混合搜索 | Hub 页用 `/` 触发搜索 |
| **实时轮询** | 2s/15s/30s 三级轮询 | 简化为 Hub 页扫描工作区目录 |

---

## 3. 设计目标

从用户视角定义目标，而非从代码视角：

1. **"我能看到我做过什么"** — 打开 TUI 第一眼看到所有历史工作区，而非一个空表单
2. **"失败了能重来"** — 任何运行结束后都能回到中枢，一键用相同配置重跑
3. **"中断了能继续"** — 崩溃/取消的工作区可以从中断处恢复
4. **"配置不用重填"** — 基于历史工作区的配置创建新运行，只需修改差异项
5. **"磁盘不被撑爆"** — 可视化工作区大小，支持清理

---

## 4. 核心设计决策

### 4.1 混合存储：SQLite 索引 + 文件系统为数据源

p2r_tui 用 SQLite 管理任务和运行记录。Harbor Flow 当前纯文件驱动。

> **决策：引入 SQLite 作为索引层，文件系统仍是完整数据的来源。** DB 存储"题目"和"运行"的元数据（用于快速列表、搜索、排序），大文件（state.json 完整报告、event_log.jsonl、截图等）仍保留在文件系统中。

**为什么需要 DB？**

| 场景 | 纯文件扫描 | SQLite 索引 |
|------|-----------|------------|
| 列出 100 个工作区 | 100 次 `stat` + `ReadWorkspace` + `LoadRunOptions` → 数秒 | 1 次 SQL 查询 → 毫秒 |
| 搜索"失败的 python 任务" | 扫描全部后内存过滤 | `WHERE status='failed' AND code_lang='py'` |
| 按时间排序 | 扫描全部后内存排序 | `ORDER BY finished_at DESC` |
| 同一题目的多次运行 | 无法关联（无 Task 概念） | `SELECT * FROM runs WHERE task_id = ?` |
| 数据库被删除 | — | 从文件系统完整重建 |

**为什么不全用 DB？**

- `state.json` 包含完整 `RunSummary`（所有报告、大量嵌套），不适合扁平化为 SQL 行
- `event_log.jsonl` 是追加式流，天然适合文件
- 工件文件（截图、日志、Docker 镜像）必须保留为文件
- 文件系统是权威来源，DB 是索引缓存，可随时重建

**数据分层：**

```
┌─────────────────────────────────────────────────┐
│ SQLite DB (.harbor-factory/harbor.db)           │
│   tasks 表: 题目元数据（名称/语言/类型/仓库）     │
│   runs 表:   运行元数据（状态/时间/大小/路径）    │
│   ← 用于 Hub 页的列表、搜索、排序、过滤         │
├─────────────────────────────────────────────────┤
│ 文件系统 (.harbor-factory/workspaces/*)         │
│   state.json:       完整 RunSummary            │
│   event_log.jsonl:  完整事件流                  │
│   run_options.json: 完整运行配置                │
│   phase*/*:         所有工件文件                │
│   ← 用于进入运行时的详细数据读取                │
└─────────────────────────────────────────────────┘
```

### 4.2 数据层设计

#### 4.2.1 实体关系

p2r_tui 的核心洞察：**"题目"和"运行"是两个实体。** 同一题目可以多次运行（不同参数、失败重试、调优重跑）。

```
Task (题目) 1 ──── N Run (运行/工作区)
  │                      │
  │ task_dir (唯一标识)    │ workspace_path (唯一标识)
  │ task_name              │ run_id
  │ code_lang              │ status
  │ task_type              │ started_at / finished_at
  │ application            │ size_bytes
  │ repo_url               │ is_active / is_resumable
  │ commit_sha             │ (FK → task_id)
  │ first_seen_at          │
```

**Harbor Flow 中的题目发现：**

1. **用户主动创建** — 通过 StartForm 指定 `TaskDir`（运行已有题目）
2. **系统生成** — `--generate` 模式从仓库生成新题目后运行
3. **首次发现** — TUI 扫描工作区时，从 `run_options.json` 提取题目信息自动注册

#### 4.2.2 SQLite Schema

```sql
-- 题目表：每个独特的 TaskDir 一条记录
CREATE TABLE IF NOT EXISTS tasks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    task_dir     TEXT NOT NULL UNIQUE,        -- 题目目录规范路径
    task_name    TEXT NOT NULL DEFAULT '',    -- 来自 task.toml 或 run_options
    code_lang    TEXT NOT NULL DEFAULT '',    -- 编程语言
    task_type    TEXT NOT NULL DEFAULT '',    -- 任务类型 (algo/web/cli/lib)
    application  TEXT NOT NULL DEFAULT '',    -- 应用领域
    repo_url     TEXT NOT NULL DEFAULT '',    -- 来源仓库 (generate 模式)
    commit_sha   TEXT NOT NULL DEFAULT '',    -- 仓库提交
    is_generated BOOLEAN NOT NULL DEFAULT 0,  -- 是否由 Factory 生成
    first_seen   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tasks_name ON tasks(task_name);
CREATE INDEX IF NOT EXISTS idx_tasks_lang ON tasks(code_lang);

-- 运行表：每个工作区一条记录
CREATE TABLE IF NOT EXISTS runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    workspace_path  TEXT NOT NULL UNIQUE,      -- 工作区规范路径
    run_id          TEXT NOT NULL,             -- 来自 state.json
    status          TEXT NOT NULL DEFAULT 'unknown',  -- running/succeeded/failed
    passed          BOOLEAN NOT NULL DEFAULT 0,
    started_at      DATETIME,
    finished_at     DATETIME,
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    is_active       BOOLEAN NOT NULL DEFAULT 0,  -- 是否被其他进程锁定
    is_resumable    BOOLEAN NOT NULL DEFAULT 0,  -- 是否可恢复
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_runs_task    ON runs(task_id);
CREATE INDEX IF NOT EXISTS idx_runs_status  ON runs(status);
CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at DESC);
```

#### 4.2.3 同步策略

DB 是文件系统的索引缓存，不是权威数据源。三种同步时机：

```
┌──────────────────────────────────────────────────────┐
│ 1. TUI 启动时（完整同步）                              │
│    ScanWorkspaces() → 每个 workspace 调用              │
│    status.ReadWorkspace() → UpsertTask() → UpsertRun()│
│    同时清理已经不存在的 workspace 记录                  │
│                                                      │
│ 2. 运行完成/状态变化时（增量同步）                       │
│    Runner 发出 runnerDoneMsg →                        │
│    UpsertRun(workspace, status, finished_at)          │
│                                                      │
│ 3. Hub 页后台轮询时（活性刷新）                         │
│    每 5s: 对 status='running' 的 runs                 │
│    调用 status.ReadWorkspace() 刷新状态                │
└──────────────────────────────────────────────────────┘
```

**Upsert 逻辑**：`INSERT OR REPLACE` — 文件系统数据始终覆盖 DB 中的元数据。DB 删除后完整重建。

#### 4.2.4 新增 Go 包

```
internal/harbor/store/
├── db.go          -- 打开/初始化 SQLite，执行 migration
├── task_store.go  -- Task CRUD: Upsert, GetByDir, List, Search, Delete
├── run_store.go   -- Run CRUD:  Upsert, GetByWorkspace, ListByTask, Delete
└── sync.go        -- ScanWorkspaces + SyncToDB (文件系统 → DB)
```

**依赖**：`github.com/mattn/go-sqlite3`（纯 Go，无需 CGO 的替代品 `modernc.org/sqlite` 也可以）。

**DB 文件位置**：`.harbor-factory/harbor.db`（放在 `.harbor-factory` 根目录下，与 `workspaces/` 平级）。

### 4.3 中枢页面：工作区 Hub（非 Kanban）

p2r_tui 使用 3 列 Kanban（审查中 / 等待人工 / 已完成）。Harbor Flow 工作区更适合：

> **决策：使用可排序表格 + 搜索栏，类似 p2r_tui 的 Overview 页。** 列：名称、状态、语言、类型、时间、大小。Enter 进入详情，`Ctrl+N` 新建，`Ctrl+R` 重跑。

理由：
- Harbor Flow 工作区没有明确的"审查中→等待→完成"状态流转
- 多个工作区之间没有依赖关系（不像 QA 任务有 pipeline）
- 表格天然支持排序和过滤，适合浏览大量历史工作区
- Harbor Flow 已有 `bubbles/table` 集成（`overviewTable`）

### 4.4 重跑 = 克隆配置 + 新工作区

p2r_tui 的 `runConfig` 允许精细控制重跑阶段。Harbor Flow 的 Runner 已有恢复机制。

> **决策：重跑提供两个选项 — (1) "恢复"：在原工作区继续运行（利用现有 `--workspace` 恢复） (2) "克隆运行"：复制 `run_options.json` 到新工作区，从头运行。**

### 4.5 保持现有 Page/Overlay 架构

Harbor Flow 已经实现了 p2r_tui 风格的 `Page`/`Overlay` 接口和 `pageRouter`。只需：

- 新增 `viewHub` 到 `viewMode` 枚举
- 新增 `hubPage` 实现 `Page` 接口
- 在 `render.go` 的 `bindPages()` 中注册

---

## 5. 用户工作流设计

### 5.1 改造后的完整流程

```
                    ┌─────────────────────────────┐
                    │       工作区 Hub（中枢）       │
                    │  ┌─────────────────────────┐ │
                    │  │ 搜索: [________] / 过滤  │ │
                    │  │ ─────────────────────── │ │
                    │  │ ▸ task-001  ✓成功 py alg│ │
                    │  │   task-002  ✗失败 go web│ │
                    │  │   task-003  ◌运行中 js  │ │
                    │  │   task-004  ⚷可恢复 cpp │ │
                    │  │ ─────────────────────── │ │
                    │  │ 共 4 个工作区  磁盘 2.3G │ │
                    │  └─────────────────────────┘ │
                    │  [Ctrl+N 新建] [Ctrl+R 重跑]  │
                    │  [Enter 详情] [Del 删除]      │
                    └──────┬──────────────────────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
              ▼            ▼            ▼
        ┌─────────┐ ┌──────────┐ ┌──────────┐
        │新建表单  │ │运行详情   │ │重跑确认   │
        │(Start)  │ │(Overview │ │(RunConfig│
        │         │ │ →Gate    │ │ Overlay) │
        │         │ │ →Detail  │ │          │
        │         │ │ →Logs    │ │          │
        │         │ │ →Done)   │ │          │
        └─────────┘ └──────────┘ └──────────┘
              │            │
              │    ┌───────┴────────┐
              │    │  Done 页底部    │
              │    │  [Esc 返回中枢] │  ← 关键改动
              │    │  [Ctrl+R 重跑] │  ← 关键改动
              │    └────────────────┘
              │            │
              └────────────┘────── Esc 返回中枢
```

### 5.2 典型用户场景

**场景 A：查看历史运行 → 重跑失败任务**
```
1. 启动 TUI → 看到工作区 Hub（4 个历史）
2. ↓ 移动到 task-002（✗失败）
3. Enter → 进入 Done 页，看到失败原因
4. Ctrl+R → 弹出重跑配置对话框
5. Enter 确认 → 回到 Hub，看到 task-005（◌运行中）
6. Enter → 进入 Overview 实时追踪
```

**场景 B：基于历史配置创建新任务**
```
1. 在 Hub 中选中 task-001（成功的 py 任务）
2. Ctrl+N → 进入 StartForm，表单预填 task-001 的配置
3. 修改 Repo URL 和 Commit
4. Enter → 开始新运行 → Hub 中出现新工作区
```

**场景 C：恢复崩溃的运行**
```
1. 启动 TUI → Hub 显示 task-003（⚷可恢复）
2. Enter → 弹出选项：恢复 / 放弃 / 只读查看
3. 选择"恢复" → 进入 Overview，从断点继续
```

---

## 6. 架构设计

### 6.1 新增数据结构

```go
// internal/tui/workspace_hub.go

// WorkspaceItem 是工作区 Hub 中一行的数据模型
// 由 store.ListRuns() JOIN tasks 查询结果填充
type WorkspaceItem struct {
    // 来自 runs 表
    RunID        string
    WorkspacePath string
    Status       string    // running/succeeded/failed
    Passed       bool
    StartedAt    time.Time
    FinishedAt   time.Time
    SizeBytes    int64
    IsActive     bool
    IsResumable  bool
    // 来自 tasks 表（JOIN）
    TaskName     string
    CodeLang     string
    TaskType     string
    Application  string
    TaskDir      string
}

// WorkspaceHubModel 是工作区列表页面
// 实现 Page 接口
type WorkspaceHubModel struct {
    pageBase
    items       []WorkspaceItem
    table       table.Model
    searchInput textinput.Model
    cursor      int
    sortBy      hubSortColumn
    sortAsc     bool
    searching   bool
    loading     bool
    err         error
}

type hubSortColumn int
const (
    hubSortByName hubSortColumn = iota
    hubSortByStatus
    hubSortByDate
    hubSortBySize
)
```

### 6.2 新增 viewMode

```go
// model.go 扩展
const (
    viewHub viewMode = iota  // 新增：工作区中枢
    viewStart
    viewOverview
    viewGate
    viewNodeDetail
    viewLogs
    viewDone
)
```

### 6.3 页面路由注册

```go
// render.go bindPages() 扩展
func (m *model) bindPages() {
    m.router.Register(viewHub,        &hubPage{pageBase: pageBase{m: m}})
    m.router.Register(viewStart,      &startPage{pageBase: pageBase{m: m}})
    m.router.Register(viewOverview,   &overviewPage{pageBase: pageBase{m: m}})
    m.router.Register(viewGate,       &gatePage{pageBase: pageBase{m: m}})
    m.router.Register(viewNodeDetail, &detailPage{pageBase: pageBase{m: m}})
    m.router.Register(viewLogs,       &logsPage{pageBase: pageBase{m: m}})
    m.router.Register(viewDone,       &donePage{pageBase: pageBase{m: m}})
}
```

### 6.4 组件树

```
tui.Run()
  └── app (tea.Model)
        ├── store *store.Store          ← 新增：SQLite 操作
        ├── pageRouter
        │     ├── [viewHub]        hubPage ─── table.Model + searchInput
        │     ├── [viewStart]      startPage ─── startForm (textinput × 39)
        │     ├── [viewOverview]   overviewPage ─── table.Model
        │     ├── [viewGate]       gatePage ─── checklist + artifact preview
        │     ├── [viewNodeDetail] detailPage ─── viewport + node list
        │     ├── [viewLogs]       logsPage ─── viewport + file tabs
        │     └── [viewDone]       donePage ─── summary report
        ├── overlays[]
        │     ├── ConfirmDialog    (gate decisions, quit, delete workspace)
        │     ├── helpOverlay      (context-sensitive keyboard help)
        │     └── RunConfigOverlay (新增：重跑配置)
        ├── focusManager
        ├── toast
        └── statusBar
```

### 6.5 导航拓扑

```
                    ┌──────────┐
                    │  viewHub │ ← 中枢（启动默认页）
                    └────┬─────┘
           ┌─────────────┼─────────────┐
           │ Enter       │ Ctrl+N      │ Ctrl+R (RunConfigOverlay)
           ▼             ▼             │
     ┌──────────┐  ┌──────────┐       │
     │ Resume?  │  │viewStart │       │
     │ 或直接进  │  │(预填配置) │       │
     │ Overview │  └────┬─────┘       │
     └────┬─────┘       │             │
          │             │ Enter       │
          ▼             ▼             │
     ┌──────────┐  ┌──────────┐      │
     │Overview  │  │Overview  │◄─────┘
     └────┬─────┘  └────┬─────┘
          │              │
    ┌─────┼──────┐       │
    ▼     ▼      ▼       │
  Gate Detail Logs      │
    │     │      │       │
    └─────┴──────┘       │
          │              │
          ▼              ▼
     ┌──────────┐  ┌──────────┐
     │ viewDone │  │ viewDone │
     │[Esc 中枢]│  │[Esc 中枢]│
     │[Ctrl+R]  │  │[Ctrl+R]  │
     └──────────┘  └──────────┘
```

- **Esc**：Done/Gate/Detail/Logs → Overview → Hub（逐级回退）
- **Esc（在 Start 中）**：→ Hub（放弃新建）
- **数字键 1-5**：快速跳转页面（1=Hub, 2=Overview, 3=Gate, 4=Detail, 5=Logs）

---

## 7. 详细页面设计

### 7.1 工作区 Hub 页（`workspace_hub.go`）

**布局：**
```
┌─ Harbor 出题工坊 ───────── 工作区管理 ──────────────┐
│                                                       │
│  搜索: [________________]  / 关闭搜索                  │
│                                                       │
│  名称         状态    语言  类型  时间         大小    │
│  ─────────────────────────────────────────────────── │
│ ▸ harbor-calc  ✓ 成功  py   algo  07-11 14:32  156M  │
│   harbor-web   ✗ 失败  go   web   07-10 09:15  892M  │
│   task-003     ◌ 运行中  js  cli   07-12 10:01   45M  │
│   task-004     ⚷ 可恢复  cpp  lib  07-09 22:00  320M  │
│  ─────────────────────────────────────────────────── │
│  共 4 个工作区   磁盘占用 1.4G                         │
│                                                       │
│  ↑↓ 选择  Enter 打开  Ctrl+N 新建  Ctrl+R 重跑        │
│  Del 删除  s 排序  / 搜索  q 退出                     │
└───────────────────────────────────────────────────────┘
```

**排序支持：**
- `s` 循环切换排序列：名称 → 状态 → 时间 → 大小
- `S` 切换升序/降序
- 默认按时间降序（最新的在上面）

**搜索/过滤：**
- `/` 进入搜索模式
- 实时过滤（debounce 150ms）
- 匹配：任务名称、语言、类型、状态（中文：成功/失败/运行中/可恢复）
- `Esc` 退出搜索

**操作交互：**

| 操作 | 触发 | 行为 |
|------|------|------|
| 打开工作区 | `Enter` | 如果可恢复→弹出恢复选项；如果已完成→viewDone；如果运行中→viewOverview |
| 新建任务 | `Ctrl+N` | 进入 viewStart（空白表单） |
| 基于选中新建 | `Ctrl+N`（有选中） | 进入 viewStart（预填选中工作区的配置） |
| 重跑 | `Ctrl+R` | 弹出 RunConfigOverlay |
| 删除 | `Del` | 弹出确认对话框，确认后删除工作区目录 |
| 搜索 | `/` | 激活搜索输入框 |
| 退出 | `q` | 如果有运行中任务→确认对话框；否则直接退出 |

**恢复选项对话框（选中可恢复工作区时按 Enter）：**
```
┌─ 工作区可恢复 ───────────────────────┐
│                                        │
│  task-004 上次运行在 07-09 22:00 崩溃  │
│  已完成 12/25 节点                      │
│                                        │
│  [R] 恢复运行  从断点继续               │
│  [N] 新建运行  复制配置到新工作区       │
│  [V] 只读查看  不修改任何文件           │
│  [Esc] 取消                            │
│                                        │
│  Enter 确认  Esc 取消                  │
└────────────────────────────────────────┘
```

### 7.2 重跑配置覆盖层（`runconfig_overlay.go` — 新增）

参考 p2r_tui 的 `runconfig.go`，但简化为 Harbor Flow 的需求：

```
┌─ 重跑配置 ────────────────────────────┐
│                                         │
│  源工作区: harbor-web (go/web)          │
│  目标工作区: [harbor-web-retry-1_____]  │
│                                         │
│  □ 复用 Docker 验证结果                  │
│  □ 复用质量检查结果                      │
│  □ 复用相似度检查结果                    │
│  □ 复用 Harbor 运行结果                  │
│                                         │
│  □ 自动批准所有审查关卡 (无头模式)       │
│                                         │
│  Tab 切换字段  Space 开关  Enter 开始   │
│  Esc 取消                               │
└─────────────────────────────────────────┘
```

实现 `Overlay` 接口，ZIndex=50（低于 ConfirmDialog 的 100）。

### 7.3 改造 Done 页

在现有 `page_done.go` 底部增加操作栏：

```
┌─ 运行完成 ────────────────────────────┐
│                                         │
│  [现有摘要内容不变]                      │
│                                         │
│  ───────────────────────────────────── │
│  [Esc 返回工作区中枢] [Ctrl+R 重跑]     │
│  [Ctrl+N 基于此配置新建]                │
└─────────────────────────────────────────┘
```

### 7.4 改造 TUI 入口（`run.go`）

```
当前逻辑：
  1. 有 --workspace → 恢复/只读
  2. 有 --task --generate → initialModel
  3. 其他 → initialStartModel

改造后：
  1. 有 --workspace → 恢复/只读（保持）
  2. 有 --task --generate → 直接进入 viewOverview（跳过 Hub）
  3. 其他 → initialHubModel（扫描工作区，进入 Hub）
```

新增 `initialHubModel()`：
```go
func initialHubModel(ctx context.Context, cancel context.CancelFunc) model {
    return initModelComponents(model{
        ctx:    ctx,
        cancel: cancel,
        view:   viewHub,
        // 不创建 Runner
    })
}
```

### 7.5 工作区目录扫描

```go
// internal/tui/workspace_scanner.go

type WorkspaceScanner struct {
    RootDirs []string  // 默认 [".harbor-factory/workspaces", ".harbor-factory/workspace"]
}

func (s *WorkspaceScanner) Scan(ctx context.Context) ([]WorkspaceItem, error) {
    var items []WorkspaceItem
    for _, root := range s.RootDirs {
        entries, _ := os.ReadDir(root)
        for _, entry := range entries {
            if !entry.IsDir() { continue }
            wsPath := filepath.Join(root, entry.Name())
            ws, err := status.ReadWorkspace(wsPath)
            if err != nil || !ws.StatePresent { continue }
            size, _ := dirSize(wsPath)
            items = append(items, WorkspaceItem{
                Path:        wsPath,
                Status:      ws,
                SizeBytes:   size,
                IsActive:    ws.Active,
                IsResumable: ws.Resumable,
            })
        }
    }
    return items, nil
}
```

### 7.6 新增文件清单

**新增包 `internal/harbor/store/`：**

| 文件 | 职责 | 预估行数 |
|------|------|---------|
| `internal/harbor/store/db.go` | 打开/初始化 SQLite，migration | ~80 |
| `internal/harbor/store/task_store.go` | Task CRUD + Upsert | ~120 |
| `internal/harbor/store/run_store.go` | Run CRUD + Upsert + List/Search | ~180 |
| `internal/harbor/store/sync.go` | 文件系统扫描 + DB 同步 | ~130 |

**新增/修改 TUI 文件：**

| 文件 | 职责 | 预估行数 |
|------|------|---------|
| `internal/tui/workspace_hub.go` | HubPage 实现 Page 接口 | ~300 |
| `internal/tui/runconfig_overlay.go` | 重跑配置覆盖层 | ~200 |
| `internal/tui/workspace_resume.go` | 恢复选项对话框 | ~120 |
| `internal/app/workspace_clone.go` | 克隆 run_options.json 到新工作区 | ~60 |

**修改文件：**

| 文件 | 改动 |
|------|------|
| `internal/tui/model.go` | `viewMode` 加 `viewHub`；`model` struct 加 `hubItems`、`hubTable` 字段 |
| `internal/tui/render.go` | `bindPages()` 注册 hubPage |
| `internal/tui/keymap.go` | 扩展 `handleGlobalKey` 支持 Hub 页；更新 `cyclePage` |
| `internal/tui/page_done.go` | 底部加 Esc/Ctrl+R/Ctrl+N 操作栏 |
| `internal/tui/run.go` | 默认进入 Hub（而非 StartForm） |
| `internal/tui/localize.go` | 新增 Hub 页、恢复对话框、重跑配置的中文翻译 |
| `internal/tui/styles.go` | 新增 `resumableIcon`、`activeIcon` 等样式 |

---

## 8. 键位设计

### 8.1 全局键位

| 键位 | 功能 |
|------|------|
| `Ctrl+Q` / `q`（非表单中） | 退出程序 |
| `?` | 帮助覆盖层 |
| `Esc` | 逐级回退（Overlay → 当前页 → 上级页面 → Hub） |
| `1` | 工作区 Hub |
| `2` / `Ctrl+O` | 总览（运行中时） |
| `3` / `Ctrl+G` | 审查关卡（有活跃 Gate 时） |
| `4` / `Ctrl+D` | 节点详情 |
| `5` / `Ctrl+L` | 日志 |

### 8.2 Hub 页键位

| 键位 | 功能 |
|------|------|
| `↑` / `↓` 或 `j` / `k` | 上下移动光标 |
| `Enter` | 打开选中的工作区 |
| `Ctrl+N` | 新建任务（有选中则预填配置） |
| `Ctrl+R` | 重跑选中的工作区 |
| `Del` | 删除选中的工作区（需确认） |
| `/` | 搜索/过滤 |
| `s` | 切换排序列 |
| `S` | 切换升序/降序 |
| `Esc` | 退出搜索（搜索模式中）/ 无操作（浏览模式） |
| `q` | 退出程序 |

### 8.3 Done 页新增键位

| 键位 | 功能 |
|------|------|
| `Esc` | 返回工作区 Hub |
| `Ctrl+R` | 重跑此运行 |
| `Ctrl+N` | 基于此配置新建 |

### 8.4 上下文 Footer

**Hub 页：**
```
↑↓ 选择  Enter 打开  Ctrl+N 新建  Ctrl+R 重跑  Del 删除  s 排序  / 搜索  q 退出  ? 帮助
```

**Done 页（改造后）：**
```
Esc 返回工作区  Ctrl+R 重跑  Ctrl+N 新建  1 工作区  2 总览  4 详情  5 日志  q 退出  ? 帮助
```

**恢复选项对话框：**
```
R 恢复运行  N 新建运行  V 只读查看  Enter 确认  Esc 取消
```

**重跑配置覆盖层：**
```
Tab 切换  Space 开关  Enter 开始重跑  Esc 取消
```

---

## 9. 数据流

### 9.1 Hub 加载

```
TUI 启动
  └── main() → store.Open(".harbor-factory/harbor.db")
        ├── 创建 tasks/runs 表（如不存在）
        └── store.SyncFromFilesystem()
              ├── 扫描 .harbor-factory/workspaces/*
              ├── 对每个目录:
              │     ├── status.ReadWorkspace()  → status/run_id/...
              │     ├── nodes.LoadRunOptions()  → task_name/lang/type/...
              │     ├── dirSize()               → size_bytes
              │     ├── store.UpsertTask(task)   → tasks 表
              │     └── store.UpsertRun(run)     → runs 表
              └── 清理 DB 中已不存在的 workspace 记录

  └── initialHubModel(store)
        └── HubPage.Init()
              └── store.ListRuns(ctx, sort, filter)
                    ├── SELECT runs.*, tasks.*
                    │   FROM runs JOIN tasks ON runs.task_id = tasks.id
                    │   ORDER BY started_at DESC
                    └── 返回 []WorkspaceItem → 填充 table.Model rows
```

### 9.2 从 Hub 进入运行

```
HubPage: Enter 键
  └── 判断 WorkspaceItem.IsResumable
        ├── true  → 显示恢复选项对话框
        │             ├── "恢复" → 创建 Runner(从 run_options.json 恢复)
        │             │             → 切换到 viewOverview
        │             ├── "新建" → initialStartModel(预填配置)
        │             └── "只读" → initialWorkspaceModel(只读)
        └── false → 判断 Status.Status
                      ├── "running" → initialWorkspaceModel(只读快照)
                      ├── "succeeded"/"failed" → initialWorkspaceModel(查看结果)
                      └── 其他 → 直接进入 viewOverview
```

### 9.3 从 Done 返回 Hub

```
DonePage: Esc 键
  └── 清理当前 Runner（如果已结束）
  └── 释放工作区锁（如果持有）
  └── m.router.SwitchTo(viewHub)
  └── 触发 HubPage.Init() 重新扫描工作区
```

---

## 10. 实施路线图

### Phase 0：数据层（1-2 天）**← 新增**

**目标：SQLite 数据库就绪，文件系统同步可用。**

| 步骤 | 内容 | 文件 |
|------|------|------|
| 0.1 | 添加 `go-sqlite3` 依赖 | `go.mod` |
| 0.2 | 实现 `store.Open()` + schema migration | `internal/harbor/store/db.go` |
| 0.3 | 实现 `UpsertTask` / `UpsertRun` / `GetTaskByDir` / `GetRunByWorkspace` | `task_store.go`, `run_store.go` |
| 0.4 | 实现 `ListRuns` 联表查询（JOIN tasks）支持排序/过滤 | `run_store.go` |
| 0.5 | 实现 `SyncFromFilesystem`（全量文件系统 → DB 同步） | `sync.go` |
| 0.6 | 单元测试：CRUD + 同步 + 去重 | `store/*_test.go` |

### Phase 1：最小可用中枢（2-3 天）

**目标：用户打开 TUI 看到工作区列表，能选择进入，能从 Done 返回。**

| 步骤 | 内容 | 文件 |
|------|------|------|
| 1.1 | 新增 `viewHub` + `WorkspaceItem`（从 Store 读取） | `model.go` |
| 1.2 | 实现 `HubPage`（表格渲染 + 上下移动 + Store 驱动） | `workspace_hub.go` |
| 1.3 | 注册 Hub 页面到 router，默认启动进 Hub | `render.go`, `run.go` |
| 1.4 | Done 页新增 `Esc 返回中枢` 操作 | `page_done.go` |
| 1.5 | 恢复选项对话框（只读查看 + 恢复运行） | `workspace_resume.go` |
| 1.6 | 更新中文化和 Footer | `localize.go`, `keymap.go` |

### Phase 2：重跑和新建（2-3 天）

**目标：用户能重跑失败任务，能基于历史配置新建。**

| 步骤 | 内容 | 文件 |
|------|------|------|
| 2.1 | `RunConfigOverlay`（重跑配置覆盖层） | `runconfig_overlay.go` |
| 2.2 | `CloneRunnerOptions()` 从 `run_options.json` 克隆到新工作区 | `internal/app/workspace_clone.go` |
| 2.3 | `Ctrl+R` 重跑流程：配置 → 克隆 → Runner → 进入 Overview → 完成后 DB 更新 | `workspace_hub.go` |
| 2.4 | `Ctrl+N` 新建流程：进入 StartForm，有选中时预填 | `page_start.go`, `workspace_hub.go` |
| 2.5 | Done 页 `Ctrl+R` / `Ctrl+N` 操作 | `page_done.go` |
| 2.6 | 运行完成/失败时自动 UpsertRun 更新 DB | `app.go` (runnerDoneMsg 处理) |

### Phase 3：搜索、排序、删除（1-2 天）

**目标：工作区多了以后能高效管理。**

| 步骤 | 内容 | 文件 |
|------|------|------|
| 3.1 | `/` 搜索过滤 → `store.SearchRuns(query)` | `workspace_hub.go`, `run_store.go` |
| 3.2 | `s`/`S` 排序切换 → `store.ListRuns(sort, order)` | `workspace_hub.go` |
| 3.3 | `Del` 删除工作区 → 确认 → `os.RemoveAll` + `store.DeleteRun` | `workspace_hub.go`, `confirm.go` |
| 3.4 | 磁盘占用统计和显示（来自 DB `size_bytes` 字段） | `workspace_hub.go` |

### Phase 4：细节打磨（1-2 天）

**目标：体验完善。**

| 步骤 | 内容 | 文件 |
|------|------|------|
| 4.1 | Hub 页后台轮询：每 5s 刷新 `running` 状态的 runs | `workspace_hub.go` |
| 4.2 | Hub 页空状态提示（"暂无工作区，Ctrl+N 创建第一个"） | `workspace_hub.go` |
| 4.3 | 工作区根路径可配置（`--workspace-root` 标志 + DB 路径联动） | `cmd/tui.go` |
| 4.4 | 键盘导航 Tab 在 Hub 和运行视图间切换 | `keymap.go` |
| 4.5 | DB 重建命令：`harbor-factory tui --rescan` 强制全量同步 | `cmd/tui.go`

---

## 11. 风险与缓解

| 风险 | 概率 | 影响 | 缓解措施 |
|------|------|------|---------|
| SQLite 与文件系统不同步 | 中 | Hub 显示过期数据 | 每次 TUI 启动执行 SyncFromFilesystem；`--rescan` 强制重建 |
| `dirSize()` 在大工作区上慢 | 中 | 首次同步变慢 | 异步扫描，缓存到 DB `size_bytes`，后续增量更新 |
| DB 文件损坏 | 低 | Hub 页空白 | DB 损坏时自动重建（从文件系统完整恢复） |
| 多个 TUI 实例冲突 | 低 | 数据竞争 | 现有 `runlock` 机制已处理，Hub 页显示 Active 状态 |
| 工作区目录结构变更 | 中 | 扫描失败 | 向后兼容处理，`status.ReadWorkspace` 已有优雅降级 |
| 配置克隆时敏感信息泄露 | 低 | 安全风险 | `RunnerOptionsSnapshot` 已实现 secrets 脱敏 |
| `go-sqlite3` 需要 CGO | 低 | 交叉编译困难 | 使用 `modernc.org/sqlite`（纯 Go 实现，无需 CGO） |

---

## 12. 关键代码参考

### 12.1 现有基础设施（可直接调用）

```go
// 读取工作区状态 — 已存在
ws, err := status.ReadWorkspace("/path/to/workspace")
// ws.RunID, ws.Status, ws.Passed, ws.StartedAt, ws.FinishedAt
// ws.RunOptions.TaskName, ws.RunOptions.CodeLang, ws.RunOptions.TaskType
// ws.Resumable, ws.Active

// 读取工作区摘要 — 已存在
summary, err := domain.LoadRunSummary("/path/to/workspace/state.json")

// 读取运行配置 — 可通过 nodes 包路径访问
opts, err := nodes.LoadRunOptions("/path/to/workspace/run_options.json")

// 工作区锁检测 — 已存在
active, err := runlock.IsActive("/path/to/workspace")
```

### 12.2 p2r_tui 参考文件（在 `/tmp/p2r_tui/internal/tui/`）

| 参考文件 | 可借鉴内容 |
|---------|-----------|
| `taskboard.go:31-60` | Page 接口实现模式 |
| `overview.go:36-56` | 表格 + 搜索组合模式 |
| `runconfig.go:30-47` | 重跑配置结构体 |
| `keymap.go:622-640` | `handleEscape()` 逐级回退逻辑 |
| `keymap.go:829-875` | 上下文 Footer 渲染 |
| `render.go:366-391` | 确认对话框渲染 |
| `focus.go:11-53` | 焦点栈 Push/Pop（已对齐，可参考） |

---

## 附录 A：与 p2r_tui 的术语映射

| Harbor Flow | p2r_tui | 说明 |
|-------------|---------|------|
| 题目 (Task) | 任务 (Task) | `task.toml` 描述的基准测试题目 |
| 工作区/运行 (Workspace/Run) | 运行 (Run) | 一次完整的 Pipeline 执行 |
| TaskDir | Task.Path / GitURL | 题目在磁盘上的位置 |
| 工作区 Hub | 任务看板 (TaskBoard) | 题目+运行的管理入口 |
| `store.Task` | `model.Task` | DB 中的题目记录 |
| `store.Run` | `model.RunRecord` | DB 中的运行记录 |
| `store.SyncFromFilesystem` | `db.ProjectSearch` | 数据获取方式（文件同步 vs DB 查询） |
| `RunConfigOverlay` | `runConfig` | 重跑配置 |
| 恢复 (Resume) | 恢复 (Recovery) | 断点续跑 |
| 克隆运行 (Clone & Run) | Re-run (initial mode) | 复制配置重跑 |
| 题目生成 (--generate) | （无对应） | Harbor Flow 特有：从仓库 AI 生成题目 |

---

## 附录 B：文件变更汇总

```
新增包 internal/harbor/store/ (4 文件):
  internal/harbor/store/db.go           (~80 行)
  internal/harbor/store/task_store.go   (~120 行)
  internal/harbor/store/run_store.go    (~180 行)
  internal/harbor/store/sync.go         (~130 行)

新增 TUI 文件 (3):
  internal/tui/workspace_hub.go         (~300 行)
  internal/tui/runconfig_overlay.go     (~200 行)
  internal/tui/workspace_resume.go      (~120 行)

新增 App 文件 (1):
  internal/app/workspace_clone.go       (~60 行)

修改文件 (8):
  internal/tui/model.go                 +30 行 (viewHub, store 字段)
  internal/tui/render.go                +5 行 (注册 hubPage)
  internal/tui/keymap.go                +40 行 (Hub 页键位 + Footer)
  internal/tui/page_done.go             +30 行 (底部操作栏)
  internal/tui/run.go                   +20 行 (store.Open, 默认进 Hub)
  internal/tui/app.go                   +10 行 (runnerDoneMsg→store.UpsertRun)
  internal/tui/localize.go              +30 行 (新翻译)
  internal/tui/styles.go                +5 行 (新图标样式)
  go.mod                                +1 行 (modernc.org/sqlite)

总计: ~690 行新增代码 + ~170 行修改
```
