# PRD: TUI 渲染修复、流水线模式绑定与 Codex 集成改进

日期: 2026-04-30
状态: 待实现

---

## 背景

基于对 p2r_tui 当前代码、测试与本机 Codex CLI 行为的 Ralph 复核，发现三个需要改进的问题。本文档是在原 PRD 基础上的修订版，重点修正了原草案中的几个风险点：

- `renderConfirm()` 也独立调用 `affectedStages()`，所以不能只改 `runSelected()`，否则确认框和真实执行会不一致。
- `initial` 模式不应通过显式传入 A-F 来实现全量运行，因为 `pipeline.selectedStages()` 在 `len(opts.Stages) > 0` 时会绕过既有 `static_only` 语义。
- 左侧面板溢出不只来自 6 个 stage 行，还包括 recheck 下可能很长的参考运行列表。
- `codex exec --help` 内只列出 `resume/review/help` 是 `exec` 子命令自己的嵌套子命令列表，不代表顶层没有 `exec`。
- `--full-auto` 在本机 `codex-cli 0.125.0` 中会把有效沙箱变成 `workspace-write`，不能作为 p2r 静态审查的自动审批 fallback。

---

## 改项 1: 质检模式与全量/增量运行绑定

### 当前行为

`runSelected()` (app.go:223) 统一使用 `affectedStages()` 决定运行哪些 stage，不区分首次质检还是打回重检。用户按 `Ctrl+R` 确认重跑后，实际只会执行当前选中 stage 的影响链。

例如：当前选中 Stage A → `affectedStages("A")` 返回 `["A", "F"]` → B/C/D/E 全部以 "Not selected for this run." 被跳过。

### 问题

`affectedStages()` 是为增量重跑设计的，但对首次运行完全不适用。用户若在首次质检时触发重跑，会发现大部分 stage 被跳过，且错误信息 "Not selected for this run." 没有解释清楚原因。

### 改进方案

在 TUI 层新增一个共享的 stage plan helper，让确认框和真实执行使用同一份策略：

| QA 模式 | 行为 | 运行的 Stage |
|---------|------|-------------|
| `initial`（首次质检） | 使用 pipeline 默认选择逻辑 | runtime 配置下 A-F；`static_only` 配置下 A/D/E/F |
| `recheck`（打回重检） | 增量重跑，从当前 stage 起计算影响链 | 由 `affectedStages()` 决定 |

修改位置:
- `internal/tui/app.go` 的 `runSelected()` 函数
- `internal/tui/render.go` 的 `renderConfirm()` 函数
- `internal/tui/viewmodel.go` 或新文件中的共享 helper

```go
type stagePlan struct {
    runStages     []string // nil 表示交给 pipeline 默认选择，保留 static_only 语义
    displayStages []string
    blockedReason string
}

func stagePlanForMode(mode, stage string, staticOnly bool) stagePlan {
    if mode == "recheck" {
        if staticOnly && (stage == "B" || stage == "C") {
            return stagePlan{blockedReason: "static-only 模式不能重跑 runtime 阶段 B/C"}
        }
        stages := affectedStages(stage)
        return stagePlan{runStages: stages, displayStages: stages}
    }
    if staticOnly {
        return stagePlan{displayStages: []string{"A", "D", "E", "F"}}
    }
    return stagePlan{displayStages: []string{"A", "B", "C", "D", "E", "F"}}
}
```

`openRerunConfirm()` 应先检查 `plan.blockedReason`，非空时显示提示并拒绝打开确认框。`runSelected()` 应传入 `plan.runStages`。`initial` 模式下该值保持 `nil`，让 `pipeline.selectedStages()` 继续处理默认全量运行和 `cfg.Pipeline.StaticOnly`。`renderConfirm()` 应显示 `plan.displayStages`，不再直接调用 `affectedStages()`。

如果后续要支持 TUI 内显式切换 static-only/runtime 模式，应在这个 helper 中扩展，而不是在 `runSelected()` 和 `renderConfirm()` 各写一份逻辑。

### 影响

- `recheck` 模式行为不变（增量重跑）
- `initial` 模式下首次运行不再有 stage 被意外跳过
- `initial` 模式保留 `pipeline.static_only` 既有契约，不会意外启动 B/C runtime 阶段
- `recheck` 模式在 `static_only` 配置下不会误触发 B/C runtime 阶段
- 确认对话框与真实执行使用同一个 stage plan，避免“显示 A-F、实际只跑 A/F”或反向不一致

