# Overview 分页与排序重构计划

Date: 2026-05-07
Status: Ralph-reviewed 二次修订版，待实现

## 0. Ralph 复核结论

本轮按 oh my codex Ralph 循环修订：读取现有契约，审查原计划漏洞，定位可落地修复，再把修复直接回写到本文档。复核对象包括 `internal/tui/app.go`、`internal/tui/keymap.go`、`internal/tui/viewmodel.go`、`internal/tui/layout.go`、`internal/db/store.go`、`internal/db/migrate.go` 和现有 TUI/DB 测试。

原草案的主要缺陷已经在本文修正：

1. **包循环风险**：原计划让 `internal/db` 直接使用 TUI 的 `SortMode`，会形成 `tui -> db -> tui` 的编译循环。修正为 DB 包定义查询排序契约，TUI 只做 UI 状态到 DB 查询的映射。
2. **DB 访问边界矛盾**：原计划说 `OverviewModel` 不直接访问 DB，但结构体又持有 `store` 和 `cfg`。修正为 `OverviewModel` 只持有 UI 状态，`app` 负责 DB、配置和异步命令。
3. **SQL 最新 run 语义回归**：原计划直接 JOIN `projects.last_run_id`，会和当前 `LatestRunForTask` 的语义不一致，也会受旧库中 stale `last_run_id` 影响。修正为用 latest-run CTE 按 `started_at/run_id` 解析每个 task 的最新 run。
4. **排序方向错误**：原计划的 status/verdict CASE 值配合 `DESC` 会把低优先级排到前面。修正为显式 rank，高 rank 默认 `DESC`。
5. **N+1 未真正消除**：原计划移除了部分二次查询，但 `buildOverviewItems` 仍需 `LatestRunForTask` 获取 `ArtifactRoot/StaticOnly`。修正为分页查询一次性返回概览渲染所需的最新 run 字段。
6. **异步结果乱序**：原计划只说旧请求“可以忽略”，但没有契约。修正为 `seq` 版本号，子模型只接受最新请求结果。
7. **页码越界处理不完整**：原计划收到越界页结果后只改 `current`，没有重新加载有效页。修正为 clamp 后再发一次加载请求。
8. **键位语义不可实现**：原计划用同一个 `s` 同时“切换排序模式”和“同模式反转方向”，状态机不明确。修正为 `s` 循环排序模式，`S` 反转当前方向。
9. **测试和文件清单不完整**：原计划把 `testhooks.go` 标为不变，但重构会直接影响测试钩子。修正为把测试钩子、DB 分页测试、乱序消息测试纳入实施范围。

本次二次 Ralph 复核又补齐以下实现级漏洞：

1. **搜索谓词过宽/过窄风险**：单个 `Text + Statuses/Verdicts/Stages` 结构无法表达 `TASK 通过` 这类多词搜索；如果全局 OR 会过宽，如果 Text AND 中文枚举会过窄。修正为按 term 表达搜索，term 之间 AND，term 内原始 LIKE 与枚举扩展 OR。
2. **COUNT 和分页结果快照不一致**：`COUNT` 和 page SELECT 若在不同自动提交语句里执行，刷新/写入并发时会出现 `total` 与 `items` 不一致。修正为 `ListProjectsPaginated` 在同一个只读事务内执行两条查询。
3. **CTE 作用域误用**：SQL CTE 只作用于紧随其后的单条语句，不能写一次 `WITH` 后连续执行 `COUNT` 和 page SELECT。修正为实现中复用同一段 base CTE 字符串，但分别 prepend 到两条 SQL。
4. **空 manual verdict 排序异常**：`COALESCE(lr.manual_verdict, 'unset')` 不会把空字符串归一为 `unset`，会导致旧数据排序和显示不一致。修正为 `COALESCE(NULLIF(lr.manual_verdict, ''), 'unset')`。
5. **详情刷新回归**：原流程只在 selected task 或 latest run 变化时刷新详情，会让同一 run 的 stage/finding 变化在 tick 刷新中滞后。修正为 overview load request/result 携带 `refreshDetail`，tick/scheduler/显式刷新接受结果后继续刷新详情。
6. **搜索输入查询风暴**：逐字输入直接发 SQL 会制造大量过期查询，`seq` 只能保护 UI，不能减少 DB 压力。修正为搜索文本变化使用短 debounce；排序、翻页、刷新仍立即加载。

## 1. 目标与非目标

目标：

1. 在 TUI 项目总览页实现 SQL 层真分页，使用 `LIMIT/OFFSET`，总数来自同一过滤条件下的 `COUNT`。
2. 支持按任务 ID、运行状态、严重程度、最近运行时间、人工判定排序。
3. 抽出 `OverviewModel` 子模型，隔离搜索、表格、排序、分页和选中态，减少 `app` 主模型的概览逻辑。
4. 保持现有证据信号可见性：状态、失败阶段、阻断/严重数量、人工判定、docs、cleanup、批次、最后运行、模式不能被分页重构删除。
5. 保持 `View()` 纯渲染，不在 `View()` 中做 DB 查询、文件读取或昂贵计算。

非目标：

