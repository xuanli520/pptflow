# TUI 质检工作流重构方案设计（实施版）

> **修订记录**：本文档经 10 轮多维度审查（架构/数据模型/状态机/Pipeline/Git/TUI/Docker/安全/实施计划/综合），修复了 5 个阻断性缺陷、30+ 个高严重度缺陷、40+ 个中低严重度缺陷。修订版可直接作为实施依据。

## 背景

当前 p2r_tui 质检流水线是一个**全自动、无人交互**的过程。题目（交付包）通过文件系统扫描发现，A→B→C→D→F 五阶段自动执行（Stage E 当前被 `defaultRunStages()` 显式排除），Docker 运行时在 Stage C 完成后自动清理。质检员无法在流水线过程中介入、无法手动验证系统功能、无法直观管理多道题目的质检状态。

本轮重构目标：

1. 将质检题目状态管理从"运行状态"重构为**面向质检员的三个阶段**，让质检员一目了然地掌握每道题的质检进度。
2. 引入 **Git 集成**，支持从 GitLab 自动拉取题目仓库，初次 clone、再次强制同步。
3. 引入**人工交互环节**——Docker 启动后保持运行，质检员手动访问系统测试后回到 TUI 确认，再进入完成状态。
4. 调整 Pipeline Stage 顺序，将 BC（运行时验证）移到 F（标注员修复报告）之后，并将 Stage E（静态验收审计）纳入默认流水线。
5. 记录每道题的质检完成次数。
6. 对涉及模块进行重构，符合软件工程最佳实践。

本方案仅描述设计，不在本文档落地代码。

---

## 核心概念变化

### 旧模型

```
scanner 扫描 projects-qa/ → 发现 package → 自动运行 pipeline → 完成
```

题目是被动发现的，状态由 pipeline 的运行状态间接表达。

### 新模型

```
质检员输入 TASK ID → 系统 clone + 自动运行 A→D→E→F→B→C → 等待人工测试 → 确认完成
```

题目由质检员主动创建，状态是题目的第一类属性，pipeline 是题目的附属执行单元。

### 双系统共存策略

task 驱动的题目和文件系统扫描发现的旧项目**暂时共存**：

- task 创建时同步写入 `projects` 表（保持内部引用完整性）
- 仅扫描发现的项目在 TaskBoard 中不显示（无对应 `tasks` 行），仅在 Overview 表格中可见
- Overview 表格中，task 管理的行按 `tasks.state` 着色；扫描发现的行无 task 状态着色
- `Ctrl+R` 行为区分上下文：task 管理的项目执行 ReInspect（Git force-pull + 新 pipeline），扫描发现的项目显示历史运行配置面板
- 后续版本考虑完全移除文件系统扫描，统一为 task 驱动模型

---

## 零、TUI 页面架构重构

### 0.1 当前架构问题

当前 `app.go`（964 行）承担过多职责：页面路由、消息分发、状态同步、详情加载、调度器轮询全部耦合在一个结构体中。

### 0.2 新页面架构

```
┌──────────────────────────────────────────────────────────────────┐
│  P2R QA TUI    [题目管理] [总览]                    活跃批次: 003 │
│                                                     [设置 Ctrl+/]│  ← 页签栏
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│                    当前激活页面内容                                │
│                                                                  │
├──────────────────────────────────────────────────────────────────┤
│  [输入 TASK ID: ________________]                                │  ← 全局输入框（左下角）
│  /:聚焦输入框  Ctrl+E:确认  Ctrl+R:重检  Ctrl+O:总览  Ctrl+/:设置│  ← 全局页脚
│  Esc:取消聚焦  Ctrl+G:重试Git  Q:退出                            │
└──────────────────────────────────────────────────────────────────┘
```

#### 页面定义

| 页面 | 类型 | 位置 | 进入方式 | 说明 |
|------|------|------|---------|------|
| **题目管理** (TaskBoard) | 全屏页面 | 页签栏最左 | TUI 启动默认显示，Tab 切换 | 三栏题目状态管理视图 |
| **总览** (Overview) | 全屏页面 | 页签栏最右 | `Ctrl+O`，Tab 切换 | 历史项目查询，兼容扫描发现项目 |
| **设置** (Settings) | 模态浮层 | 右下角弹出 | `Ctrl+/` | Docker 镜像源配置，从全屏页签改为浮层 |

### 0.3 app 结构体重构

当前 `app` 结构体有 30+ 字段，职责混杂。重构后按关注点拆分：

```go
// app 主结构体 — 仅持有顶层协调字段
type app struct {
    // 基础设施（不可变，通过构造函数注入）
    store          appStore
    cfg            config.Config
    scheduler      schedulerClient
    recoverFn      func(context.Context) error

    // 服务层（由 app 构造并注入到各页面）
    taskQuerySvc   TaskQueryService   // 实现：基于 db.Store 的查询适配
    taskActionSvc  TaskActionService  // 实现：编排 Git + Pipeline + 状态变更

    // 页面管理
    router     *pageRouter

    // 子页面（通过构造函数注入服务）
    taskBoard  *TaskBoardModel
    overview   *OverviewModel
    settings   *SettingsOverlay

    // 全局组件
    taskInput  *TaskInputModel
    pipeline   *PipelineBarModel

    // 全局状态
    width      int
    height     int
    message    string

    // 调度器轮询（抽离为独立模块，注入服务）
    poller     *schedulerPoller
}
```

#### 拆出模块

| 新模块 | 从 app.go 拆出 | 职责 |
|--------|---------------|------|
| `internal/tui/router.go` | `switchPanel`、`handleEscape` 路由逻辑 | 页面注册、切换、生命周期管理、Overlay 管理 |
| `internal/tui/poller.go` | `tick`、`recoverStaleRunsCmd`、`reloadSchedulerJobs` | 调度器轮询、Docker 健康检查、`tasks` 表状态修复 |
| `internal/tui/pipelinebar.go` | `renderPipelineBar` + `activeJobs`/`pendingJob` 相关的消息处理 | 流水线状态栏渲染（保持为纯函数+snapshot） |
| `internal/tui/settings_overlay.go` | 从 `settings.go` + `docker_mirror.go` 改造 | 设置浮层 |
| `internal/tui/focus.go` | **新增** — 全局焦点管理 | FocusStack：Page → Column → Card → InputBox → Overlay 的焦点栈 |

### 0.4 页面路由与焦点模型

#### Page 接口

```go
type Page interface {
    Init() tea.Cmd
    Update(msg tea.Msg) (bool, tea.Cmd)   // bool: 消息是否被处理
    View(width, height int) string
    Focus()
    Blur()
    HandleKey(msg tea.KeyMsg) tea.Cmd      // 返回 nil 表示未处理，由全局键处理
    Destroy() tea.Cmd                      // 页面销毁时的资源释放
}
```

#### Overlay 接口

```go
type Overlay interface {
    Init() tea.Cmd
    Update(msg tea.Msg) (bool, tea.Cmd)
    View(width, height int) string         // 渲染浮层内容
    ZIndex() int                           // 堆叠层级
    InterceptsAllKeys() bool               // true = 捕获所有键盘事件
    Destroy() tea.Cmd
}
```

#### 焦点模型（FocusStack）

```go
type focusTarget int
const (
    focusInputBox focusTarget = iota   // 全局输入框聚焦
    focusPage                          // 页面内容聚焦（由 Page 内部管理子焦点）
    focusOverlay                       // 浮层聚焦
)

type focusManager struct {
    stack   []focusTarget
    current focusTarget
}
```

**焦点规则**：
- `/` 键：从任意位置聚焦输入框（push focusInputBox）
- `Esc`：输入框聚焦时清空内容并回到 page（pop focusInputBox）；浮层打开时关闭浮层（pop focusOverlay）
- 浮层打开时拦截所有键盘事件（`InterceptsAllKeys() = true`），仅 `Esc`/`Ctrl+/` 关闭浮层
- 输入框聚焦时：`Enter` → 触发 StartInspection，方向键 → 移动文本光标而非列切换，全局快捷键（`Ctrl+E`/`Q`/`Ctrl+O`）仍然有效

