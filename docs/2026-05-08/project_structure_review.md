# p2r_tui 项目结构评审

> 评审日期：2026-05-08
> 评审方式：oh my codex Ralph 循环（Read -> Analyze -> Link -> Plan -> Harden）
> 评审范围：仓库源码、测试、文档与构建入口。统计口径以 `git ls-files` 与 `internal/`、`cmd/`、`tests/` 为准，排除 `.go-cache/`、`.gomodcache/`、`projects-qa/` 等本地运行产物。

---

## 0. Ralph 复核结论

本轮先读取原评审文档，再对照 `go.mod`、`cmd/`、`internal/config`、`internal/db`、`internal/pipeline`、`internal/scheduler`、`internal/tui`、`internal/projectlayout`、`internal/scanner` 与测试目录逐项核对。原文方向基本正确，但存在几处会误导后续重构的缺陷，本文已直接修正：

1. **事实口径过期**：原文称“42 个 Go 源文件 + 27 个测试文件”。当前主体口径为 `internal/` 47 个 Go 文件、`cmd/` 9 个 Go 文件、`tests/` 27 个测试文件；全仓库被 git 跟踪的 Go 文件合计 85 个，另外 2 个是根 `main.go` 与 `assets/embed.go`。若粗暴 `find . -name '*.go'` 会把 `.gomodcache/` 计入，得到完全失真的数字。
2. **接口结论过绝对**：项目并非“没有任何 interface”，`pipeline.CodexReviewSession` 已经是一个好的端口抽象；真正的问题是核心边界 `db.Store`、`scheduler.Scheduler`、`pipeline.Runner`、`executor.Runner` 仍以具体类型在上层扩散。
3. **pipeline 状态已变化**：`internal/pipeline/pipeline.go` 已从旧文档中的超大单文件拆到多个同包文件，但 `Run()` 仍承担 run 准备、阶段编排、持久化、cleanup 与终态收口，仍是后续重构核心。
4. **配置风险低估**：手写 YAML 不只是“维护成本高”，还会吞掉无效数字、无效布尔值、未知字段和部分语法错误；`bufio.Scanner` 默认 token 限制也可能让长行配置异常。
5. **阶段建模风险低估**：Stage 字符串分散在 `pipeline`、`model`、`cmd`、`tui`、`preflight`、`db SQL` 与测试中。新增或改名 stage 时，当前没有一个单一事实源。
6. **运行中断场景缺口**：TUI 侧已有 scheduler cancel/recovery 设计，但 CLI `p2r run` 仍使用 `context.Background()`，没有 `signal.NotifyContext`；`scan`、`status` 的 DB 操作也直接使用 background context。用户在 CLI 中按 Ctrl+C 时可能跳过 pipeline abort/cleanup 收口，只能等待 stale-run recovery 兜底。
7. **非正常收口仍有漏洞**：`finishAbortedRun()` 在真正 `FinishRun()` 成功前就把 `runFinished` 置为 true，且只返回原始 cancel error；`markRunCrashed()` 使用无超时的 background context 并忽略 DB/artifact 写入错误。遇到 SQLite 短暂阻塞或 artifact 写失败时，run 可能仍停在 running 或缺少 crash/abort 证据。
8. **scheduler 取消语义混杂**：`CancelTask()` 把排队取消和运行中取消都呈现为 `JobFailed + ErrJobCancelledByUser`，而 pipeline 侧有明确的 `RunAborted`。UI 和后续自动化会难以区分“用户主动取消”和“基础设施失败”。
9. **测试边界脆弱**：测试中有 36 处 `go:linkname` 直连私有符号，短期利于覆盖，长期会冻结内部结构，阻碍优雅拆分。
10. **仓库卫生问题遗漏**：根目录存在已跟踪的 18M `p2r` 二进制构建产物，本地还有 `.go-cache/`、`.gomodcache/`。后两者已被 `.gitignore` 忽略，但仍会污染人工统计和扫描；前者建议移出版本库或明确作为发布物管理。

---

## 一、体（Body）—— 架构与领域建模

### 做得好的地方