1. 本轮不实现鼠标点击页码。
2. 本轮不新增 DB 字段保存 cleanup/docs/mode，也不把 cleanup/docs/mode 做成全局 SQL 搜索字段。
3. 本轮不改执行详情面板的信息架构，仅适配 selected task 的来源。

## 2. 当前代码契约

现状关键点：

1. `app` 当前持有 `projects`、`overviewItems`、`visibleRows`、`table`、`search`。
2. `reload()` 调 `store.ListProjects(ctx)`，随后 `buildOverviewItems(ctx, store, cfg, projects)`。
3. `ListProjects` 先扫 `projects`，再对每个 project 调 `LatestRunForTask`、`firstFailedStage`、`findingCounts`，存在 N+1 查询。
4. `buildOverviewItems` 又再次调 `LatestRunForTask`，用于 `runMode` 和 `cleanupStatus`，存在第二层 N+1。
5. 当前搜索在内存中基于 `overviewSearchText` 做过滤，支持中文本地化状态和人工判定。
6. `keymap.go` 顶层处理全局键，`focusSearch` 和 `focusOverviewTable` 直接操作 `m.search/m.table`。

重构必须保留这些用户可见行为：

1. 搜索框输入普通字符不触发全局 `q/m/s` 类命令。
2. 总览表格移动选中项后，执行详情能重新加载对应 task。
3. tick、scheduler notify、run submit 完成后仍会刷新总览。
4. `docs/preflight/cleanup` 信号在总览和详情中继续可见。

## 3. 修订后的架构边界

### 3.1 DB 包拥有查询契约

`internal/db` 新增查询类型，避免 DB 包引用 TUI 包：

```go
type ProjectSort string

const (
    ProjectSortTaskID   ProjectSort = "task_id"
    ProjectSortStatus   ProjectSort = "status"
    ProjectSortSeverity ProjectSort = "severity"
    ProjectSortLastRun  ProjectSort = "last_run"
    ProjectSortVerdict  ProjectSort = "verdict"
)

type ProjectSearch struct {
    Terms []ProjectSearchTerm
}

type ProjectSearchTerm struct {
    Text         string
    Statuses     []string
    Verdicts     []string
    FailedStages []string
}

type ProjectQuery struct {
    Sort   ProjectSort
    Asc    bool
    Search ProjectSearch
    Limit  int
    Offset int
}
```

`ProjectSummary` 增加概览页构建所需字段，保证 `buildOverviewItems` 不再为每一行查询 latest run：

```go
type ProjectSummary struct {
    TaskID        string
    Batch         string
    Path          string
    RunCount      int
    LastRunID     string
    LastRunAt     string
    RunStatus     string
    ManualVerdict string
    FailedStage   string
    Blocking      int
    High          int

    LatestArtifactRoot string
    LatestStaticOnly   bool
}
```

`latest_static_only` 在 SQL 中是 0/1 整数；实现扫描时用局部 `int` 变量接收后再赋给 `ProjectSummary.LatestStaticOnly`，不要直接把 SQLite integer 扫进 bool，避免 driver 转换差异。

新增方法：

```go
func (s *Store) ListProjectsPaginated(ctx context.Context, q ProjectQuery) ([]ProjectSummary, int, error)
```

保留 `ListProjects(ctx)`，但不要继续保留旧 N+1 实现。抽出 private 查询 helper，让 `ListProjects` 使用不带 `LIMIT/OFFSET` 的同一条 summary 查询路径，`ListProjectsPaginated` 使用带 clamp 的分页路径。这样旧测试和未来非 TUI 调用也能获得一致的 latest-run、failed-stage、finding-count、artifact 字段语义。

### 3.2 TUI 子模型只拥有 UI 状态

`OverviewModel` 不持有 `store` 和 `cfg`：

```go
type overviewSortMode int

const (
    sortByTaskID overviewSortMode = iota
    sortByStatus
    sortBySeverity
    sortByLastRun
    sortByVerdict
)

type PageState struct {
    current  int
    size     int
    total    int
    autoSize bool
}

type OverviewModel struct {
    search   textinput.Model
    table    table.Model
    sortMode overviewSortMode
    sortAsc  bool
    page     PageState

    items      []overviewItem
    selectedID string

    loading   bool
    err       error
    seq       uint64
    searchSeq uint64

    width  int
    height int
}
```

`OverviewModel` 不需要实现 Bubble Tea 的 `tea.Model` 接口；它作为 app 内部子模型使用 typed update 即可：

```go
func newOverviewModel() OverviewModel
func (m OverviewModel) Init() tea.Cmd
func (m OverviewModel) Update(msg tea.Msg) (OverviewModel, tea.Cmd)
func (m OverviewModel) View() string
func (m *OverviewModel) SetFocus(focusArea)
func (m *OverviewModel) SetSize(width, height int)
func (m OverviewModel) SelectedTaskID() string
func (m OverviewModel) SelectedItem() (overviewItem, bool)
```

`SetSize(width, height)` 接收的是 overview 内容宽度和表格可用高度，不是整个终端尺寸；`app.applyLayout()` 负责把 `layout.contentWidth` 和 `layout.overviewTableHeight` 传进去。

`app.selectedTaskID()` 改为返回 `m.overview.SelectedTaskID()`。