### 测试要求

- 新增 helper 级测试：`initial + staticOnly=false` 显示 A-F 且 `runStages == nil`。
- 新增 helper 级测试：`initial + staticOnly=true` 显示 A/D/E/F 且 `runStages == nil`。
- 新增 helper 级测试：`recheck + Stage A` 显示并运行 A/F。
- 新增 helper 级测试：`recheck + staticOnly=true + Stage B/C` 返回 `blockedReason`，不返回可执行 stage。
- 新增 TUI 确认框测试：`renderConfirm()` 使用同一 stage plan，不能再直接依赖 `affectedStages()`。

---

## 改项 2: 左侧菜单面板渲染溢出修复

### 当前行为

执行详情页（`renderExecution`, render.go:48）的左侧面板通过 `renderExecutionLeft()` 渲染所有阶段行作为纯文本，无 viewport 滚动机制。该内容被放入 `panelStyle.Height(contentHeight)` 容器后与右侧 viewport 面板用 `lipgloss.JoinHorizontal` 拼接。

### 问题

当终端高度较小时，左侧面板内容（任务信息 + 6 个阶段行 + 可能的参考运行列表）的总行数超过 panel 的 height 约束。lipgloss 的 `Height()` 在某些情况下无法正确裁剪内容，导致左侧面板上半部分内容溢出到终端 header 区域。

用户在阶段列表中使用上下键切换阶段时，问题更为明显——因为每次 `moveStage()` 会触发 `updateDetailContent()` 重渲染右侧 viewport，而左侧面板没有对应的视口偏移机制。

### 根因分析

关键代码路径：

1. `renderExecutionLeft()` (render.go:72) — 无高度参数，无条件渲染全部阶段行
2. `renderExecution()` (render.go:55) — 左侧面板传入固定 `contentHeight`，不感知内容实际行数
3. 左侧面板缺少 viewport 滚动机制，`stageIndex` 仅用于高亮，不影响哪些行被渲染

### 改进方案（轻量方案）

在 `renderExecutionLeft` 中根据可用高度计算可见行范围，配合当前焦点和选中 index 做派生视口偏移。不要依赖 `lipgloss.Height()` 裁剪超高内容；传入面板内部可用高度后，函数自身必须保证输出行数不超过该高度。

核心逻辑：

1. 在 `renderExecution()` 中传入面板内部高度：

```
leftContentHeight = max(1, layout.contentHeight - panelStyle.GetVerticalFrameSize())
```

2. `renderExecutionLeft()` 内部保留固定信息行，再给 stage 列表和 ref run 列表分配剩余高度。

3. 阶段行做视口裁剪：

```
可见起始 = max(0, stageIndex - 可见行数 + 1)  // 确保当前选中行在可见范围内
可见结束 = min(总阶段数, 可见起始 + 可见行数)
```

4. 参考运行列表也必须裁剪，且在 `focusRefRunList` 时优先保证 `refIndex` 可见。原 PRD 只裁剪 stage 行是不完整的，recheck 历史运行较多时仍会溢出。

5. 如果内容超出可视范围，在顶部和/或底部显示 `↑` / `↓` 提示。最后再做一次 `splitNonEmptyLines`/`strings.Split` 级别的安全截断，确保返回内容行数不超过 `maxHeight`。

修改位置: `internal/tui/render.go` 的 `renderExecutionLeft()` 函数签名和实现。

```go
// 改造前
func renderExecutionLeft(m app, width int) string

// 改造后
func renderExecutionLeft(m app, width int, maxHeight int) string
```

### 影响

- 仅影响执行详情页左侧面板渲染
- 概览页不受影响，stacked/minimal 布局仍走 `renderStageSummary()`
- 右侧 viewport 已有独立滚动机制，不受影响
- 在极小高度下优先保证任务、模式、当前 stage/ref run 可见，低优先级行可被裁剪

### 测试要求