**领域主线清晰。** `Project -> Run -> Stage -> Finding` 的模型在 `internal/pipeline/model/model.go` 中表达直接，`RunRunning`、`RunCompletedClean`、`RunCrashed`、`RunAborted` 等状态常量避免了大部分魔法字符串。

**Go 布局整体规范。** `cmd/` 作为 Cobra CLI 入口，`internal/` 封装私有实现，`assets/` 使用 embed 资源，`tests/` 做外部包测试，整体符合 Go 项目习惯。

**流水线阶段已完成第一轮同包拆分。** `stage_a.go`、`stage_b.go`、`stage_c.go`、`stage_codex.go`、`stage_f.go`、`compose.go`、`runtime_evidence.go`、`cleanup.go` 等文件已经把旧版 `pipeline.go` 的大块职责拆出，可读性比早期计划好很多。

**项目根规则已经集中。** `internal/projectlayout` 统一了 batch/task 命名、canonical package root、original session marker 和 package root validation。Scanner 与 Pipeline 都复用了它，这是近期重构里最有价值的收束之一。

### 问题 1：核心端口抽象不足，具体类型向上泄漏

当前情况：

- `pipeline.Runner` 持有 `*db.Store` 与具体 `executor.Runner`，`NewRunner()` 内部直接 `executor.New()`。
- `scheduler.Scheduler` 持有 `*db.Store`，并在 `runJob()` 中直接调用 `pipeline.NewRunner(s.store, s.cfg).Run(...)`。
- `tui.app` 持有 `*db.Store` 与 `*scheduler.Scheduler`，测试只能用真实 store 或 UI testhook。
- `cmd/run.go` 直接创建 store 和 runner。

影响：

- 单元测试容易退化为集成测试，必须创建 SQLite 文件、真实目录和较完整的 pipeline package。
- TUI、Scheduler、Pipeline 难以分别替换为 fake/mock 以验证异常路径。
- 后续想引入远程 runner、dry-run runner、只读 store 或内存 store 时改动面会偏大。

重构建议：

1. **按消费者定义小接口，而不是在 `db` 包导出巨型 Store 接口。**
   - `pipeline` 内定义 `runStore`，只包含 `GetProject`、`GetRun`、`CreateRun`、`PutStage`、`InsertFindings`、`FinishRun` 等实际需要的方法。
   - `scheduler` 内定义 `RunnerFactory` 或 `RunExecutor`，让 scheduler 不直接知道 `pipeline.NewRunner`。
   - `tui` 内定义 `OverviewStore`、`ExecutionStore`、`SchedulerClient`，只覆盖视图需要的方法。
2. **给 `Runner` 增加 options 构造，而不是扩散构造函数参数。**

```go
type RunnerOption func(*Runner)

func WithExecutor(exec commandRunner) RunnerOption
func NewRunner(store runStore, cfg config.Config, opts ...RunnerOption) Runner
```

3. **保留 `CodexReviewSession` 的模式。** 这是现有代码里端口抽象最成熟的例子，后续接口应该采用同样的小而具体的风格。

### 问题 2：`Runner.Run()` 仍是系统收口瓶颈

`internal/pipeline/pipeline.go` 当前约 835 行，其中 `Run()` 约 220 行。虽然 stage、compose、artifact IO 已拆出，但 `Run()` 仍同时处理：

- 项目读取与 canonical path 校正
- 运行参数校验
- task lock
- artifact root 创建
- assets release 与 docs import
- run/stage DB 持久化
- preflight
- stage 循环
- runtime cleanup
- abort/crash recovery
- run terminal status

风险：

- 任意一个细节变更都要进入 `Run()`，容易引入状态机回归。
- 正常完成、取消、panic、cleanup failure 的持久化策略分散。
- required artifact 与 best-effort artifact 的错误等级不清晰。
- `finishAbortedRun()` 过早设置 `runFinished`，如果后续 `FinishRun()` 失败，defer 不会再把 run 标成 crashed，调用方也只拿到 context cancel error。

重构建议：

将 `Run()` 拆成同包私有阶段，不先拆子包：