### 3.3 app 仍负责 DB、配置与全局状态

`app` 改为：

```go
type app struct {
    store     *db.Store
    cfg       config.Config
    scheduler *scheduler.Scheduler

    overview OverviewModel
    detail   viewport.Model

    tab   int
    focus focusArea

    selectedStageKey string
    selectedRefRunID string
    stageIndex       int
    refIndex         int

    width  int
    height int

    message    string
    pendingJob string
    qaMode     string
    runConfig  runConfig
    activeJobs []scheduler.JobSnapshot

    detailVM       executionViewModel
    detailContent  string
    lastRecoveryAt time.Time
}
```

`setFocus` 保留在 app 中，但 overview 相关焦点同步给子模型：

```go
func (m *app) setFocus(area focusArea) {
    m.focus = area
    m.overview.SetFocus(area)
}
```

详情面板焦点仍由 app 管理。

## 4. 消息流与乱序保护

### 4.1 消息类型

```go
type overviewLoadRequestMsg struct {
    seq           uint64
    query         db.ProjectQuery
    cursorIntent  overviewCursorIntent
    silent        bool
    refreshDetail bool
}

type overviewLoadResultMsg struct {
    seq           uint64
    query         db.ProjectQuery
    items         []overviewItem
    total         int
    refreshDetail bool
    err           error
}

type overviewRefreshMsg struct {
    silent        bool
    refreshDetail bool
}

type overviewSearchDebounceMsg struct {
    searchSeq uint64
    text      string
}

type overviewCursorIntent int

const (
    cursorKeep overviewCursorIntent = iota
    cursorFirst
    cursorLast
)
```

`seq` 由 `OverviewModel` 增加并写入请求。收到 result 时，只有 `result.seq == m.seq` 才能更新 UI；旧结果直接丢弃。`searchSeq` 只保护搜索 debounce，不能代替 load `seq`。

### 4.2 正常加载流程

```text
OverviewModel.Update(排序/翻页/resize/refresh 或搜索 debounce 命中)
  -> 更新 sort/page/search 状态
  -> seq++
  -> 返回 overviewLoadRequestMsg cmd

app.Update 收到 overviewLoadRequestMsg
  -> handleOverviewLoad(req)
  -> 用带 timeout 的 context 调 store.ListProjectsPaginated(ctx, req.query)
  -> buildOverviewItems(cfg, projects)
  -> 返回 overviewLoadResultMsg

app.Update 收到 overviewLoadResultMsg
  -> before := m.selectedTaskID()
  -> beforeItem := m.overview.SelectedItem()
  -> m.overview.Update(result)
  -> after := m.selectedTaskID()
  -> 如果 selected task 改变，或当前 selected row 的 detail key 变化，或 result.refreshDetail=true，则 reloadDetail()
```

子模型不能直接把 `overviewLoadRequestMsg` 作为返回值返回给 app；必须通过 `tea.Cmd` 返回：

```go
func overviewLoadCmd(req overviewLoadRequestMsg) tea.Cmd {
    return func() tea.Msg { return req }
}
```

详情刷新 key 至少包含 `TaskID`、`LastRunID`、`RunStatus`、`ManualVerdict`、`FailedStage`、`Blocking`、`High`。这样同一 run 仍在写 stage/finding 时，tick/scheduler refresh 不会被误判为“无需刷新详情”。

### 4.3 页码越界重载

如果一次加载返回后发现 `page.current > totalPages()`：

1. `OverviewModel` 把 `page.current` clamp 到最后一页，空结果则保持 `current=1`。
2. 若 clamp 后页码和 result 对应页码不同，立即发出新的 `overviewLoadRequestMsg`。
3. 新请求使用新的 `seq`，旧 result 不再影响 UI。

这样避免“删除数据后停在空的越界页”。

### 4.4 搜索 debounce 与查询超时

搜索框普通字符更新后不立刻发 SQL；`OverviewModel` 递增 `searchSeq`，返回一个约 120ms 的 `tea.Tick` cmd。tick 返回 `overviewSearchDebounceMsg{searchSeq, text}` 后，只有 `searchSeq == m.searchSeq` 且 text 仍等于当前搜索框内容时才重置到第 1 页并发起 load。排序、翻页、窗口尺寸变化、tick refresh、scheduler notify 不走 debounce。

`handleOverviewLoad` 中的 DB 查询必须使用超时 context，例如 5s。超时结果按正常 error result 返回；若此时 UI 已产生更大的 `seq`，仍由 `OverviewModel` 丢弃。不要用 `writeMu` 包裹读查询，读路径依赖 SQLite 自身快照和 busy timeout。

## 5. SQL 查询设计

### 5.1 查询原则

1. 每个 project 在中间结果中只能出现一行，`COUNT` 才可靠。
2. 最新 run 以 `runs.task_id` 分组计算，不依赖可能 stale 的 `projects.last_run_id`。
3. `LastRunAt` 和现有 `ListProjects` 保持一致：优先 `finished_at`，其次 `started_at`，最后 `projects.last_run_at`。
4. `FailedStage` 使用和阶段展示一致的 A-F 顺序。
5. `findings` 统计只计算最新 run 的 `Blocker` 和 `High`。
6. SQL 动态部分只允许白名单 ORDER BY；搜索值、limit、offset 全部走参数。
7. `COUNT` 和 page SELECT 必须在同一个只读事务里执行，保证 `total/items` 来自同一快照。