- 扩展现有 `TestExecutionRenderDoesNotExceedViewportWidth`，同时断言 `lipgloss.Height(view) <= height`，覆盖 `120x12`、`100x12`、`90x12` 等 wide/medium 小高度。
- 新增 recheck 参考运行列表测试：构造 20 个 ref run，选择靠后的 `refIndex`，确认渲染高度不超限且当前 ref run 可见。
- 新增 stage 列表测试：小高度下移动到 Stage F，确认 Stage F 可见且顶部有上滚提示。

---

## 改项 3: Codex CLI 集成修复

### 环境确认与修正

本次复核环境：

- `codex-cli 0.125.0`
- 顶层 `codex --help` 明确列出 `exec`
- `codex exec --help` 支持非交互执行所需的主要 flag

当前环境关键 flag：

| Flag | 是否支持 |
|------|---------|
| `exec` 子命令 | 支持 |
| `--ask-for-approval` / `-a` | 支持 |
| `-c, --config` | 支持 |
| `--sandbox` (read-only/workspace-write/danger-full-access) | 支持 |
| `--full-auto` | 支持，但**不得用于 p2r 静态审查** |
| `--dangerously-bypass-approvals-and-sandbox` | 支持 |
| `-C, --cd` | 支持 |
| `--ephemeral` | 支持 |
| `--skip-git-repo-check` | 支持 |
| `--ignore-user-config` | 支持 |

### 问题 3a: `exec` 子命令误判

当前 `DetectCLI()` (cli.go:54) 执行 `codex exec --help` 来检测 flag。`codex exec --help` 输出中的 `resume`、`review`、`help` 是 `exec` 子命令下的嵌套子命令，不是顶层命令列表。因此“没有再次列出 exec”不能作为 exec 不存在的证据。

p2r 需要非交互执行、stdin prompt、stdout 报告捕获，因此默认仍应使用 `codex exec ... -`。不要退回到不带 `exec` 的交互 TUI 入口，除非未来版本明确提供等价的非交互能力并有测试覆盖。

### 问题 3b: `approval_policy` fallback 需要保留

当前 fallback 路径 (cli.go:96-97):
```go
} else if cap.HasConfig {
    args = append(args, "-c", `approval_policy="never"`)
}
```

本机 `codex debug prompt-input -c approval_policy=never -c sandbox_mode='read-only'` 的开发者上下文显示有效权限为 `sandbox_mode=read-only` 且审批策略为 `never`，说明当前版本中 `approval_policy` 仍是有效 top-level config key。该 fallback 应保留，但优先级低于显式 `--ask-for-approval never`。

### 问题 3c: `--full-auto` 是安全风险，不应使用

`--full-auto` 的帮助描述：

> Convenience alias for low-friction sandboxed automatic execution

原 PRD 认为它适合 p2r，但复核发现这个结论不成立。本机执行：

```bash
codex --full-auto --sandbox read-only debug prompt-input 'hello'
```

输出的权限上下文显示有效 `sandbox_mode` 为 `workspace-write`，即使命令行同时带了 `--sandbox read-only`。这会允许 Codex 修改当前工作区，违反 D/E/F “纯静态审查、不修改文件”的边界。

因此：

- `--full-auto` 可以被检测并记录到 capability summary，方便诊断。
- `BuildExecArgs()` 不得把 `--full-auto` 作为 `--ask-for-approval` 或 `-c approval_policy` 的 fallback。
- `safeCodexExtraArgs()` 和 preflight 的 `validateExtraArgs()` 必须把 `--full-auto` 加入黑名单，防止用户通过 `codex.extra_args` 覆盖只读边界。

### 改进方案

**步骤 1**: 在 `ApplyExecHelp()` 中添加 `--full-auto` 检测，仅用于日志和诊断：

```go
func ApplyExecHelp(cap *Capability, help string) {
    // ... 现有检测 ...
    cap.HasFullAuto = hasHelpToken(help, "--full-auto")
}
```

在 `Capability` struct 中添加对应字段。

**步骤 2**: 修改 `BuildExecArgs()` 中的审批策略，明确不使用 `--full-auto`：

```go
// 优先级: --ask-for-approval > -c approval_policy
if cap.HasAskForApproval {
    args = append(args, "--ask-for-approval", "never")
} else if cap.HasConfig {
    args = append(args, "-c", `approval_policy="never"`)
} else {
    return nil, fmt.Errorf("codex exec exposes neither --ask-for-approval nor -c/--config; cannot force approval_policy=never")
}
```