```text
loadAndPrepareRun()       # project、opts、lock、artifact root、manifest 前置
persistInitialStages()    # initial stage records + status file
executeStageLoop()        # stage transition + progress event
finalizeRuntimeCleanup()  # cleanup summary + cleanup finding
finishRun()               # terminal run status
abortRun() / crashRun()   # 非正常收口，必须返回持久化错误
```

这一轮只做方法抽取和状态对象整理，不改变数据库 schema，不移动包边界。等 `Run()` 降到 80-120 行后，再决定是否把 stage registry、artifact writer 或 cleanup coordinator 拆成独立类型。

### 问题 3：CLI 与 TUI 的中断语义不一致

TUI 已有 `scheduler.CancelTask()`、`finishAbortedRun()` 与 stale-run recovery；但 `cmd/run.go` 使用 `context.Background()`：

```go
result, err := runner.Run(context.Background(), args[0], ...)
```

这意味着 CLI `p2r run` 收到 SIGINT/SIGTERM 时，默认进程退出可能不会走 pipeline 的 abort cleanup、stage 持久化和 lock release。`cmd/scan.go`、`cmd/status.go` 也没有从 Cobra command context 取取消信号。现有 recovery 可以兜底，但它是“事后修复”，不是正常控制流。

重构建议：

- 在 Cobra root 或 run command 层建立 `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`。
- `cmd/run.go`、`cmd/scan.go`、`cmd/status.go` 统一使用 command context。
- `p2r run` 收到取消后应复用 `finishAbortedRun()`，输出明确的 aborted 信息。
- 增加 CLI e2e 测试：启动长跑 Stage A/D，取消 context 后 run 状态为 `aborted`，task lock 被释放，必要 cleanup 被记录。

---

## 二、面（Surface）—— 模块耦合与包设计

### 做得好的地方

**依赖方向没有明显反向依赖。** `cmd -> internal/*`，`tui -> scheduler/db/pipeline`，`scheduler -> pipeline/db`，`pipeline -> db/executor/config/projectlayout`，目前未发现循环依赖。

**平台差异处理得体。** `internal/pipeline/process_unix.go`、`process_windows.go` 与 `internal/executor/process_*` 使用 build tag 分离进程终止细节。

**Scanner 与 ProjectLayout 的边界清楚。** `scanner.Scan()` 只识别 canonical package，不再从 `.git`、`package.json` 等通用 repo 标记推断项目根，避免误扫运行产物。

### 问题 4：Stage 字符串散落，没有单一事实源

当前 `"A"` 到 `"F"` 分散在：

- `internal/pipeline/model/stages.go`：display name
- `internal/pipeline/pipeline.go`：`executeStage`、`selectedStages`、`initialStages`、`stageLogPath`
- `internal/pipeline/cleanup.go`：runtime cleanup point
- `internal/preflight/preflight.go`：check affects stages
- `cmd/run.go`：`validStage`
- `internal/tui/stage_plan.go`、`viewmodel.go`、`localize.go`
- `internal/db/store.go`：SQL `CASE s.stage WHEN 'A' ...`

风险：

- 新增 stage 或改名时必须改多处 switch/数组/SQL。
- DB 排序、TUI 展示、pipeline 执行顺序可能发生隐性不一致。
- `StageRecord.Stage string` 无法阻止无效值，CLI 只校验 `--stage/--from`，但内部 `RunOptions{Stage: "Z"}` 会退化成“只跑 F、A-E skipped”，`RunOptions{Stages: []string{"Z"}}` 会让 A-F 全部 skipped。这个静默降级比显式 unknown stage 更危险。

重构建议：

在 `internal/pipeline/model` 或新建 `internal/stages` 中建立 stage registry：

```go
type StageID string

const (
	StageA StageID = "A"
	StageB StageID = "B"
	StageC StageID = "C"
	StageD StageID = "D"
	StageE StageID = "E"
	StageF StageID = "F"
)

type StageSpec struct {
	ID       StageID
	Order    int
	Name     string
	Runtime  bool
	Static   bool
	LogName  string
}
```