#### 键盘事件分发顺序

```
1. Overlay（如打开）→ 拦截所有键
2. 输入框聚焦 → 捕获可打印字符 + Enter + 方向键；全局快捷键透传
3. Page.HandleKey() → 页面级键处理
4. 全局快捷键（Ctrl+E, Ctrl+R, Q, Tab, Ctrl+O, Ctrl+/）
```

---

## 一、状态机设计

### 1.1 题目生命周期

```
                    ┌──────────────────────────────┐
                    │      pipeline aborted/crashed │
                    │      (clear current_run_id)   │
                    ▼                              │
              ┌──────────┐    pipeline     ┌──────────┐    Ctrl+E    ┌──────────┐
  输入TASK ID →│ 开始质检  │──────────────▶│  待处理   │────────────▶│ 结束质检  │
              └──────────┘    完成         └──────────┘   确认测试    └──────────┘
                    ▲                              │        完毕          │
                    │                              │                      │
                    │         Git sync 失败         │                      │
                    │    (停留在 inspecting，       │                      │
                    │     显示错误，允许 Ctrl+G)     │                      │
                    │                              │                      │
                    └──────────────────────────────┘                      │
                              Ctrl+R 重新质检                              │
                                                                          │
                                                     completion_count += 1 │
```

### 1.2 状态语义

| 状态 | 常量 | 含义 | 子状态/活动 |
|------|------|------|------------|
| **开始质检** | `inspecting` | 流水线正在准备或执行 | `syncing`（Git 同步中，无 current_run_id）→ `running`（Pipeline 执行中，有 current_run_id） |
| **待处理** | `waiting_manual` | Pipeline 已执行完毕，等待质检员手动确认 | Docker 运行中（docker_running=1）或 Docker 失败（docker_running=0） |
| **结束质检** | `completed` | 质检员确认测试完毕，Docker 已清理 | — |

### 1.3 关键路径：Stage B 成功 vs 失败

```
Stage F 完成
    │
    ├── Stage B (Docker 启动)
    │       │
    │       ├── 成功 ──▶ Stage C (run_tests.sh) ──▶ tasks.state = 'waiting_manual'
    │       │         (compose 项目名 + 文件 + workDir 写入 tasks 表)     frontend_url = 提取的首个服务 URL
    │       │                                                             docker_running = 1
    │       │
    │       └── 完全失败（F1-F6）──▶ 跳过 Stage C ──▶ tasks.state = 'waiting_manual'
    │       │   (Docker 未启动)                        frontend_url = ""
    │       │                                          docker_running = 0
    │       │
    │       └── 部分失败（F7a/F7b）──▶ 继续 Stage C ─▶ tasks.state = 'waiting_manual'
    │           (Docker 已启动但                 (Stage C 可尝试通过环境变量
    │            端口检测失败)                    连接服务)    docker_running = 1
    │                                                          frontend_url = "" (标记"端口检测失败")
    │
    └── 无论成功/失败，最终都进入 waiting_manual，由质检员统一确认
```

**注**：Stage B 的 7 个故障模式分类见 Section 3.5。

### 1.4 状态转换规则

```
inspecting ──▶ waiting_manual  : pipeline 全部 Stage 完成（含失败/跳过），由 pipeline 的 finishRun 或 scheduler finishJob 触发
inspecting ──▶ inspecting       : pipeline aborted 或 crashed，清除 current_run_id，任务留在 inspecting 并显示错误，允许重试
waiting_manual ──▶ completed   : 质检员按 Ctrl+E 确认（触发 Docker 清理，如有运行中的容器）
completed ──▶ inspecting       : 质检员按 Ctrl+R 重新质检，Git force-pull 后重新运行 pipeline（Git sync 成功后状态才切换为 inspecting）
waiting_manual ──▶ inspecting  : 不允许（必须先完成当前轮次）
```

### 1.5 非 BC 阶段失败的路径

当 Stage A/D/E/F 失败时，pipeline 继续执行后续阶段（现有行为保留）。所有阶段执行完毕后（无论成败），任务转换到 `waiting_manual`。质检员在 TUI 卡片上可以看到哪个阶段失败，决定后续操作（Ctrl+E 确认或 Ctrl+R 重检）。

### 1.6 生命周期约束

- 同一题目**同一时间最多只有一个活跃的 pipeline run**，通过 `tasks.current_run_id` CAS 互斥检查。
- `current_run_id` 生命周期：
  - **设置时机**：Pipeline `Run()` 创建成功后，在 `prepareRun` → `CreateRun` 成功后，通过 CAS 写入：`UPDATE tasks SET current_run_id = ? WHERE id = ? AND current_run_id IS NULL`
  - **清除时机**：当 run 达到任何终止状态（completed_clean/completed_with_findings/aborted/crashed），在 `FinishRun` 完成后清除：`UPDATE tasks SET current_run_id = NULL, updated_at = ? WHERE id = ?`
- 开始质检前，系统异步同步 Git 仓库（作为 scheduler Job）。Git 同步失败则阻断进入 pipeline，任务保持在 `inspecting`（子状态 `syncing`），显示错误并提供 Ctrl+G 重试。
- 进入 `completed` 状态时，通过 SQL 原子操作 `UPDATE tasks SET completion_count = completion_count + 1 ... WHERE id = ? AND state = 'waiting_manual'` 自增，防止并发重复计数。
- `Ctrl+R` 时，任务先在 `completed` 状态保持，等待 Git force-pull 成功后才转换到 `inspecting`。若 Git 失败，任务留在 `completed` 并显示错误，允许重试。
- 在 `waiting_manual` 状态下强制退出 TUI：触发 Docker 全量清理，尽力而为（单步失败不阻塞退出，但记录日志）。
- 系统注册 SIGTERM/SIGINT 信号处理器，在 OS 级终止信号下触发 `FullDockerCleanup()` 后退出。

### 1.7 状态修复与恢复

#### TUI 启动时
- 检测 `tasks.state = 'inspecting'` 且 `current_run_id` 指向的 run 状态为终止态（completed_clean/aborted/crashed 等）→ 修复 `tasks.state` 为 `waiting_manual`（run 已完成）或清除 `current_run_id`（run 被清理）
- 检测 `tasks.state = 'waiting_manual'` 且 `docker_running = 1` → 运行 `docker compose ps` 验证实际状态，若容器丢失则更新 `docker_running = 0`

#### 定期轮询（poller）
- `RecoverStaleRuns` 完成后，同步修复对应 task 的 `current_run_id`（设为 NULL）和 `state`
- 对 `waiting_manual + docker_running=1` 的任务运行健康检查，若容器丢失则更新 `docker_running=0`

---

## 二、数据库 Schema 变更

### 2.1 新增 `tasks` 表

```sql
CREATE TABLE IF NOT EXISTS tasks (
    id               TEXT PRIMARY KEY,              -- TASK-YYYYMMDD-XXXXXX（格式由输入层校验）
    batch_id         TEXT NOT NULL,                 -- 系统生成的批次 ID
    git_url          TEXT NOT NULL,                 -- GitLab 完整仓库 URL
    repo_path        TEXT NOT NULL,                 -- 本地 clone 路径
    state            TEXT NOT NULL DEFAULT 'inspecting'
                     CHECK (state IN ('inspecting', 'waiting_manual', 'completed')),
    current_run_id   TEXT,                          -- 当前活跃的 run ID (关联 runs 表)
    completion_count INTEGER NOT NULL DEFAULT 0
                     CHECK (completion_count >= 0),
    frontend_url     TEXT DEFAULT '',               -- 首个 HTTP 服务 URL（多服务时存储第一个；Docker 失败时为空）
    docker_running   INTEGER NOT NULL DEFAULT 0,    -- Docker 是否在运行 (0/1)
    compose_meta     TEXT DEFAULT '',               -- JSON: {project, files[], work_dir}，用于 Ctrl+E 清理时确定 compose 目标
    entered_waiting_at TEXT DEFAULT '',             -- 进入 waiting_manual 的时间戳（用于计算等待时长）
    last_completed_at  TEXT DEFAULT '',             -- 最近一次完成时间（用于完成卡片显示）
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    FOREIGN KEY (current_run_id) REFERENCES runs(run_id) ON DELETE SET NULL
);

CREATE INDEX idx_tasks_state ON tasks(state);
CREATE INDEX idx_tasks_batch ON tasks(batch_id);
CREATE INDEX idx_tasks_batch_state ON tasks(batch_id, state);
CREATE INDEX idx_tasks_state_docker ON tasks(state, docker_running);
```