`BuildExecArgs()` 仍必须始终添加：

```go
args := []string{"exec"}
args = append(args, "--sandbox", "read-only")
```

如果 `codex exec --help` 不可用，应让 D/E/F 写 unavailable-review，而不是调用交互入口。

**步骤 3**: 更新黑名单：

- `internal/pipeline/stage_codex.go` 的 `safeCodexExtraArgs()`
- `internal/preflight/preflight.go` 的 `validateExtraArgs()`

新增拒绝：

```go
"--full-auto": true,
"--search": true,
```

继续保留 `--dangerously-bypass-approvals-and-sandbox`、`--sandbox`、`--ask-for-approval`、`-a`、`-c`、`--config`、`--cd`、`-C`、`--add-dir` 等边界变更参数的黑名单。

**步骤 4**: `capabilitySummary()` 增加 `full_auto=%t`，日志中能解释“可用但未使用”。

修改文件:
- `internal/codex/cli.go` — Capability struct、ApplyExecHelp、BuildExecArgs
- `internal/pipeline/stage_codex.go` — safeCodexExtraArgs(), capabilitySummary()
- `internal/preflight/preflight.go` — validateExtraArgs()
- `tests/internal/codex/cli_test.go` — fake help / arg builder 测试
- `tests/internal/pipeline/pipeline_test.go` — extra_args 黑名单测试

### 关于 `--dangerously-bypass-approvals-and-sandbox`

该 flag 会同时绕过审批和沙箱，p2r 不需要也不应使用它。在 `safeCodexExtraArgs` 中保留其黑名单是正确的。`--full-auto` 虽然不像该 flag 一样完全关闭沙箱，但会把有效沙箱放宽到 `workspace-write`，也必须禁用。

---

## 验证计划

| # | 验证项 | 方法 |
|---|-------|------|
| 1 | initial 模式全量运行 | 对一个无历史 Run 的任务按 Ctrl+R 确认，确认对话框显示 A-F；`runSelected()` 传 `Stages:nil`，pipeline 生成 A-F |
| 2 | recheck 模式增量运行 | 切换到 recheck 模式，选中 Stage A，确认对话框只显示 A、F |
| 3 | initial + static_only | 配置 `pipeline.static_only=true`，确认框显示 A/D/E/F；真实运行不执行 B/C |
| 4 | recheck + static_only 阻断 | 配置 `pipeline.static_only=true`，recheck 选中 B/C 时确认框不打开，并显示不能重跑 runtime 阶段 |
| 5 | 左侧面板渲染 | 在 120x12、100x12、90x12 下进入执行详情页，上下切换阶段和参考运行，确认左侧面板不溢出且选中项可见 |
| 6 | Codex 自动审批 | fake help 覆盖 `--ask-for-approval` 路径和 `-c approval_policy` fallback；命令必须包含 `exec --sandbox read-only`，不得包含 `--full-auto` |
| 7 | Codex extra_args 安全 | `--full-auto`、`--search`、`--dangerously-bypass-approvals-and-sandbox` 均被 preflight 和 runtime validator 拒绝 |
| 8 | Codex 本机诊断 | 可手动运行 `codex --sandbox read-only -c approval_policy=never debug prompt-input 'hello'`，确认有效权限为 read-only/never；`--full-auto` 诊断仅作为禁用依据，不作为集成路径 |
| 9 | 回归测试 | `go test ./...` |

---

## 相关文件

- `internal/tui/app.go` — runSelected(), enterExecution()
- `internal/tui/render.go` — renderExecutionLeft(), renderExecution()
- `internal/tui/viewmodel.go` — affectedStages()
- `internal/tui/layout.go` — layoutFor(), renderPanel()
- `internal/codex/cli.go` — Capability, DetectCLI, BuildExecArgs, ApplyExecHelp
- `internal/pipeline/stage_codex.go` — stageCodex(), runCodexWithLog()
- `internal/preflight/preflight.go` — validateExtraArgs()
- `tests/internal/tui/*` — stage plan、confirm、渲染高度测试
- `tests/internal/codex/cli_test.go` — Codex capability 和 args 测试
- `tests/internal/pipeline/pipeline_test.go` — Codex extra_args 安全测试