迁移顺序：

1. 保持 DB 与 JSON 仍写字符串，只在 Go 内部引入 `StageID`。
2. 用 `AllStages()`、`ParseStageID()`、`StageLogName()` 替代散落数组和 switch。
3. `selectedStages()`、TUI run config、CLI flag 与 config stage timeout 都必须调用同一个 parser；无效 stage 在入口直接返回错误。
4. DB SQL 的 stage order 由 registry 生成固定 CASE 片段，避免手写两套顺序。
5. TUI 与 CLI 通过 registry 做校验、排序和本地化映射。

### 问题 5：`absClean` 重复，路径工具边界未统一

`internal/pipeline/pipeline.go` 与 `internal/db/store.go` 都定义了相同的 `absClean()`。这不是大问题，但它暴露出路径判断逻辑分散：`pathWithin`、`relUnderRoot`、artifact path pruning、safe path segment 分布在多个包。

重构建议：

- 新建 `internal/pathutil`，只放低层、无业务含义的函数：`AbsClean`、`RelUnderRoot`、`PathWithin`。
- 业务规则继续留在 `projectlayout`，例如 `ExpectedProjectPath`、`ValidatePackageRoot`、`SafePathSegment`。
- 先迁移 `absClean` 与 `relUnderRoot`，不要把所有路径逻辑都塞进一个杂物包。

### 问题 6：仓库入口和产物边界不够干净

当前同时存在：

- 根目录 `main.go`
- `cmd/p2r/main.go`
- 根目录已跟踪二进制 `p2r`
- README 推荐 `go install ./cmd/p2r`

风险：

- 两个 main 入口行为目前相同，但后续可能漂移。
- 根目录二进制会增加仓库体积，也容易让扫描、发布、代码审查误判。

重构建议：

- 明确唯一安装入口为 `./cmd/p2r`。若保留根 `main.go`，README 需要解释它只是兼容入口；否则删除根入口。
- 将构建产物放入 `dist/` 并确保被 `.gitignore` 忽略。
- 若 `p2r` 二进制已被 git 跟踪，单独做一次“移除构建产物”的提交，不与功能重构混在一起。

---

## 三、线（Line）—— 函数关系与控制流

### 做得好的地方

**取消与恢复路径已经有明确雏形。** `finishAbortedRun()` 用 bounded background context 保存 stage、findings、cleanup 与 run terminal status，避免父 ctx 已取消后 DB 写入失败。这个方向正确，但还需要把 terminal persistence 的错误显式抬出来。

**Codex review session 抽象是可复用样板。** `CodexReviewSession` 把 app-server start/guidance/wait 拆出，测试可用 fake session 覆盖 guidance deadline，这是项目里最优雅的测试端口。

**错误消息多数带上下文。** 如 canonical project path 失败、Codex capability 失败、static review schema 失败，错误能告诉用户下一步怎么修。

### 问题 7：artifact 写入错误大量 best-effort 化，缺少分级策略

`internal/pipeline` 中仍有大量 `_ = writeText(...)`、`_ = writeJSON(...)`、`_ = appendText(...)`、`_ = r.store.PutStage(...)`、`_ = r.store.InsertFindings(...)`。按当前粗略匹配，pipeline 内与 artifact/DB/cleanup 相关的忽略点超过 60 处，其中有些是可接受的 best-effort，有些会直接导致证据缺失。

风险：

- 用户看到 stage done，但关键 artifact 未落盘。
- `stage_status.json` 写入失败与“该 stage 没有状态变化”无法区分。
- cleanup finding 插入失败会让 infra 风险消失。

重构建议：

建立 artifact 写入分级：

```go
type ArtifactWriter struct {
	Root string
}

func (w ArtifactWriter) RequiredJSON(path string, value any) error
func (w ArtifactWriter) RequiredText(path, content string) error
func (w ArtifactWriter) BestEffortJSON(path string, value any) ArtifactWarning
func (w ArtifactWriter) BestEffortText(path, content string) ArtifactWarning
```

落地规则：