### 5.2 CTE 形态

示意 SQL：

```sql
WITH latest_run AS (
    SELECT *
    FROM (
        SELECT r.*,
               ROW_NUMBER() OVER (
                   PARTITION BY r.task_id
                   ORDER BY COALESCE(r.started_at, '') DESC, r.run_id DESC
               ) AS rn
        FROM runs r
    ) ranked_runs
    WHERE rn = 1
),
failed_stage AS (
    SELECT run_id, stage
    FROM (
        SELECT s.run_id,
               s.stage,
               ROW_NUMBER() OVER (
                   PARTITION BY s.run_id
                   ORDER BY CASE s.stage
                       WHEN 'A' THEN 1 WHEN 'B' THEN 2 WHEN 'C' THEN 3
                       WHEN 'D' THEN 4 WHEN 'E' THEN 5 WHEN 'F' THEN 6
                       ELSE 99 END, s.stage
               ) AS rn
        FROM run_stages s
        WHERE s.status IN ('failed', 'blocked')
    ) ranked_stages
    WHERE rn = 1
),
finding_counts AS (
    SELECT run_id,
           SUM(CASE WHEN severity = 'Blocker' THEN 1 ELSE 0 END) AS blocking,
           SUM(CASE WHEN severity = 'High' THEN 1 ELSE 0 END) AS high
    FROM findings
    GROUP BY run_id
),
project_rows AS (
    SELECT p.task_id,
           p.batch,
           p.path,
           p.run_count,
           COALESCE(lr.run_id, '') AS last_run_id,
           COALESCE(NULLIF(lr.finished_at, ''), NULLIF(lr.started_at, ''), NULLIF(p.last_run_at, ''), '') AS last_run_at,
           COALESCE(lr.status, '') AS run_status,
           COALESCE(NULLIF(lr.manual_verdict, ''), 'unset') AS manual_verdict,
           CASE COALESCE(lr.status, '')
               WHEN 'running' THEN 50
               WHEN 'crashed' THEN 40
               WHEN 'completed_with_findings' THEN 30
               WHEN 'aborted' THEN 20
               WHEN 'completed_clean' THEN 10
               ELSE 0
           END AS status_rank,
           CASE COALESCE(NULLIF(lr.manual_verdict, ''), 'unset')
               WHEN 'fail' THEN 40
               WHEN 'rework' THEN 30
               WHEN 'unset' THEN 20
               WHEN 'pass' THEN 10
               ELSE 0
           END AS verdict_rank,
           COALESCE(fs.stage, '') AS failed_stage,
           COALESCE(fc.blocking, 0) AS blocking,
           COALESCE(fc.high, 0) AS high,
           COALESCE(lr.artifact_root, '') AS latest_artifact_root,
           COALESCE(lr.static_only, 0) AS latest_static_only
    FROM projects p
    LEFT JOIN latest_run lr ON lr.task_id = p.task_id
    LEFT JOIN failed_stage fs ON fs.run_id = lr.run_id
    LEFT JOIN finding_counts fc ON fc.run_id = lr.run_id
    WHERE <search predicate>
)

-- count query, prepend the base WITH/project_rows block above:
SELECT COUNT(*) FROM project_rows;

-- page query, prepend the same base WITH/project_rows block above:
SELECT task_id,
       batch,
       path,
       run_count,
       last_run_id,
       last_run_at,
       run_status,
       manual_verdict,
       failed_stage,
       blocking,
       high,
       latest_artifact_root,
       latest_static_only
FROM project_rows
<safe order clause>
LIMIT ? OFFSET ?;
```

上面的 CTE 是共享形态示意。SQLite 的 `WITH` 只作用于下一条语句，因此实现时应构造 `baseProjectRowsSQL`，分别拼成 `WITH ... SELECT COUNT(*) FROM project_rows` 和 `WITH ... SELECT ... FROM project_rows <ORDER BY> LIMIT ? OFFSET ?` 两条 SQL；两条语句共用同一份 search predicate 和参数构造逻辑，并在同一个 read transaction 中执行。

现代 SQLite 支持 window function。若后续发现目标运行环境 SQLite 版本不支持 window function，回退方案是用 correlated subquery 先取 latest run id，但必须保持“按 task_id 最新 run”语义，不允许退回只 JOIN `projects.last_run_id`。

### 5.3 排序契约

默认排序：`task_id ASC`。

| UI 模式 | DB sort | 默认方向 | 主排序 | 稳定二级排序 |
|---|---|---:|---|---|
| 任务ID | `ProjectSortTaskID` | ASC | `task_id` | 无 |
| 状态 | `ProjectSortStatus` | DESC | `status_rank` | `task_id ASC` |
| 严重程度 | `ProjectSortSeverity` | DESC | `blocking`, `high` | `task_id ASC` |
| 最近运行 | `ProjectSortLastRun` | DESC | `last_run_at` | `task_id ASC` |
| 人工判定 | `ProjectSortVerdict` | DESC | `verdict_rank` | `task_id ASC` |

