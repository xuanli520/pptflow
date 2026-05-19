# 质检规则调整实施计划

## 背景

题目方规则调整，需对 TUI 和流水线进行三项改动：
1. 所有产物文件名不再添加 `QA_` 前缀
2. `codex_report.md` 不再作为必须提交的产物，但仍复制到 submit 目录作为质检证据
3. 首次质检（initial）和打回重检（recheck）默认不勾选 Stage E（静态验收审查），用户仍可手动勾选
4. Stage F 不再将 Stage E 的发现作为输入，只使用用户上传文档 + metadata.json 等上下文
5. Stage F 删除 self-test 报告作为输入来源

不考虑旧产物兼容，文件名是什么就显示什么。

---

## 改动 A：去掉 QA_ 前缀

### A1. `internal/pipeline/artifact_names.go`

**当前逻辑**：`qaArtifactName()` 对所有产物名添加 `QA_` 前缀，`qaArtifactPath()` 基于 `qaArtifactName()` 生成完整路径。

**改动**：
- 删除 `qaArtifactPrefix` 常量
- `qaArtifactName(name)` 改为直接返回 `name`（去掉前缀逻辑）
- `qaArtifactPath(root, name)` 改为 `filepath.Join(root, name)`（不再经过 `qaArtifactName` 的前缀处理）

> 注意：由于大量代码调用 `qaArtifactName()` / `qaArtifactPath()`，这两个函数保留但逻辑简化，避免大面积重构调用方。

### A2. `internal/tui/viewmodel.go`

**当前逻辑**：`qaArtifactCandidates(names)` 为每个名称生成 `QA_` 前缀和无前缀两个候选名，用于搜索产物文件。

**改动**：
- `qaArtifactCandidates(names)` 简化为直接返回 `names` 列表（不再生成 `QA_` 前缀候选）
- `isShortCommentArtifact(base)` 去掉 `QA_short_comment.txt` 判断，只保留 `short_comment.txt`

### A3. `internal/pipeline/submit.go`

**当前逻辑**：`submitArtifactSpecs()` 中所有产物名通过 `qaArtifactName()` 生成，结果带 `QA_` 前缀。

**改动**：
- 由于 `qaArtifactName()` 已简化为直接返回原名（见 A1），调用 `qaArtifactName()` 的结果不再带前缀，无需额外修改 submit 中的调用方式
- 但需调整 `submitArtifactSpecs()` 中 codex_report 的地位（见改动 B）

### A4. `internal/pipeline/stage_a.go`

**当前逻辑**：`missingValidationReportFinding()` 的 Title 和 Rule 字段硬编码了 `QA_validation_report.md`。

**改动**：
- Title 改为 `"run_validate.py did not emit validation_report.md"`
- Rule 改为 `"validation_report.md must be produced by run_validate.py, not by run_acceptance.py or a pipeline fallback."`
- MinimumFix 中 `filepath.Base(path)` 部分无需修改（因为 path 本身不再含 QA_ 前缀）

### A5. 测试文件更新

所有测试文件中硬编码的 `QA_` 前缀产物名需要去掉：

- `tests/internal/pipeline/pipeline_test.go` — submit 产物名断言、manifest 内容断言
- `tests/internal/pipeline/stage_codex_test.go` — Stage F 产物名断言、Stage E 报告路径断言、compat 警告路径断言
- `tests/internal/pipeline/stage_a_test.go` — Stage A 产物名断言、validation report 路径断言
- `tests/internal/pipeline/pipeline_e2e_test.go` — e2e 测试报告路径断言
- `tests/internal/tui/viewmodel_test.go` — viewmodel 中报告路径断言

---

## 改动 B：codex_report 不再作为必须提交产物

### B1. `internal/pipeline/submit.go`

**当前逻辑**：initial 模式中 codex_report.md（Stage E）是 submit 列表中的产物之一，和 Stage A/D/F/B/C 的产物并列。

**改动**：
- 在 `submitArtifactSpec` 结构体中增加 `Optional bool` 字段
- initial 模式的 `submitArtifactSpecs()` 中 codex_report.md 对应的 spec 设置 `Optional: true`
- `aggregateSubmitArtifacts()` 中，Optional 产物在 Stage 被选中但文件不存在时，不记录为错误，而是记录为 `item.OK = false` 但 `item.NotSelected = false`，同时在 submit_manifest.json 中标注 `optional: true`

> 具体：当 `selected[spec.Stage]` 为 true 且文件不存在时，Optional spec 的 `item.Error` 设为空字符串（而非 os.Stat 的错误），manifest 中标注 `optional: true` 和 `ok: false`。

---

## 改动 C：Stage E 默认不勾选

### C1. `internal/tui/stage_plan.go`

**当前逻辑**：`stagePlanForMode()` 在 initial 模式下无显式多选时返回 `model.AllStages()`（A~F 全选），recheck 模式下返回 `affectedStages(stage)`（含 E）。

**改动**：
- 新增辅助函数 `withoutStageE(stages []string) []string`，从阶段列表中过滤掉 "E"
- initial 模式：`displayStages` 从 `model.AllStages()` 改为 `withoutStageE(model.AllStages())`
- recheck 模式：`displayStages` 从 `affectedStages(stage)` 改为 `withoutStageE(affectedStages(stage))`
- `staticDisplayStages()` 也需要 `withoutStageE` 处理（因为 E 是 static stage）

### C2. `internal/tui/runconfig.go`