### 2.2 新增 `batches` 表

```sql
CREATE TABLE IF NOT EXISTS batches (
    id           TEXT PRIMARY KEY,              -- batch-001, batch-002, ...
    display_name TEXT NOT NULL,                 -- 显示名称
    task_count   INTEGER NOT NULL DEFAULT 0,    -- 当前题目数量（已废弃：改为查询 COUNT(*)）
    max_tasks    INTEGER NOT NULL DEFAULT 20,   -- 每批上限
    created_at   TEXT NOT NULL,
    is_full      INTEGER NOT NULL DEFAULT 0     -- 是否已满（已废弃：改为查询 COUNT(*) >= max_tasks）
);
```

**注意**：`task_count` 和 `is_full` 字段保留用于迁移兼容，但**不再作为权威数据源**。所有批满判断改为 `SELECT COUNT(*) FROM tasks WHERE batch_id = ?`。这消除了去规范化计数器漂移的风险。

### 2.3 `runs` 表扩展

```sql
-- 注意: runs.task_id 列已存在于当前 schema（migrate.go:122），无需重复添加
-- 仅添加 completion_round 列：
ALTER TABLE runs ADD COLUMN completion_round INTEGER NOT NULL DEFAULT 1;
```

`completion_round` 表示本次 run 是该题目的第几次质检轮次（= 创建 run 时 tasks.completion_count + 1）。用于审计和排序。所有历史 runs 默认为 1。

### 2.4 迁移安全措施

- 在迁移中启用 `PRAGMA foreign_keys = ON`（需验证现有数据无孤立引用）
- `batches` 表初始数据从现有 `projects.batch` 去重填充
- 新增 `CHECK` 约束仅对新写入数据生效，现有数据不受影响
- 所有新查询**必须**使用参数化查询（`?` 占位符），延续现有 `store.go` 的安全模式

### 2.5 现有表影响

- `projects` 表：保留不变。task 创建时通过 `ON CONFLICT(task_id) DO UPDATE` 同步写入，确保 Overview 页能 JOIN 查询
- `run_stages` 表：无需变更
- `findings` 表：无需变更

---

## 三、Pipeline Stage 顺序调整

### 3.1 当前实际顺序

`defaultRunStages()` (`internal/pipeline/stage.go:208-217`) 实际执行顺序为 **A → B → C → D → F**（Stage E 被显式排除；`staticStageSet()` 同样排除 E）。只包含 5 个阶段。

### 3.2 新顺序

```
A (结构与规则检查) → D (测试有效性静态审查) → E (静态验收审计)
                   → F (标注员修复静态审查) → B (Docker运行时证据) → C (测试运行时证据)
```

所有六个阶段全部纳入默认流水线。

### 3.3 变更理由

标注员修复报告（F）完成后，质检员可以看到完整的代码审查结果。之后再启动 Docker 运行系统，质检员可以：
- 结合修复报告的内容手动验证系统功能
- 通过 run_tests.sh 验证修复后的测试是否通过
- 更有针对性地进行手动测试

### 3.4 改动文件与具体变更

| 文件 | 改动内容 |
|------|---------|
| `internal/pipeline/model/stages.go:25-32` | 重新排序 `stageSpecs` 数组声明顺序 + 调整各 StageSpec 的 `Order` 字段：A=1, D=2, E=3, F=4, B=5, C=6 |
| `internal/pipeline/stage.go:208-217` | `defaultRunStages()`：**移除** `if stage == string(model.StageE) { continue }` 的排除逻辑 |
| `internal/pipeline/stage.go:198-208` | `staticStageSet()`：**移除** Stage E 排除逻辑（`&& spec.ID != model.StageE`） |
| `internal/tui/stage_plan.go:121-124` | `withoutStageE()` 调用点评估：保留函数但确认在 task 驱动模式下不再过滤 E |
| `internal/tui/runconfig.go:61,162` | Re-run 配置面板的 Stage 列表确认涵盖 Stage E |
| `internal/pipeline/run_lifecycle.go:284-335` | 新增 `SkipNextStage` 检查逻辑；**移除**自动 Docker 清理触发点（`runtimeCleanupPoint` 调用及其 fallback） |
| `internal/pipeline/cleanup.go:175-188` | `runtimeCleanupPoint()` 修改为始终返回 false（task 驱动模式下 Docker 由 TUI 控制） |
| `internal/pipeline/cleanup.go:110-133` | 拆分 `finalizeRuntime()` 为 `StopDockerRuntime(runtimeState)` 和 `FullDockerCleanup()` |

### 3.5 Stage B 失败模式与处理

Stage B 有 7 个故障点（位于 `internal/docker/manager.go:93-352` 的 `StartRuntime()`）：

| 编号 | 故障 | Category | Docker 状态 | 处理 |
|------|------|----------|-------------|------|
| F1 | 无 compose 文件 | `compose_config_failed` | 未启动 | `SkipNextStage=true`，跳过 C |
| F2 | 无 docker 二进制 | `compose_config_failed` | 未启动 | `SkipNextStage=true`，跳过 C |
| F3 | compose config 失败 | `compose_config_failed` | 未启动 | `SkipNextStage=true`，跳过 C |
| F4 | pull 失败 (required) | (仅 require 策略) | 未启动 | `SkipNextStage=true`，跳过 C |
| F5 | build 失败 | `build_failed` | 未启动 | `SkipNextStage=true`，跳过 C |
| F6 | up 失败 | `up_failed` | 未启动 | `SkipNextStage=true`，跳过 C |
| F7a | 端口收集失败，无映射 | — | **已启动** | `SkipNextStage=false`，继续 C |
| F7b | 无端口映射 | — | **已启动** | `SkipNextStage=false`，继续 C |

F1-F6（Docker 未启动）：跳过 Stage C。F7a/F7b（Docker 已启动但端口检测失败）：**继续执行 Stage C**，因为 Stage C 通过环境变量注入服务 URL，可能可通过非 HTTP 方式访问。

### 3.6 `SkipNextStage` 实现

```go
// internal/pipeline/stage.go — StageOutcome 新增字段
type StageOutcome struct {
    Record        model.StageRecord
    Runtime       *RuntimeState
    SkipNextStage bool  // 【新增】是否跳过下一个 Stage
}

// internal/pipeline/run_lifecycle.go — executeStageLoop 修改
// 在 persistStageUpdate (line 327) 之后添加：
if outcome.SkipNextStage && index+1 < len(stages) {
    nextStage := stages[index+1]
    s.execution.stages[index+1].Status = model.StageSkipped
    s.execution.stages[index+1].Reason = "Skipped: previous stage (B) failed"
    // 记录日志...
}
```

### 3.7 Stage E 纳入默认流水线的注意事项

Stage E 使用 `static_acceptance_audit.md` profile（`stage_codex_review.go:373`），此前从未在默认流水线中运行。实施时应：
1. 验证 E 的 profile 产生有效的 `p2r.static_review.v1` JSON 输出
2. 确认 `finalizeStaticReviewReport()` 可正确解析 E 的输出
3. Stage F 的 `priorStageSnapshot()` 在新顺序下缺少 B/C findings，但这是预期行为（静态审查在运行前进行）