`status_rank` 和 `verdict_rank` 是 `project_rows` 的内部排序列，不扫描进 `ProjectSummary`。rank 定义：

```sql
CASE run_status
    WHEN 'running' THEN 50
    WHEN 'crashed' THEN 40
    WHEN 'completed_with_findings' THEN 30
    WHEN 'aborted' THEN 20
    WHEN 'completed_clean' THEN 10
    ELSE 0
END

CASE manual_verdict
    WHEN 'fail' THEN 40
    WHEN 'rework' THEN 30
    WHEN 'unset' THEN 20
    WHEN 'pass' THEN 10
    ELSE 0
END
```

严重程度排序必须优先 blocker，再 high，不能只用 `blocking + high`，否则 `1 blocker` 和 `1 high` 会被误判为同等风险：

```sql
ORDER BY blocking DESC, high DESC, task_id ASC
```

### 5.4 ORDER BY 白名单

`orderClause` 不拼用户输入，只使用 `ProjectSort` switch：

```go
func projectOrderClause(q ProjectQuery) string {
    dir := "ASC"
    if !q.Asc {
        dir = "DESC"
    }

    switch q.Sort {
    case ProjectSortStatus:
        return fmt.Sprintf("ORDER BY status_rank %s, task_id ASC", dir)
    case ProjectSortSeverity:
        return fmt.Sprintf("ORDER BY blocking %s, high %s, task_id ASC", dir, dir)
    case ProjectSortLastRun:
        return fmt.Sprintf("ORDER BY last_run_at %s, task_id ASC", dir)
    case ProjectSortVerdict:
        return fmt.Sprintf("ORDER BY verdict_rank %s, task_id ASC", dir)
    default:
        return fmt.Sprintf("ORDER BY task_id %s", dir)
    }
}
```

`ProjectQuery` 入库前必须 normalize：

1. `Sort` 不在白名单时回退 `ProjectSortTaskID`。
2. `Limit` 只允许 10/20/40/50；非法值回退 20。
3. `Offset < 0` 回退 0。
4. `Search.Terms` 最多保留 8 个 term，每个 term 的 `Text` trim 后最多 64 rune，避免异常长输入拖慢查询。
5. `Statuses`、`Verdicts`、`FailedStages` 全部按 DB 包白名单过滤并去重；空 term 丢弃。

### 5.5 搜索契约

SQL 搜索字段：

1. `task_id`
2. `batch`
3. `path`
4. `run_status`
5. `manual_verdict`
6. `failed_stage`

搜索框文本由 TUI 层按空白切成 term，并把中文本地化词扩展成 DB 原始值；DB 包不导入本地化逻辑。谓词组合规则：

1. term 之间使用 AND，保证 `TASK-1 通过` 表示“同时满足 TASK-1 和通过”。
2. 同一个 term 内，原始文本 LIKE、status filter、verdict filter、failed-stage filter 使用 OR，保证 `通过` 这类中文词不会因为 DB 原始列不含中文而匹配不到。
3. 搜索为空时 predicate 是 `1=1`。
4. 如果 term 的 `Text` 为空，只生成枚举 filter；如果枚举 filter 为空，只生成 LIKE 子句。

例如单个 term 的谓词形态：

```sql
(
    task_id LIKE ? ESCAPE '\'
    OR batch LIKE ? ESCAPE '\'
    OR path LIKE ? ESCAPE '\'
    OR run_status LIKE ? ESCAPE '\'
    OR manual_verdict LIKE ? ESCAPE '\'
    OR failed_stage LIKE ? ESCAPE '\'
    OR run_status IN (...)
    OR manual_verdict IN (...)
    OR failed_stage IN (...)
)
```

中文扩展示例：

| 输入包含 | 扩展 |
|---|---|
| `运行中` | `status IN ('running')` |
| `崩溃` | `status IN ('crashed')` |
| `有发现` | `status IN ('completed_with_findings')` |
| `已中止` / `中止` | `status IN ('aborted')` |
| `通过` | `status IN ('completed_clean') OR verdict IN ('pass')` |
| `未判定` | `verdict IN ('unset')` |
| `返工` | `verdict IN ('rework')` |
| `不通过` | `verdict IN ('fail')` |
| `A` 到 `F` 或 `阶段A` | `failed_stage IN (...)` |

LIKE 搜索必须转义 `%`、`_`、`\`，并使用 `ESCAPE '\'`。pattern 统一由 helper 生成，不在调用点手写：

```sql
task_id LIKE ? ESCAPE '\'
```

`不通过` 必须先于 `通过` 识别，避免同时扩展成 pass 和 fail。stage token 只接受单字母 `A-F`、`阶段A-F` 或本地化阶段名；普通长文本中包含字母 `A` 不应被误扩展成阶段过滤。

阶段本地化扩展要复用 `localizeStageName` 的标签语义并保留原有“输入局部中文也可命中”的体验，例如 `结构` 命中 A，`Docker` 命中 B，`测试` 可命中 C/D，`验收` 命中 E，`修复` 命中 F。若一个 term 命中多个阶段别名，同一 term 内用 `failed_stage IN (...)` 表达 OR。