**当前逻辑**：`defaultStageSet()` 基于 `stagePlanForMode()` 生成默认勾选集。

**改动**：由于 `stagePlanForMode()` 已在 C1 中调整，`defaultStageSet()` 自动继承新逻辑，无需额外修改。

但需确认：当用户手动切换到 recheck 模式时，`toggleRunConfigFocused()` 中 `m.runConfig.stages = stageSet(affectedStages(...))` 也应排除 E。这里 `affectedStages()` 返回值会经过 `stagePlanForMode()` 处理，需确保一致性。

### C3. 测试文件更新

- `tests/internal/pipeline/pipeline_test.go` — 验证默认阶段集不含 E
- `tests/internal/tui/` — 验证 runConfig 默认勾选不含 E

---

## 改动 D：Stage F 不依赖 Stage E 的发现

### D1. `internal/pipeline/stage_f.go`

**当前逻辑**：
- `priorStageSnapshot(prior)` 遍历 `["A", "B", "C", "D", "E"]` 收集 findings
- `stageFPreviousFindingsContext()` 输出 A~E 的阶段状态和 findings
- `writeRepairSupplements()` 将 findings 写入 repair_summary.json

**改动**：
- `priorStageSnapshot()` 的遍历列表改为 `["A", "B", "C", "D"]`（去掉 "E"）
- `stageFPreviousFindingsContext()` 的遍历列表改为 `["A", "B", "C", "D"]`
- Stage F 的 CodexReviewSpec 中 `BuildContext` 不再注入 E 的 findings

### D2. `internal/pipeline/stage_codex.go`

**当前逻辑**：`refRunStaticContext()` 为 Stage F 构建参考运行上下文时，引用 E 的产物名（`qaArtifactName("codex_report.md")`、`qaArtifactName("codex_report_verification.md")`）。

**改动**：
- Stage F 的 `refRunStaticContext()` names 列表中去掉 E 的产物名（codex_report.md、codex_report_verification.md）
- 同时去掉 `qaArtifactName("operator_codex_report_issues_verification.md")` 和 `qaArtifactName("codex_report_issues_verification.md")`（这些是 Stage F 自己的产物，但在参考运行中读取 E 的报告来交叉验证——既然 E 不运行了，不需要）

### D3. 测试文件更新

- `tests/internal/pipeline/stage_codex_test.go` — Stage F 上下文断言不再包含 E findings

---

## 改动 E：Stage F 删除 self-test 输入来源

### E1. `internal/pipeline/stage_codex.go`

**当前逻辑**：`codexContext()` 中 Stage F 的分支调用 `r.selfTestReportContext(project)` 获取自测报告内容。

**改动**：
- 在 `codexContext()` 的 `stage == "F"` 分支中，删除 `selfTestPath, content, err := r.selfTestReportContext(project)` 调用及其对应的 `builder.WriteString(untrustedDocument(...))`
- 删除对应的 `self-test report was not available` fallback 写入
- `selfTestReportContext()` 函数本身保留（Stage D 可能仍使用），但 Stage F 不再调用

### E2. 测试文件更新

- `tests/internal/pipeline/stage_codex_test.go` — `TestCodexContextOnlyExposesUploadedDocsToStageF` 中断言 Stage F 上下文不再包含 `SELF TEST CLAIM`

---

## 改动汇总清单

| 文件 | 改动类型 | 涉及内容 |
|------|----------|----------|
| `internal/pipeline/artifact_names.go` | A1 | 删除 QA_ 前缀逻辑 |
| `internal/tui/viewmodel.go` | A2 | 简化 qaArtifactCandidates、isShortCommentArtifact |
| `internal/pipeline/submit.go` | A3+B1 | 产物名不再带前缀（自动继承 A1）；codex_report 标记 Optional |
| `internal/pipeline/stage_a.go` | A4 | finding 中硬编码 QA_ 名称改为无前缀 |
| `internal/pipeline/stage_f.go` | C1+D1 | priorStageSnapshot 和 findingsContext 去掉 E |
| `internal/pipeline/stage_codex.go` | D2+E1 | refRunStaticContext 去掉 E 产物；Stage F 删除 self-test 输入 |
| `internal/tui/stage_plan.go` | C1 | 默认阶段集排除 E |
| `internal/tui/runconfig.go` | C2 | 确认继承 stage_plan 逻辑 |
| `tests/internal/pipeline/pipeline_test.go` | A5+B+C | 产物名断言、submit 断言、默认阶段断言 |
| `tests/internal/pipeline/stage_codex_test.go` | A5+D+E | 产物名断言、Stage F 上下文断言 |
| `tests/internal/pipeline/stage_a_test.go` | A5 | 产物名断言 |
| `tests/internal/pipeline/pipeline_e2e_test.go` | A5 | 产物路径断言 |
| `tests/internal/tui/viewmodel_test.go` | A5 | 报告路径断言 |

---

## 验证方法

1. 运行全部测试 `go test ./tests/...` 确认所有改动通过
2. 启动 TUI `p2r tui`，选择一个任务，按 Ctrl+R 启动首次质检：
   - 确认阶段多选列表中 Stage E 默认未勾选
   - 确认可以手动勾选 Stage E
   - 确认所有产物文件名不含 QA_ 前缀
3. 运行一次完整质检（不含 E），确认：
   - submit 目录中 codex_report.md 不存在时不报错
   - Stage F 的 Codex 上下文不含 E findings 和 self-test 报告
4. 切换到 recheck 模式，确认 Stage E 默认也未勾选