---

## 四、Git 集成设计

### 4.1 URL 构造规则

```
base:       由配置项 git.base_url 提供（默认：https://gitlab.mindflow.com.cn/Prompt2Repo/fullstack/）
template:   {base}/{TASK_ID}
example:    https://gitlab.mindflow.com.cn/Prompt2Repo/fullstack/TASK-20260508-DA6249
```

URL 构造使用 `url.JoinPath()` 防止 URL 注入。`base_url` 必须通过 HTTPS、域名白名单验证。

认证：依赖系统已配置的 GitLab 全局凭据（SSH key 或 credential helper），p2r 不额外管理认证。Git 操作前检查 `git` 在 PATH 中存在，并设置 `GIT_TERMINAL_PROMPT=0` 强制非交互失败。

### 4.2 Batch 管理

系统自动管理 batch，每满 20 题自动切到下一个 batch。

```
batch-001: TASK-001 ~ TASK-020
batch-002: TASK-021 ~ TASK-040
...
```

逻辑（**所有步骤在单个 `withWriteTx` 事务中完成**）：

1. 质检员输入 TASK ID（经格式校验后）
2. `SELECT id FROM batches WHERE id = (SELECT MAX(CAST(SUBSTR(id, 7) AS INTEGER)) FROM batches WHERE (SELECT COUNT(*) FROM tasks WHERE batch_id = batches.id) < 20)`
3. 如果无未满 batch，计算下一个 batch ID 并 `INSERT INTO batches`
4. 本地路径：`projects-qa/{batch-id}/{task-id}/`（使用 `filepath.Join` 构造，通过 `pathutil.PathWithin` 验证包含关系）
5. 在同一事务中 `INSERT INTO tasks` 并分配 batch_id

### 4.3 TASK ID 验证

输入框在键盘输入接收时即执行验证：

```go
var taskIDPattern = regexp.MustCompile(`^TASK-\d{8}-[A-F0-9]{6}$`)
const maxTaskIDLength = 64

func ValidateTaskID(raw string) (string, error) {
    cleaned := strings.TrimSpace(raw)
    if len(cleaned) > maxTaskIDLength {
        return "", fmt.Errorf("TASK ID exceeds max length")
    }
    if !taskIDPattern.MatchString(cleaned) {
        return "", fmt.Errorf("invalid TASK ID format, expected: TASK-YYYYMMDD-XXXXXX")
    }
    return cleaned, nil
}
```

### 4.4 Git 同步模型：异步（Scheduler Job）

Git 同步作为**独立的调度器 Job 类型**执行，非阻塞 TUI。

**新增调度器支持**：
```go
// internal/scheduler/ — 新增
type GitSyncRunner interface {
    Sync(ctx context.Context, taskID, gitURL, repoPath string, onProgress func(SyncProgress)) (*SyncResult, error)
}

type SyncProgress struct {
    Phase   string // "clone", "fetch", "checkout"
    Percent int    // 0-100, -1 if unknown
    Message string
}
```

**流程**：
1. TUI 输入框 Enter → `TaskActionService.StartInspection()` → 创建 task 行（state=inspecting）→ 提交 `GitSyncJob` 到 scheduler
2. Scheduler 执行 Git 同步，通过 `onProgress` 回调发布进度事件
3. TUI 的 `taskcard.go` 通过 poller 或消息通道接收进度并渲染 `[Git 同步中] clone: 45%`
4. Git 同步成功后，scheduler 自动提交 `PipelineJob`（`Run()`）
5. Git 同步失败后，task 保持 `inspecting` 并记录错误信息

### 4.5 Git 同步流程

#### 初次质检（目录不存在或克隆标记缺失）

```
1. filepath.Join 构造 repoPath，pathutil.PathWithin 验证
2. mkdir -p projects-qa/{batch-id}/
3. git clone --depth 1 {git_url} {repoPath}   ← 默认浅克隆
4. 验证目录结构（是否存在 .git 目录）
5. git lfs pull（如启用 LFS）
6. git submodule update --init --recursive（如存在子模块）
7. 写入 .qa-clone-done 标记文件
8. 记录 repo_path 到数据库
```

#### 再次质检（.qa-clone-done 标记存在）

```
1. cd {repoPath}
2. git stash push -m "auto-stash-before-force-pull-{timestamp}"   ← 防止数据丢失
3. git fetch origin --force --prune
4. git reset --hard origin/HEAD   （若 origin/HEAD 不存在则使用 origin/main）
5. git clean -fdx
6. git lfs pull（如启用）
7. git submodule update --init --recursive
```

### 4.6 新增模块

| 文件 | 职责 |
|------|------|
| `internal/git/sync.go` | Git 同步核心逻辑：Clone、ForcePull、GetStatus |
| `internal/git/progress.go` | SyncProgress 类型定义 |
| `internal/git/sync_test.go` | 单元测试 |

```go
package git

type SyncProgress struct {
    Phase   string
    Percent int
    Message string
}

type SyncResult struct {
    Operation string // "clone" or "force-pull"
    Commit    string
    Error     error
}

type Syncer struct {
    BasePath string // projects-qa/
    cfg      GitConfig
}

// GitConfig 新增到 config.go
type GitConfig struct {
    BaseURL      string        `yaml:"base_url"`
    CloneTimeout time.Duration `yaml:"clone_timeout"` // 默认 10m
    ShallowClone bool          `yaml:"shallow_clone"` // 默认 true
    LFSEnabled   bool          `yaml:"lfs_enabled"`   // 默认 false
}

type SyncCallback func(SyncProgress)

func NewSyncer(basePath string, cfg GitConfig) *Syncer
func (s *Syncer) Sync(ctx context.Context, taskID, gitURL string, onProgress SyncCallback) (*SyncResult, error)
func (s *Syncer) RepoPath(batchID, taskID string) string
```

---

## 五、TUI 界面设计

### 5.1 题目管理页：左中右三栏布局

（ASCII 示意与原始设计一致，略）

### 5.2 三栏布局规格

| 属性 | 值 |
|------|-----|
| 列宽分配 | 三等分终端宽度（`width / 3`），列间 `│` 分隔 |
| 降级阈值 | **≤ 110 列** 时降级为单栏 Tab 视图（提高自 100 列以避免内容溢出） |
| 列标题 | 固定顶部，带题目计数：`─── 开始质检 (3) ───` |
| 滚动 | 每列独立纵向滚动，保留滚动位置和选中索引 |
| 选中高亮 | 白字蓝底 |
| 列间切换 | `←` `→` 键 |
| 空列 | 显示灰色占位文字：`暂无题目` |
| 新任务插入 | 追加到列顶部；不移动现有滚动视口 |

### 5.3 题目卡片内容规格（含截断规则）

所有卡片内容必须使用现有截断函数（`layout.go:171-230` 的 `truncateDisplay`/`truncateMiddleDisplay`）处理，**禁止换行**。

截断宽度 = `colWidth - 4`（保留边框和内边距余量）。

#### 开始质检卡片——正常执行中（4 行）

```
TASK-YYYYMMDD-XXXXXX              ← 截断使用 truncateMiddleDisplay
D: 测试有效性审查                  ← 紧凑模式：仅显示 Stage 字母 + 中文名（省略 "Stage " 前缀）
[▓▓▓▓▓▓▓▓▓░░░░░░░░░] 65%
─────────────────────────────
```

#### 开始质检卡片——Git 同步中（3 行）

```
TASK-YYYYMMDD-XXXXXX
[Git 同步中] clone: 45%
─────────────────────────────
```

#### 开始质检卡片——Git 同步失败（4 行）

```
TASK-YYYYMMDD-XXXXXX
[Git 同步失败] 网络超时           ← 红色
Ctrl+G 重试
─────────────────────────────
```

#### 开始质检卡片——阶段失败（4 行）