- `run_manifest.json`、`stage_status.json`、stage 主报告、`port_map.json`、`cleanup_summary.json` 是 required 或至少要进入 `record.ErrorSummary`。
- 兼容文件、日志补写、截图 fallback 可 best-effort，但 warning 要进入 manifest 或 stage log。
- DB 状态写入不能静默；如果 stage DB 写失败，应让 run 进入 crashed 或明确的 infra finding。

### 问题 8：cleanup 收口逻辑重复，且状态语义混在 `Run()`

正常流程中 runtime cleanup 会在 B/C 后触发，也会在 stage 循环结束后兜底；取消路径在 `finishAbortedRun()` 中再次实现 cleanup + manifest merge + finding 插入。结构相似，但分散在多处。

重构建议：

抽取一个统一 helper：

```go
type cleanupOutcome struct {
	Summary       dockermgr.CleanupSummary
	Finding       *model.Finding
	Warnings      []string
	PersistErrors []error
}

func (r Runner) finalizeRuntime(ctx context.Context, run model.RunRecord, stages []model.StageRecord, keepRuntime bool, reason string) cleanupOutcome
```

调用方只决定使用当前 ctx 还是短 background ctx；cleanup 自己负责：

- 是否适用
- 写 `cleanup_summary.json`
- merge `run_manifest.json`
- 生成 cleanup finding
- 返回 artifact/DB 持久化错误，交给 `finishRun()`、`abortRun()`、`crashRun()` 决定终态

### 问题 9：Progress reporter 事件是字符串，状态机不够显式

`RunProgress.Event` 使用 `"run_created"`、`"stage_running"`、`"stage_done"`、`"run_done"`、`"run_crashed"`、`"cleanup"` 等字符串。Scheduler 与 TUI 通过这些字符串更新 job 状态。

风险：

- 编译器无法发现拼写错误。
- `run_done` 同时承载正常完成和 aborted 完成，必须结合 `Err` 推断。
- 事件语义与数据载荷耦合在一个结构里，后续扩展容易变成 if/switch 堆叠。
- Scheduler 把用户取消映射成 `JobFailed`，与真实 pipeline failure 共享 UI/自动化语义。

重构建议：

- 引入 `type ProgressEvent string` 常量。
- 将 run terminal event 分为 `RunCompleted`、`RunAborted`、`RunCrashed`。
- Scheduler 只依赖稳定事件常量，不直接比较裸字符串。
- Scheduler 增加 `JobCancelled` 或 `JobAborted`，并从 `RunRecord.Status` 映射终态，避免把主动取消当作失败。
- TUI 文案层根据 `RunRecord.Status` 和 `JobState` 决定展示，不从 event 字符串推断业务含义。

### 问题 10：TUI `Update()` 仍是消息路由中心，后续扩展成本高

`internal/tui/app.go` 约 619 行，`Update()` 将窗口、overview、detail、scheduler、recovery、tick、key 输入全部放在一个 switch 中。当前可读，但再加入批量操作、更多 modal 或后台任务后会迅速变脆。

重构建议：

- 保留 Bubble Tea 架构，不引入新框架。
- 将 switch 分支拆成小方法：`handleOverviewMsg`、`handleSchedulerMsg`、`handleRecoveryMsg`、`handleKeyMsg`。
- 把 `schedulerJobsMsg` 到 `activeJobs/message/layout` 的更新规则集中，避免消息覆盖。
- 给 `app` 注入 `SchedulerClient`，TUI 单元测试不再依赖真实 scheduler。

---

## 四、点（Point）—— 代码细节与安全性

### 做得好的地方

**SQLite 配置适合本地 CLI。** WAL、NORMAL synchronous、busy timeout、有限连接池，对本地多读少写的 TUI/CLI 场景是合理选择。

**Codex sandbox 边界有主动防御。** 当前强制 `approval_policy=never`、`sandbox_mode=read-only`，拒绝 unsupported `extra_args`，并把外部文档包在 untrusted boundary 中，这些决策是正确的。