本轮明确不把 `docsCount`、`cleanupStatus`、`mode` 纳入全局 SQL 搜索。它们仍在当前页展示，并仍可用于后续 denormalize 后的增强搜索。搜索框 placeholder 应调整为：

```text
搜索任务ID、批次、路径、状态、判定或阶段...
```

### 5.6 索引

分页和排序会让读路径更依赖索引。新增索引用 `CREATE INDEX IF NOT EXISTS`，并在迁移结束时统一确保存在；不只放在新库建表路径里。

建议索引：

```sql
CREATE INDEX IF NOT EXISTS idx_runs_task_started ON runs(task_id, started_at DESC, run_id DESC);
CREATE INDEX IF NOT EXISTS idx_runs_task_status ON runs(task_id, status);
CREATE INDEX IF NOT EXISTS idx_run_stages_run_status_stage ON run_stages(run_id, status, stage);
CREATE INDEX IF NOT EXISTS idx_findings_run_severity ON findings(run_id, severity);
CREATE INDEX IF NOT EXISTS idx_projects_batch ON projects(batch);
```

可以不提升 `currentSchemaVersion`，但 `migrate()` 必须保证所有路径都会执行 `ensureReadIndexes(ctx, tx)`。当前空库路径会在 `createCurrentSchema` 后提前 `tx.Commit()`，因此必须二选一：要么在 `createCurrentSchema` 末尾调用 `ensureReadIndexes`，要么移除空库分支的提前返回，让统一尾部执行索引确保逻辑。旧库、当前版本库和空库都要有测试覆盖。

## 6. 分页状态

```go
func (ps PageState) totalPages() int {
    size := normalizePageSize(ps.size)
    if ps.total <= 0 {
        return 1
    }
    return (ps.total + size - 1) / size
}

func (ps PageState) currentPage() int {
    return clamp(ps.current, 1, ps.totalPages())
}

func (ps PageState) offset() int {
    return (ps.currentPage() - 1) * normalizePageSize(ps.size)
}

func (ps PageState) rangeStart() int {
    if ps.total <= 0 {
        return 0
    }
    return ps.offset() + 1
}

func (ps PageState) rangeEnd() int {
    if ps.total <= 0 {
        return 0
    }
    return min(ps.offset()+normalizePageSize(ps.size), ps.total)
}
```

空结果显示为：

```text
第 1/1 页  0-0/0
```

不要让 `page.current` 变成 0；显示层用 `0-0/0` 表达空集合。

所有会读取 `page.size` 的方法都必须先走 `normalizePageSize`，避免测试注入或未来状态迁移造成 `size=0` 时除零。

### 6.1 动态页大小

页大小预设：10、20、40、50。

```go
func computePageSize(termHeight int, auto bool, current int) int {
    if !auto {
        return normalizePageSize(current)
    }
    available := termHeight - 11
    switch {
    case available >= 50:
        return 50
    case available >= 40:
        return 40
    case available >= 20:
        return 20
    default:
        return 10
    }
}
```

`z` 手动切换页大小后，`page.autoSize=false`，只在当前终端尺寸下固定。收到 `tea.WindowSizeMsg` 后恢复 `page.autoSize=true` 并重新计算页大小；若页大小变化，重置到第 1 页并重新加载。

## 7. 键位设计

总览局部键只在 `focusSearch` 或 `focusOverviewTable` 生效。全局 `ctrl+c/ctrl+q/ctrl+r/tab/shift+tab/esc` 仍由 app 顶层优先处理，run config 打开时继续由 `runconfig.go` 独占输入。

| 焦点 | 键位 | 行为 |
|---|---|---|
| 搜索框 | 普通字符 | 更新搜索文本，启动 debounce；debounce 命中后 `page.current=1` 并发起加载 |
| 搜索框 | `enter` / `down` | 转到 overview table |
| 总览表格 | `s` | 循环排序模式：taskID -> status -> severity -> lastRun -> verdict -> taskID，并使用该模式默认方向 |
| 总览表格 | `S` | 反转当前排序方向，不切换排序模式 |
| 总览表格 | `pgdown` | 下一页，cursor 放到第一页行 |
| 总览表格 | `pgup` | 上一页，cursor 放到最后一行 |
| 总览表格 | `z` | 页大小 10 -> 20 -> 40 -> 50 -> 10，固定当前尺寸下的手动页大小 |
| 总览表格 | `up` | 当前页内上移；在第一行继续按时翻上一页并选最后一行 |
| 总览表格 | `down` | 当前页内下移；在最后一行继续按时翻下一页并选第一行 |
| 总览表格 | `/` | 回到搜索框 |
| 总览表格 | `enter` | 进入执行详情 |
| 总览表格 | `m` / `q` | 保持现有行为：切换模式 / 回搜索框 |

不要使用 `ctrl+s` 作为反转方向键，很多终端会把它当作 XOFF 流控。

边界页行为必须是 no-op：第 1 页第一行继续按 `up` 不发加载；最后一页最后一行继续按 `down` 不发加载；`pgup` 在第 1 页、`pgdown` 在最后一页也不发加载。

## 8. UI 渲染

### 8.1 分页栏