```
TASK-YYYYMMDD-XXXXXX
D: 测试有效性审查
✗ 失败: Codex API 超时
─────────────────────────────
```

#### 待处理卡片——Docker 运行中（4 行）

```
TASK-YYYYMMDD-XXXXXX
http://localhost:30080              ← 蓝色，截断
等待: 02:35                         ← 基于 entered_waiting_at 计算
─────────────────────────────
```

#### 待处理卡片——Docker 失败（4 行）

```
TASK-YYYYMMDD-XXXXXX
✗ Docker 启动失败                   ← 红色，带 ✗ 冗余图标
等待: 45:12
─────────────────────────────
```

#### 待处理卡片——Docker 部分启动（4 行）

```
TASK-YYYYMMDD-XXXXXX
! Docker 已启动，端口检测失败        ← 黄色，带 ! 图标
等待: 00:05
─────────────────────────────
```

#### 结束质检卡片（3 行）

```
TASK-YYYYMMDD-XXXXXX
累计完成: 3 次 · 最后: 05-21 14:30  ← 基于 completion_count + last_completed_at
─────────────────────────────
```

### 5.4 总览页（保留并增强）

保留当前 `overview.go` 的表格形式。增强点：

- 新增 `task_id` 列（`projects LEFT JOIN tasks ON projects.task_id = tasks.id`），扫描发现项目显示 "-"
- 新增 `completion_count` 列
- 表格行按 task 状态着色（无 task 行 → 默认色）
- 新增 task state 过滤选项
- 新增 `completion_count` 排序列

### 5.5 设置浮层

从全屏页签改为**右下角模态浮层**。

尺寸公式：`width = clamp(floor(terminalWidth * 0.35), 40, 60)`，`height = min(contentRows, max(10, terminalHeight * 0.6))`。

**浮层键盘捕获矩阵**：

| 按键 | 行为 |
|------|------|
| `Tab` | 在浮层字段之间循环 |
| `Esc` / `Ctrl+/` | 关闭浮层 |
| `↑` `↓` `←` `→` | 导航浮层字段 |
| `Enter` | 激活当前按钮/字段 |
| 所有其他快捷键 | **阻止**（浮层模态捕获） |
| `Q` | 关闭浮层（不退出 TUI） |

### 5.6 共享逻辑抽象

```go
// shared.go — 依赖注入架构

// TaskQueryService 由 app 构造（持有 db.Store），注入到 taskBoard 和 overview
type TaskQueryService interface {
    ListByState(ctx context.Context, state string) ([]TaskProject, error)
    ListAll(ctx context.Context, query db.ProjectQuery) ([]TaskProject, int, error)
    GetByID(ctx context.Context, taskID string) (*TaskProject, error)
    FindWithDockerRunning(ctx context.Context) ([]TaskProject, error)
    FindStaleInspecting(ctx context.Context) ([]TaskProject, error)
}

// TaskActionService 由 app 构造（持有 Syncer + scheduler + store），注入到 taskBoard
type TaskActionService interface {
    StartInspection(ctx context.Context, taskID string) error
    ReInspect(ctx context.Context, taskID string) error
    ConfirmComplete(ctx context.Context, taskID string) error
    RetryGitSync(ctx context.Context, taskID string) error
}

// TaskProject 统一的视图模型（所有查询返回此类型）
type TaskProject struct {
    ID              string
    BatchID         string
    TaskState       string
    CompletionCount int
    FrontendURL     string
    DockerRunning   bool
    LastRunID       string
    LastRun         string
    RunStatus       string
    ManualVerdict   string
    FailedStage     string
    Blocking        int
    High            int
    DocsCount       int
    Mode            string
    Path            string
    // 以下仅在有 tasks 行时填充
    LastCompletedAt string
    EnteredWaitingAt string
    SyncError       string
}
```

### 5.7 全局输入框

- 位于终端最下方左下角，固定一行
- 聚焦时显示彩色边框提示（视觉焦点指示器）
- `/` 键：从任意位置聚焦输入框
- `Esc`：清空内容并失焦
- `Enter`：在校验 TASK ID 格式后提交 `TaskInputSubmitMsg{TaskID}`，由 `app.Update` 路由到 `TaskActionService.StartInspection()`
- 最多输入 64 字符；粘贴内容自动截断
- 输入框聚焦时方向键移动文本光标（不切换列）

### 5.8 交互操作汇总

| 操作 | 快捷键 | 适用位置 | 前置条件 | 行为 |
|------|--------|---------|---------|------|
| 输入题目编号 | 键入字符 | 全局（输入框聚焦） | 输入框聚焦 | 捕获可打印字符 |
| 聚焦输入框 | `/` | 全局 | 非浮层状态 | 聚焦输入框 |
| 取消输入框 | `Esc` | 输入框聚焦 | — | 清空并失焦 |
| 开始质检 | `Enter` | 输入框聚焦 | TASK ID 校验通过 | 提交 StartInspection |
| 确认测试完毕 | `Ctrl+E` | 待处理列（选中题目） | tasks.state = waiting_manual | Docker 清理 → completed |
| 重新质检 | `Ctrl+R` | 结束质检列（选中题目） | tasks.state = completed | Git force-pull → 新 pipeline |
| 重试 Git 同步 | `Ctrl+G` | 开始质检列（选中题目） | Git sync 已失败 | 重新执行 Git sync |
| 总览页面 | `Ctrl+O` | 全局 | 非浮层状态 | 切换到总览页 |
| 设置浮层 | `Ctrl+/` | 全局 | — | 打开/关闭设置浮层 |
| 查看详情 | `Enter` | 任意列卡片聚焦 | 非输入框聚焦 | 展开历史 run 和 findings |
| 切换页面 | `Tab` | 全局 | 非浮层状态 | 题目管理 ↔ 总览 |
| 切换列 | `←` `→` | 题目管理页（非输入框聚焦） | 非浮层状态 | 在三列间切换 |
| 列内滚动 | `↑` `↓` / 滚轮 | 题目管理页（非输入框聚焦） | 非浮层状态 | 焦点列内滚动 |
| 退出 | `Q` | 全局（非输入框聚焦） | 非浮层状态 | 触发退出确认 → 清理 → 退出 |

### 5.9 颜色方案（含冗余指示器）

| 元素 | 颜色 | 冗余指示器 |
|------|------|-----------|
| 开始质检列标题 | `#00DDDD`（青色） | — |
| 待处理列标题 | `#DDAA00`（黄色） | — |
| 结束质检列标题 | `#00CC66`（绿色） | — |
| Docker 失败 | `#FF4444`（红色） | ✗ 前缀 |
| Docker 部分失败 | `#DDAA00`（黄色） | ! 前缀 |
| 等待时长 > 30 分钟 | `#FF4444`（红色） | ⏱ 前缀 |
| 前端 URL | `#4488FF`（蓝色） | — |
| 阶段失败标记 | `#FF4444`（红色） | ✗ 前缀 |

**自适应回退**：检测终端颜色能力（`COLORTERM`、`NO_COLOR`）。16 色终端降级到 ANSI 命名颜色；`NO_COLOR=1` 则完全禁用颜色，仅依赖图标区分状态。

### 5.10 响应式降级

当终端宽度 **≤ 110 列**时，三栏布局降级为单栏 Tab 视图：三列变为三个水平 Tab（`开始质检` | `待处理` | `结束质检`），通过 `←` `→` 切换。降级时保留当前聚焦列的选中状态。

---

## 六、Docker 生命周期管理

### 6.1 完整生命周期