**配置加载优先级清晰。** 默认值、配置文件、环境变量、CLI flag 的覆盖顺序明确，配置文件中的相对路径按配置文件目录解析，环境变量/flag 按当前工作目录解析。

### 问题 11：手写 YAML 解析器会静默接受坏配置

`internal/config/config.go` 通过 `bufio.Scanner`、`section`、`subSection`、indent 手动解析 YAML。主要风险：

- `parseInt("abc", fallback)` 静默回退默认值。
- `parseBool("maybe")` 静默变成 false。
- 未知 key、错误缩进、复杂 YAML 语法不会报错。
- `stripComment()` 会截断引号内的 `#`。
- 默认 scanner token 限制可能导致长行失败。
- 数值范围缺少集中校验，`docs.max_attachment_bytes <= 0`、`codex.max_output_bytes <= 0`、负 timeout 等配置错误可能一路传到运行时才暴露。

重构建议：

引入 `gopkg.in/yaml.v3`，使用 raw config + merge default：

```go
type rawConfig struct {
	ScanPath *string `yaml:"scan_path"`
	DBPath   *string `yaml:"db_path"`
	Pipeline *rawPipelineConfig `yaml:"pipeline"`
}
```

落地要求：

- `yaml.Decoder.KnownFields(true)`，未知字段直接报错。
- 数字/布尔解析失败直接返回带路径的错误，如 `pipeline.max_concurrent must be integer`。
- stage timeout key 通过 stage registry 校验，允许 `B_PULL` 这类子超时，但拒绝未知主 stage。
- 保留现有路径基准目录语义与环境变量引用 `${VAR}`。
- 增加 `Validate()`：统一检查 timeout、docs/codex byte limit、Docker cleanup policy、network policy、max concurrency 等业务约束。

### 问题 12：SQL 拼接模式当前安全，但未来容易被误用

`baseProjectRowsSQL(where string)` 用 `fmt.Sprintf` 注入 WHERE 子句。当前 where 来自内部 `projectSearchPredicate`，参数值仍走占位符，因此暂无直接 SQL 注入。但这个函数签名会鼓励未来调用方传任意字符串。

重构建议：

- 将 `where string` 替换为内部 query builder 类型：

```go
type projectWhere struct {
	SQL  string
	Args []any
}
```

- `baseProjectRowsSQL` 不接受外部字符串，只接受受控 enum/filter。
- 排序字段已经用 `ProjectSort` enum 控制，应继续保持。

### 问题 13：测试大量依赖 `go:linkname`

当前 pipeline、TUI 与 preflight 测试通过 36 处 `go:linkname` 访问私有函数，例如 `selectedStages`、`parseComposePS`、`safeCodexExtraArgs`、`stageLogPreview`、localize helpers、`validateExtraArgs` 等。

风险：

- 私有函数签名被测试冻结，重构时成本异常高。
- `unsafe` 链接绕开包边界，降低测试对真实公共行为的约束力。
- IDE/静态分析对这些隐式依赖不友好。

重构建议：

- 对纯函数，如果确实值得单测，迁移为同包测试文件或提升到更合适的小包。
- 对行为逻辑，优先通过公共 API 或小接口 fake 验证。
- 保留少量 `go:linkname` 只用于无法通过公共路径触达的遗留函数，并建立迁移清单。

### 问题 14：本地缓存与构建产物会污染项目结构判断

`.go-cache/`、`.gomodcache/` 已在 `.gitignore` 中，但仍在工作目录内；根目录 18M 的 `p2r` 二进制被 git 跟踪。对一个“扫描 delivery package 并生成证据”的工具而言，仓库自身体积和运行产物边界很重要。

重构建议：

- 文档和脚本统计源码时统一使用 `git ls-files` 或显式排除本地缓存。
- 把 `p2r` 二进制从版本库移除，发布产物进入 `dist/`。
- CI 增加轻量检查：禁止提交根目录二进制、`.go-cache`、`.gomodcache`、`projects-qa`。

---

## 重构路线图（按收益/风险排序）