分页栏放在表格下方、footer 上方，占 1 行。

宽度 >= 72：

```text
上页 PgUp  第 3/12 页  41-60/230  下页 PgDn  排序: 状态↓  条数: 20
```

宽度 < 72：

```text
PgUp  3/12 41-60/230  PgDn  状态↓ 20
```

页码不可点击。当前不实现 `[1] [2] ...` 页码列表，避免在 TUI 中占用横向空间；若未来需要页码列表，应作为第二阶段增强。

### 8.2 表头排序指示

`overviewColumnSpecs` 增加排序参数：

```go
func overviewColumnSpecs(width int, sort overviewSortMode, asc bool) []overviewColumnSpec
func buildOverviewColumns(width int, sort overviewSortMode, asc bool) []table.Column
```

排序列 title 追加 `↑` 或 `↓`。如果窄屏隐藏了排序列，分页栏仍显示当前排序状态。

### 8.3 Footer

`focusOverviewTable` footer：

```text
↑↓选择 Enter详情 /搜索 s排序 S反向 PgUp/PgDn翻页 z条数 Ctrl+R重跑 m模式
```

宽度不足时用现有断点策略截断，但必须保留翻页和排序提示之一。

## 9. 文件变更清单

| 文件 | 操作 | 内容 |
|---|---|---|
| `internal/db/store.go` | 修改 | 新增 `ProjectSort`、`ProjectSearchTerm`、`ProjectSearch`、`ProjectQuery`、`ListProjectsPaginated`、只读事务、查询 normalize、ORDER BY 白名单 |
| `internal/db/migrate.go` | 修改 | 新增 `ensureReadIndexes`，迁移结束时确保分页读索引存在 |
| `internal/tui/overview.go` | 新增 | `OverviewModel`、分页状态、排序状态、搜索 term 扩展、debounce、分页栏渲染、子模型 Update |
| `internal/tui/app.go` | 修改 | 用 `overview OverviewModel` 替代概览字段；处理 overview request/result；selected task 代理到子模型 |
| `internal/tui/keymap.go` | 修改 | 全局键保留，overview 局部键委托给 `OverviewModel`；详情焦点逻辑保持 |
| `internal/tui/render.go` | 修改 | `renderOverview(m app)` 调用 `m.overview.View()` |
| `internal/tui/viewmodel.go` | 修改 | `buildOverviewItems(cfg, projects)` 不再接收 store/ctx；使用 `ProjectSummary` 中的 latest run 字段 |
| `internal/tui/layout.go` | 修改 | `overviewColumnSpecs`/`buildOverviewColumns` 增加排序指示参数，保留原宽度断点 |
| `internal/tui/testhooks.go` | 修改 | harness 通过 `OverviewModel` 读写搜索、可见行、选中项、分页和排序状态 |
| `tests/internal/db/store_test.go` | 修改 | 增加分页、排序、搜索、latest-run fallback、索引迁移测试 |
| `tests/internal/tui/*.go` | 修改 | 增加 overview 分页/排序/乱序消息/空结果测试，更新现有 harness 断言 |

## 10. 实施顺序

### Phase 1：DB 查询契约

1. 在 `internal/db/store.go` 定义 `ProjectSort`、`ProjectSearchTerm`、`ProjectSearch`、`ProjectQuery`。
2. 实现 `normalizeProjectQuery`、`projectOrderClause`、LIKE escape、term predicate builder。
3. 实现 `ListProjectsPaginated`，使用 latest-run CTE 和一行一 project 的 `project_rows`；`COUNT` 与 page SELECT 在同一个只读事务内执行。
4. 增加 `ProjectSummary.LatestArtifactRoot` 和 `ProjectSummary.LatestStaticOnly`。
5. 抽出 private summary 查询 helper，让旧 `ListProjects` 复用非分页路径并消除 N+1。
6. 增加 `ensureReadIndexes` 并确保空库、旧库、当前版本库迁移路径都会调用。

### Phase 2：OverviewModel

1. 新建 `internal/tui/overview.go`。
2. 迁移 search/table/items/visibleRows/selectedID 到子模型。
3. 实现 `requestLoad(silent, cursorIntent)`，内部递增 `seq`。
4. 实现 search 中文扩展到 `db.ProjectSearchTerm`，并补搜索 debounce。
5. 实现分页栏、排序标签和空状态。

### Phase 3：app 集成

1. `newApp` 初始化 `overview := newOverviewModel()`。
2. `Init()` 使用 `m.overview.Init()` 触发首次加载。
3. `reload()` 改为发出 `overviewRefreshMsg{silent:true, refreshDetail:true}` 或直接委托 `m.overview.RefreshCmd(true, true)`。
4. `Update` 处理 `overviewLoadRequestMsg` 和 `overviewLoadResultMsg`。
5. `selectedTaskID()`、`setFocus()`、`applyLayout()` 改为代理到子模型，`applyLayout()` 调用 `overview.SetSize(...)`。

### Phase 4：键位和布局

1. `handleKey` 保留全局键优先级。
2. 当 `tab == panelOverview` 且焦点属于 overview 时，把局部 key 交给 `OverviewModel.Update`。
3. 根据 before/after selected task、detail key 和 `refreshDetail` 决定是否 reload detail。
4. 更新 footer 和 layout 宽度断点。