```
Stage B:
  ├── docker compose pull (optional, best-effort — 默认策略已是 best_effort)
  ├── docker compose build
  ├── docker compose up -d --label managed_by=p2rqa=true   ← 【关键】添加标签
  ├── 健康检查
  ├── 端口探测
  │
  ├── 成功 ──▶ 提取首个 HTTP 服务 URL → frontend_url
  │          │  记录 compose_meta = {project, files[], work_dir} 到 tasks 表
  │          │  设置 docker_running=1
  │          │
  │          ▼
  │           Stage C:
  │             ├── bash run_tests.sh
  │             ├── 清理临时文件
  │             └── Docker 继续运行
  │          │
  │          ▼
  │           进入 waiting_manual（Docker 正在运行）
  │
  ├── 完全失败（F1-F6）──▶ docker_running=0, frontend_url=""
  │                        SkipNextStage=true，跳过 C
  │                        进入 waiting_manual（Docker 未运行）
  │
  └── 部分失败（F7a/F7b）──▶ docker_running=1, frontend_url=""
                             SkipNextStage=false，继续 C
                             进入 waiting_manual

waiting_manual 期间（Docker 成功时）:
  容器保持运行，质检员通过 frontend_url 手动测试

waiting_manual 期间（Docker 失败时）:
  质检员可自行排查问题后确认完成

Ctrl+E 确认（Docker 正在运行）:
  ├── 读取 tasks.compose_meta 获取 compose 项目参数
  ├── 写入清理检查点 (.qa-control/cleanup_checkpoint.json)
  ├── docker compose -f {files} -p {project} down --timeout 30
  ├── docker compose -f {files} -p {project} rm -f
  ├── 清理 p2r 标签的网络/卷
  ├── tasks.state = 'completed', docker_running = 0
  ├── tasks.completion_count = completion_count + 1 (SQL 原子操作)
  ├── tasks.last_completed_at = now, updated_at = now
  └── 删除清理检查点

Ctrl+E 确认（Docker 未运行/已失败）:
  └── tasks.state = 'completed', completion_count += 1, last_completed_at = now

强制退出 TUI（Q 键，等待中 + Docker 运行中）:
  ├── 弹窗确认："存在运行中的 Docker 容器，退出将清理。确认退出？[Y/N]"
  ├── ForceExitCleanup(): 遍历本地 TUI 实例管理所有 compose 项目
  │     → docker compose down（每项目 2 分钟 timeout，context.WithTimeout 全局 5 分钟）
  │     → docker container prune --filter "label=managed_by=p2rqa" --force
  │     └── docker network prune --filter "label=managed_by=p2rqa" --force
  └── 退出（best-effort：单步失败记录日志但继续）

正常退出 TUI（Q 键，无运行中 Docker）:
  ├── 弹窗确认："退出 TUI？[Y/N]"
  ├── LightExitCleanup(): 仅 prune 已停止的容器和未使用网络（按标签过滤）
  └── 退出
```

### 6.2 Docker 异常处理

| 场景 | 处理 |
|------|------|
| Stage B 启动失败（F1-F6） | pipeline 继续（跳过 C），直接进入 `waiting_manual`，标记 Docker 失败 |
| Stage B 部分失败（F7a/F7b） | pipeline 继续（不跳过 C），进入 `waiting_manual`，标记 Docker 已运行但端口检测失败 |
| Docker daemon 崩溃 | TUI poller 定期检测（`docker compose ps`），若容器丢失则更新 `docker_running=0` |
| TUI 启动时检测到遗留 p2r 容器 | 弹窗提示："检测到遗留 Docker 容器，是否清理？[Y/N]"；同时修复 tasks.docker_running |
| Ctrl+E 时 Docker daemon 无响应 | 守护进程活性检测（`docker info` timeout 3s），失败则跳过清理步骤 |
| TUI 收到 SIGTERM/SIGINT | 信号处理器触发 ForceExitCleanup → os.Exit |
| 多 TUI 实例同时运行 | 每个实例仅管理自己的 compose 项目（通过 batch 的 compose 项目名称前缀区分） |

### 6.3 Docker 清理函数定义

```go
// StopDockerRuntime 停止并清理单个题目的 Docker 环境
// 从 tasks.compose_meta 读取 compose 项目参数
func StopDockerRuntime(ctx context.Context, composeMeta ComposeMeta) error {
    // 1. docker compose -f {files} -p {project} down --timeout 30
    // 2. docker compose -f {files} -p {project} rm -f
    // 3. 清理该项目的 p2r 标签网络和卷
}

// ComposeMeta 存储在 tasks.compose_meta 列（JSON）
type ComposeMeta struct {
    Project     string   `json:"project"`
    ComposeFiles []string `json:"compose_files"`
    WorkDir     string   `json:"work_dir"`
}

// ForceExitCleanup 清理本 TUI 实例管理的所有 Docker 资源
func ForceExitCleanup(ctx context.Context, composeProjects []ComposeMeta) error {
    // 1. 遍历每个 compose 项目 → docker compose down（每项目 2min timeout）
    // 2. docker container prune --filter "label=managed_by=p2rqa" --force
    // 3. docker network prune --filter "label=managed_by=p2rqa" --force
}

// LightExitCleanup 轻量退出清理（无运行中 Docker 时）
func LightExitCleanup(ctx context.Context) error {
    // 1. docker container prune --filter "label=managed_by=p2rqa" --filter "status=exited" --force
    // 2. docker network prune --filter "label=managed_by=p2rqa" --force
}
```

### 6.4 退出确认逻辑

```go
func (a *app) handleQuit() tea.Cmd {
    // 检查所有状态中 docker_running=1 的任务
    runningTasks, _ := a.taskQuerySvc.FindWithDockerRunning(context.Background())
    hasRunningDocker := len(runningTasks) > 0

    if hasRunningDocker {
        a.showConfirmDialog(
            "存在运行中的 Docker 容器，退出将清理。确认退出？",
            func() { a.forceExitCleanup() },  // Y → ForceExitCleanup → quit
            func() {},                          // N → cancel
        )
    } else {
        a.showConfirmDialog(
            "退出 TUI？",
            func() { a.lightExitCleanup() },   // Y → LightExitCleanup → quit
            func() {},                          // N → cancel
        )
    }
    return nil
}
```

### 6.5 改动点

| 文件 | 改动内容 |
|------|---------|
| `internal/pipeline/stage_b.go` | F1-F6 失败时设置 `SkipNextStage=true`；F7a/F7b 失败时 `SkipNextStage=false`；成功时写入 `compose_meta` 到 task；**在 `docker compose up` 命令中添加 `--label managed_by=p2rqa=true`** |
| `internal/pipeline/stage_c.go` | 增加测试产物清理；不再触发 Docker down |
| `internal/pipeline/cleanup.go` | 新增 `StopDockerRuntime()` 和 `ForceExitCleanup()` |
| `internal/pipeline/model/model.go` | `StageOutcome` 添加 `SkipNextStage` 字段 |
| `internal/docker/manager.go` | 新增 `IsRunning()`、`GetFrontendURL()`、`ListAllProjects()` |
| `internal/docker/compose.go` | `ComposeCommandArgsWithProjectDir` 添加可选的 labels 参数 |
| `internal/docker/gc.go` | 增强全量/轻量清理 |

---

## 七、Pipeline → Task 集成契约

### 7.1 集成点（谁在何时写入 tasks 表）

| 事件 | 触发位置 | 写入 tasks 的操作 |
|------|---------|-----------------|
| 任务创建 + Git 开始 | `TaskActionService.StartInspection()` | INSERT tasks（state=inspecting, current_run_id=NULL）→ 提交 GitSyncJob |
| Git 同步完成 | `scheduler.finishGitJob()` | 提交 PipelineJob |
| Pipeline run 创建 | `run_lifecycle.go:prepareRun` after `CreateRun` | CAS: `UPDATE tasks SET current_run_id = ? WHERE id = ? AND current_run_id IS NULL` |
| Stage B 完成（成功） | `run_lifecycle.go:persistStageUpdate` after B | `UPDATE tasks SET frontend_url = ?, docker_running = 1, compose_meta = ?` |
| Stage B 完成（失败） | `run_lifecycle.go:persistStageUpdate` after B | `UPDATE tasks SET docker_running = 0` |
| Pipeline 全部 Stage 完成 | `pipeline.finishRun()` 或 `scheduler.finishJob()` | `UPDATE tasks SET state = 'waiting_manual', entered_waiting_at = ?, current_run_id = NULL`（与 FinishRun 同一事务） |
| Run 被中止 | `pipeline.finishAbortedRun()` | `UPDATE tasks SET current_run_id = NULL`（清除互斥锁） |
| Run 崩溃恢复 | `RecoverStaleRuns` 后 | `UPDATE tasks SET current_run_id = NULL, state = 'waiting_manual'` |
| TUI 启动修复 | `app.startup` | 遍历 tasks 修复不一致状态（见 Section 1.7） |
| TUI poller 修复 | `poller.tick` | 修复不一致状态 + Docker 健康检查 |