| 优先级 | 改进项 | 范围 | 预期收益 | 风险 |
|--------|--------|------|----------|------|
| P0 | CLI 使用 signal-aware context | `cmd/root.go`、`cmd/run.go`、测试 | Ctrl+C 能进入 abort/cleanup 正常收口 | 低 |
| P0 | 修正 abort/crash terminal persistence | `internal/pipeline/pipeline.go`、恢复测试 | 取消/崩溃时不会遗留 running run，持久化错误可见 | 中 |
| P0 | 配置解析改为 `yaml.v3` 且严格校验 | `internal/config`、配置测试 | 修复静默吞错和 YAML 脆弱解析 | 中 |
| P1 | Stage registry / `StageID` | `model`/新 `stages` 包、pipeline、tui、cmd、db SQL | 消除 stage 字符串分散，新增 stage 可控 | 中 |
| P1 | `Run()` 拆为 prepare/execute/finalize/abort | `internal/pipeline/pipeline.go` | 降低状态机复杂度，便于测试异常路径 | 中 |
| P1 | Artifact 写入分级 | `internal/pipeline/artifact_io.go` 与 stage 文件 | 关键产物缺失可见，减少假成功 | 中 |
| P2 | Scheduler 注入 runner factory | `internal/scheduler`、测试 | Scheduler 可单测，不依赖真实 pipeline | 低 |
| P2 | TUI 拆分消息 handler 并注入接口 | `internal/tui/app.go` | 降低 UI 扩展成本，减少 testhook | 中 |
| P2 | `internal/pathutil` 收束基础路径函数 | `pipeline`、`db`、`projectlayout` | 消除重复，统一路径判定 | 低 |
| P3 | 移除 `go:linkname` 测试依赖 | `tests/internal/*` | 重构自由度更高，测试边界更健康 | 中 |
| P3 | 清理仓库构建产物 | 根目录、CI/脚本 | 降低仓库噪音和扫描误判 | 低 |

---

## 建议的第一轮实施切片

第一轮不要同时做所有架构改造。建议只做下面四件事，每件都能独立验证：

1. **CLI signal context**
   - 给 root command 或 run command 加 `signal.NotifyContext`。
   - `p2r run` 取消后 run 状态为 `aborted`，lock 被释放。
   - 同时修正 `finishAbortedRun()`：只有 `FinishRun()` 成功后才设置 `runFinished`，否则返回 persistence error 并让 recovery 能识别。

2. **严格配置解析**
   - 引入 `yaml.v3`。
   - 新增测试覆盖无效 int、无效 bool、未知 key、引号内 `#`、stage timeout key。

3. **Stage registry 最小落地**
   - 先集中 `AllStages`、`ParseStageID`、`DisplayName`、`LogPath`。
   - 不改 DB schema，不一次性重写所有 stage 逻辑。

4. **Artifact required/best-effort 分类**
   - 先从 `run_manifest.json`、`stage_status.json`、Stage D/E/F 主报告开始。
   - 失败时返回错误或写入 infra finding，避免 stage 假成功。

---

## 综合评分（当前状态）

| 维度 | 满分 | 得分 | 说明 |
|------|------|------|------|
| 稳定性 | 40 | 28 | 取消、panic recovery、stale-run recovery 已有基础；但 CLI signal、abort/crash 持久化、artifact 写入分级、DB 写入忽略仍是风险 |
| 可维护性 | 30 | 21 | 包布局和阶段拆分不错；Stage 字符串、`Run()` 过载、具体类型耦合仍限制演进 |
| 可测试性 | 20 | 12 | 覆盖面不低；但真实 SQLite/文件系统依赖重，`go:linkname` 冻结内部结构 |
| 仓库卫生 | 10 | 6 | `.gitignore` 覆盖本地缓存；但根目录二进制和双 main 入口需要清理或说明 |

**总分：67/100**

这个分数不是说项目差，而是说明它已经越过“能跑”的阶段，进入“需要把边界打磨清楚”的阶段。当前最值得投资的是：严格配置、signal-aware CLI、abort/crash 终态持久化、Stage registry、Runner 状态机拆分。它们能同时提高健壮性、测试可控性和后续重构自由度。