### Phase 5：测试验证

1. 先跑 DB 单测：`go test ./tests/internal/db -run 'Paginated|ListProjects|Migrates'`。
2. 再跑 TUI 单测：`go test ./tests/internal/tui`。
3. 最终跑 `go test ./...`、`go vet ./...`、`go build ./...`。

## 11. 必补测试

DB 测试：

1. `ListProjectsPaginated` 返回正确 `items` 和 `total`。
2. limit/offset 生效，非法 limit 被 clamp。
3. status 默认排序顺序：running、crashed、completed_with_findings、aborted、completed_clean、无 run。
4. severity 排序优先 blocker，再 high。
5. lastRun 使用最新 run 的 finished/started 时间，不受 stale `projects.last_run_id` 影响。
6. verdict 默认排序顺序：fail、rework、unset、pass。
7. 中文搜索扩展后的 status/verdict/stage filter 能命中。
8. `%`、`_`、`\` 搜索不会扩大匹配范围。
9. 多 term 搜索使用 AND，单 term 内 LIKE 与枚举 filter 使用 OR。
10. `manual_verdict=''` 被当作 `unset` 显示和排序。
11. `COUNT` 和 page SELECT 在同一只读事务内执行；可用 hook/mock 或并发写入测试证明 `total/items` 不撕裂。
12. 旧库、当前版本库、空库 migrate 后索引都存在。
13. 小 fixture 下 `ListProjects(ctx)` 与 `ListProjectsPaginated(ctx, Limit:50, Offset:0)` 的 summary 语义一致；实现上通过 private helper 消除 per-project latest-run 查询。

TUI 测试：

1. 搜索输入只更新 debounce token；旧 debounce 消息不会发起加载，最新 debounce 命中后重置到第 1 页并发起加载。
2. `s` 循环排序模式，`S` 只反转方向。
3. `pgdown` 下一页，`pgup` 上一页。
4. 第一行按 `up` 翻上一页并选最后一行；最后一行按 `down` 翻下一页并选第一行。
5. 空结果显示 `0-0/0`，但 `page.current` 保持 1。
6. 返回旧 `seq` 的 result 不覆盖当前 items。
7. result clamp 页码后会发起第二次有效页加载。
8. selected task 变化时 app 触发 detail reload；`refreshDetail=true` 时即使 selected task 未变化也刷新详情。
9. `q/m/s/S/z` 在搜索框焦点下都是普通输入，不触发表格命令。
10. 首页/末页边界按 `up/down/pgup/pgdown` 不产生无效加载。
11. footer 在 overview table 焦点包含排序和翻页提示。

## 12. 边缘情况

| 场景 | 行为 |
|---|---|
| 搜索结果为空 | 表格为空，分页栏显示 `第 1/1 页 0-0/0`，selected task 清空 |
| 数据刷新后当前页越界 | clamp 到最后一页并重新加载该页 |
| 当前 selected task 不在新页 | 按 cursor intent 选择第一行/最后一行；默认选择第一行 |
| 旧异步结果晚到 | `seq` 不匹配，直接忽略 |
| 旧搜索 debounce 晚到 | `searchSeq` 或 text 不匹配，直接忽略，不发 DB 请求 |
| store 为 nil 的测试 harness | `OverviewModel` 可用 seed 数据；实际加载 cmd 返回空结果或测试专用注入 |
| 终端高度极小 | SQL page size 仍最小 10，table 使用自身滚动能力 |
| 排序列在窄屏隐藏 | 表头不显示箭头，但分页栏显示当前排序 |
| DB 查询失败 | 保留旧 items，分页栏显示旧状态，message 显示错误 |
| 搜索包含通配符 | `%`、`_` 被当作普通字符 |
| 搜索 `TASK-1 通过` | 两个 term AND；不会退化为“TASK-1 或通过” |
| 同一 run 的 stage/finding 更新 | tick/scheduler refresh 接受结果后通过 `refreshDetail` 重新加载详情 |
| 查询超时 | 显示错误消息；若已有更新的 `seq`，超时结果被忽略 |

## 13. 验收标准

1. 编译无包循环。
2. `go test ./...` 通过。
3. `go vet ./...` 通过。
4. `go build ./...` 通过。
5. 大量项目时总览每次只构建当前页 rows，不再为全部项目构建 `overviewItem`。
6. 刷新、排序、搜索、翻页连续操作时不会出现旧结果覆盖新结果。
7. View 层无 DB 查询、文件读取和分页状态突变。
8. 搜索、多词中文扩展、空 manual verdict、COUNT/page 快照一致性都有回归测试。

## 14. 明确不变项

1. 执行详情面板的信息内容保持不变。
2. `pipelineview.go`、`runconfig.go`、`stage_plan.go`、`filepicker.go` 的业务逻辑保持不变。
3. `localize.go` 可保持不变；中文搜索反向映射放在新的 overview 搜索辅助中。
4. 全局 `ctrl+c`、`ctrl+q`、`ctrl+r`、`tab`、`shift+tab`、`esc` 语义保持不变。
5. `q` 不恢复为全局强制退出键。