### 7.2 关键事务原子性要求

`pipeline.finishRun()` 中的 `r.store.FinishRun()` 必须将 tasks 更新纳入**同一 `withWriteTx` 事务**：

```sql
-- 事务内：
UPDATE runs SET finished_at = ?, status = ?, duration_ms = ? WHERE run_id = ?;
UPDATE projects SET run_count = run_count + 1, last_run_id = ?, last_run_at = ? WHERE task_id = ?;
UPDATE tasks SET state = 'waiting_manual', entered_waiting_at = ?, updated_at = ? WHERE id = ?;
```

这消除 crash 窗口：run 完成但 task 永远卡在 inspecting。

---

## 八、实施计划

### Phase 0：前置验证（0.5 天）

0. 验证现有 `runs` 表已有 `task_id` 列（确认 `migrate.go:122`）
1. 验证 `static_acceptance_audit.md` profile 是否能产生有效输出（Stage E 纳入默认流水线的前置条件）
2. 验证 `PRAGMA foreign_keys` 启用后无现有数据冲突

### Phase 1：数据层（2-3 天）

1. `internal/pipeline/model/model.go` — `StageOutcome` 添加 `SkipNextStage`；定义 `Task`、`Batch`、`ComposeMeta` 结构体
2. `internal/db/migrate.go` — 添加 v5 migration：`tasks` 表、`batches` 表、`runs.completion_round` 列、索引、CHECK 约束、`PRAGMA foreign_keys = ON`；从 `projects.batch` 去重填充 `batches` 初始数据
3. `internal/db/store.go` — 添加 Task 和 Batch 的 CRUD 方法（**全部使用参数化查询 + withWriteTx**）；扩展 `FinishRun` 为 `FinishRunAndTransitionTask`
4. `internal/config/config.go` — 添加 `GitConfig` 结构体（`base_url`、`clone_timeout`、`shallow_clone`、`lfs_enabled`）

### Phase 2：TUI 基础设施重构（2.5-3 天）

5. `internal/tui/router.go`（新） — `pageRouter`、`Page` 接口、`Overlay` 接口、焦点管理
6. `internal/tui/poller.go`（新） — 调度器轮询 + tasks 状态修复 + Docker 健康检查
7. `internal/tui/pipelinebar.go`（新） — 流水线状态栏渲染（纯函数 + snapshot 模式）
8. `internal/tui/app.go` — 重构 `app` 结构体，组装新模块（DI 容器构造）
9. `internal/tui/shared.go`（新） — `TaskQueryService`、`TaskActionService` 接口及基于 `db.Store` + `git.Syncer` + `scheduler` 的实现

### Phase 3：TASK ID 验证 + Git 集成（1.5-2 天）

10. `internal/tui/taskinput.go`（新） — 输入框组件，含 `ValidateTaskID()` 校验、`TaskInputSubmitMsg` 事件发送
11. `internal/git/sync.go`（新） — `Syncer` 结构体：异步 `Sync()`、进度回调、`git clone --depth 1`、force-pull + stash 保护、子模块/LFS 支持
12. `internal/git/progress.go`（新） — `SyncProgress` 类型
13. `internal/scheduler/` — 扩展：新增 `GitSyncJob` 类型和 `GitSyncRunner` 接口
14. `internal/config/config.go` — 同步添加 `git` 配置块

### Phase 4：题目管理页（2-3 天）

15. `internal/tui/taskcard.go`（新） — 单题卡片渲染（7 种变体，含截断）
16. `internal/tui/tasklist.go`（新） — 单列滚动列表组件（独立滚动，位置保持）
17. `internal/tui/taskboard.go`（新） — 三栏布局组合、列间导航、响应式降级
18. `internal/tui/render.go` — 新增 taskboard 渲染函数
19. `internal/tui/viewmodel.go` — 扩展 `overviewItem`，添加 `TaskProject` 映射
20. `internal/tui/localize.go` — 新增 task 状态中文本地化

### Phase 5：设置浮层 + 总览页调整（1.5-2 天）

21. `internal/tui/settings_overlay.go`（新） — 设置浮层（从 `settings.go` + `docker_mirror.go` 提取合并）
22. `internal/tui/settings.go` — 标记废弃，删除或委托到 `settings_overlay.go`
23. `internal/tui/docker_mirror.go` — 逻辑层保留（Docker 镜像配置操作），UI 层合并入 `settings_overlay.go`
24. `internal/tui/overview.go` — 微调（新增 `task_id` 列、`completion_count` 列、状态着色、JOIN tasks、task state 过滤、completion_count 排序）
25. `internal/tui/keymap.go` — 更新快捷键绑定（新增 Ctrl+G、Ctrl+E scope、`/` 聚焦输入框）

### Phase 6：Pipeline 调整 + Docker 标签（2-3 天）

26. `internal/pipeline/model/stages.go` — 重新排序 `stageSpecs` 数组 + Order 字段
27. `internal/pipeline/stage.go` — `defaultRunStages()` 移除 E 排除；`staticStageSet()` 移除 E 排除；`StageOutcome` 添加 `SkipNextStage`
28. `internal/pipeline/run_lifecycle.go` — `executeStageLoop` 添加 `SkipNextStage` 检查；**移除**自动 Docker 清理触发；集成 task 状态更新
29. `internal/pipeline/cleanup.go` — `runtimeCleanupPoint()` 改为始终返回 false；新增 `StopDockerRuntime()`；`finalizeRuntime()` 拆分
30. `internal/pipeline/stage_b.go` — 失败模式分类处理（F1-F6 vs F7）；添加 compose meta 写入；`docker compose up` 添加 `--label`
31. `internal/pipeline/stage_c.go` — 测试产物清理
32. `internal/docker/compose.go` — `ComposeCommandArgsWithProjectDir` 添加 labels 参数
33. `internal/docker/manager.go` — 新增 `IsRunning()`、`GetFrontendURL()`、`ListAllProjects()`
34. `internal/docker/gc.go` — 新增 `ForceExitCleanup()`、`LightExitCleanup()`
35. `internal/pipeline/lifecycle.go` — `finishAbortedRun()` 添加 `current_run_id` 清除
36. `internal/pipeline/recovery.go` — `RecoverStaleRuns` 完成后添加 tasks 状态修复

### Phase 7：TUI 退出清理 + 集成（1.5-2 天）

37. `internal/tui/app.go` — 退出确认弹窗 + ForceExitCleanup/LightExitCleanup 调用；SIGTERM/SIGINT 信号处理
38. TUI 启动时遗留容器检测 + tasks 状态修复
39. `internal/tui/layout.go` — 三栏布局宽度计算和降级逻辑

### Phase 8：测试（3-4 天）

40. task 状态机转换测试（含 Docker 成功/失败/部分失败/abort/crash 路径）
41. 数据库 migration 测试（v4→v5 升级、干净安装、数据回填验证）
42. Git 同步单元测试 + mock 测试（含超时、认证失败、中断克隆恢复）
43. TUI 组件渲染 golden/snapshot 测试（7 种 task 卡片变体、三栏布局、降级视图）
44. Docker 清理集成测试（StopDockerRuntime、ForceExitCleanup、LightExitCleanup、partial failure recovery）
45. 退出确认弹窗与清理逻辑测试
46. 并发任务创建/完成测试（`current_run_id` CAS、`completion_count` 原子递增）
47. 端到端手动测试（按场景脚本执行）

### 回滚策略

- 各 Phase 独立可回滚（git revert 对应 Phase commits）
- Phase 0（验证）不产生破坏性变更
- Phase 1-3 为新增代码，删除新文件即可回滚
- Phase 6 schema migration（v5）不可自动回滚：需手动执行 `DROP TABLE tasks; DROP TABLE batches;` + 恢复 `stage.go` 原始代码
- Phase 6 pipeline 变更回滚需同时恢复 Stage Order + `defaultRunStages()` E 排除 + `runtimeCleanupPoint()` 原始逻辑

---

## 九、文件变更总览

### 新增文件

```
internal/
├── git/
│   ├── sync.go                          # Git 同步
│   └── progress.go                      # SyncProgress 类型
├── tui/
│   ├── router.go                        # 页面路由 + Overlay 管理
│   ├── focus.go                         # FocusStack 焦点管理
│   ├── poller.go                        # 调度器轮询 + tasks 修复
│   ├── pipelinebar.go                   # 流水线状态栏
│   ├── shared.go                        # 共享接口 + TaskProject 视图模型
│   ├── taskboard.go                     # 题目管理页（三栏布局）
│   ├── taskcard.go                      # 单题卡片渲染（7 种变体）
│   ├── tasklist.go                      # 单列滚动列表
│   ├── taskinput.go                     # 全局输入框（含 TASK ID 校验）
│   └── settings_overlay.go             # 设置浮层
```

### 修改文件

```
internal/
├── pipeline/
│   ├── model/
│   │   ├── model.go                     # 添加 Task、Batch、ComposeMeta、SkipNextStage
│   │   └── stages.go                    # 调整 Order + 重新排序数组
│   ├── stage.go                         # 移除 E 排除；StageOutcome 新增字段
│   ├── stage_b.go                       # 失败分类处理 + compose meta 写入 + 标签
│   ├── stage_c.go                       # 测试产物清理
│   ├── run_lifecycle.go                 # 移除自动 Docker 清理 + SkipNextStage + task 状态写入
│   ├── cleanup.go                       # StopDockerRuntime + ForceExitCleanup + runtimeCleanupPoint 禁用
│   ├── pipeline.go                      # runStore 接口扩展
│   └── lifecycle.go                     # finishAbortedRun 添加 task 清理
│   └── recovery.go                      # RecoverStaleRuns 添加 task 修复
├── docker/
│   ├── manager.go                       # IsRunning、GetFrontendURL、ListAllProjects
│   ├── compose.go                       # labels 参数
│   └── gc.go                            # ForceExitCleanup + LightExitCleanup
├── db/
│   ├── migrate.go                       # v5 migration + PRAGMA foreign_keys
│   ├── store.go                         # Task + Batch CRUD + FinishRunAndTransitionTask
│   └── query.go                         # 新查询方法（参数化）
├── config/
│   └── config.go                        # GitConfig 配置块
├── scheduler/
│   └── scheduler.go                     # GitSyncJob 类型 + GitSyncRunner 接口
└── tui/
    ├── app.go                           # 重构：新结构体 + DI + 退出逻辑 + 信号处理
    ├── overview.go                      # 微调：新列 + 状态着色 + JOIN tasks + 过滤/排序
    ├── keymap.go                        # 新快捷键（Ctrl+G, /, updated scopes）
    ├── layout.go                        # 三栏布局 + 降级
    ├── render.go                        # 新 taskboard 渲染函数
    ├── viewmodel.go                     # 扩展视图模型 + TaskProject 映射
    └── localize.go                      # 新状态中文本地化
```

### 可能废弃/合并的文件

```
internal/tui/
├── settings.go         → settings_overlay.go 替代（全屏页签 → 浮层）
├── docker_mirror.go    → 逻辑层保留，UI 层合并入 settings_overlay.go
├── pipelineview.go     → pipelinebar.go 替代（职责抽离）
└── runconfig.go        → 保留（Ctrl+R 在扫描发现项目上仍然触发运行配置面板）
```

---

## 十、风险与注意事项

1. **Batch 管理原子性**：Batch 分配 + task 创建必须在单个 `withWriteTx` 事务内完成，消除 TOCTOU race。
2. **Git 操作超时**：大仓库 clone 可能耗时数分钟。Git 同步作为独立的 scheduler Job 执行，TUI 以非阻塞方式展示进度（通过 `SyncCallback`）。
3. **并发控制**：同一题目不能被并发质检，通过 `tasks.current_run_id` CAS 操作（`WHERE current_run_id IS NULL`）互斥检查。
4. **双页面数据一致性**：题目管理页和总览页共享 `TaskQueryService`。状态变更后由 app 协调器检查变更类型，按需触发两个页面的刷新命令。
5. **Stage 顺序调整对 findings 的影响**：findings 展示顺序跟随 `stageOrderCaseSQL()`，该函数自动跟随 `AllStageSpecs()` 的 Order 字段。Stage F 在缺少 B/C 阶段的 findings 会导致报告信息量降低，此为预期行为。
6. **向后兼容**：旧 runs 数据无 `completion_round` 字段，migration 设置 DEFAULT 1。`projects` 表保留，Overview 页 LEFT JOIN 查询兼容无 task 行的旧项目。
7. **GitLab 可用性依赖**：clone 失败给出清晰错误，不阻塞 TUI 其他操作。`Ctrl+G` 提供重试机制。
8. **三栏布局降级**：终端宽度 ≤ 110 列时降级为单栏 Tab 视图。降级阈值高于原先的 100 列，确保内容不溢出。
9. **退出清理尽力而为**：`ForceExitCleanup()` 采用 best-effort 策略，单步失败不阻塞退出但记录日志。清理检查点文件支持重启后恢复。
10. **设置浮层与主页面交互**：浮层通过 `InterceptsAllKeys()` 捕获所有键盘事件，仅 `Esc`/`Ctrl+/` 关闭，`Q` 关闭浮层但不退出 TUI。
11. **Docker 标签**：所有 compose 启动的容器必须标记 `managed_by=p2rqa=true`，否则基于标签的 prune 命令为无操作。
12. **TASK ID 安全校验**：输入框严格校验格式 `TASK-\d{8}-[A-F0-9]{6}`，防止路径遍历和注入攻击。
13. **`frontend_url` 单一 URL 假设**：多服务 compose 项目仅存储第一个 HTTP 服务 URL。质检员可从该 URL 开始手动测试其他服务。
14. **Stage E 首次纳入默认流水线**：存在 profile 输出格式兼容性风险，Phase 0 已纳入前置验证。
15. **`keepRuntime` 配置废弃**：task 驱动模式下忽略 `keepRuntime` 配置（Docker 始终保持运行直到 Ctrl+E）。扫描驱动模式继续遵守此配置。

---

## 十一、未决事项

以下事项需在实际实现过程中根据代码细节进行决策：

1. 三栏布局每列宽度的**精确像素分配**及列分隔线绘制方式（考虑 lipgloss Border 占用的列宽）
2. 设置浮层的**精确尺寸和位置**（公式：宽 = clamp(floor(w*0.35), 40, 60)，高 = min(contentRows, max(10, h*0.6))）
3. 等待时长的**刷新粒度**（已确认：1 秒）
4. 确认弹窗的**UI 样式**（已确认：居中模态框，Y/N 选择）
5. 终端宽度降级阈值和单栏样式（已确认：≤ 110 列触发降级）
6. 等待时长超过 99:59 时的显示格式（建议：超过 99 小时使用 `H:MM:SS` 格式或 `3d 2h` 缩写）
7. `completion_round` 的显示位置（是否需要在对历史 run 详情页展示轮次信息）
