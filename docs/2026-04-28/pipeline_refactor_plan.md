# pipeline.go 重构计划

## 现状

`internal/pipeline/pipeline.go` 当前共 **2235 行**。文件同时承载 Runner 调度、6 个阶段实现、Docker Compose 参数与端口解析、Codex 沙箱调用、安全上下文拼接、运行时证据转换、报告汇总、findings 处理、文件 IO 等职责。

设计文档 `docs/p2r_cli_设计.md` 第 3 节已经给出 `internal/pipeline/` 的分文件方向；当前代码还没有完成这一步。`internal/pipeline/render.go` 与 `internal/pipeline/model/model.go` 已经存在，本次重构应在现有结构上收束，而不是重新设计目录。

### 当前职责分布

| 职责类别 | 当前包含内容 |
|----------|--------------|
| Runner 调度 + 状态机 | `RunOptions`, `Result`, `Runner`, `NewRunner`, `SelfTestReportPath`, `normalizeRunOptions`, `Run`, `executeStage`, `selectedStages`, `initialStages`, `stageName`, `stageTimeout`, `startStage`, `finishStage`, `skippedStage`, `blockedStage`, `materializeSkippedStage`, `writeRunManifest`, `writeStageStatus`, `runStatus` |
| Stage A | `stageA`, `structuralFindings`, `runStageAScripts`, `runStageAScript`, `scriptExecution`, `pythonInvocation`, `acceptanceScriptArgs`, `projectTypeArgs`, `promptLooksEnglish`, `acceptanceFindings`, `issueFindings`, `issueString`, `acceptanceSeverity`, `hasHardStageAFailure`, `validationMarkdown` |
| Stage B | `stageB`, `dockerPortFallback`, `failB` |
| Docker Compose 工具 | `portMapping`, `probeResult`, `composePSService`, `findCompose`, `readmeComposeCommand`, `composeArgsWithProject`, `composePSArgs`, `composePSQArgs`, `composeServicesArgs`, `composeGlobals`, `composeCommandIndex`, `splitNonEmptyLines`, `hasFlag`, `indexOf`, `parseComposePS`, `parseDockerPort`, `probeMappings` |
| Stage C | `stageC`, `containerRunTestsCommand`, `findRunTests` |
| 运行时证据 | `runtimeEvidence`, `readRuntimeEvidence`, `serviceURLEnv`, `serviceURL`, `serviceURLEnvironment`, `preferredServiceURL`, `normalizeServiceURL`, `normalizeHost`, `sanitizeEnvKey` |
| Stage D/E (Codex) | `stageCodex`, `codexPrompt`, `codexContext`, `readBoundedText`, `untrustedDocument`, `safeCodexExtraArgs`, `sha256Text`, `configuredEnvKeys`, `staticUnavailableReport` |
| Stage F | `stageF`, `repairMarkdown`, `shortComment` |
| Findings 处理 | `assignFindingIDs`, `severityShort`, `extractFindingsFromReport`, `countSeverity`, `highestRisk`, `sortFindings`, `severityRank` |
| Artifact / 文件 IO | `writeJSON`, `writeText`, `appendText`, `copyPackageSnapshot`, `copyFile`, `dirExists`, `fileExists` |
| 文本 / 小工具 | `truncateString`, `firstNonEmpty`, `minDuration`, `renderLogFile` |

### 核心问题

1. **单文件职责过多**：一个文件承载了流水线调度、阶段实现、Docker 编排、Codex 沙箱、报告生成和文件 IO。
2. **修改原因混杂**：修改 Compose 解析、Codex 安全边界或 Stage F 文案时都需要触碰 `pipeline.go`。
3. **旧计划已过期**：原计划按 1839 行库存设计，漏掉了 `codexContext`、Codex 安全参数过滤、runtime service URL 环境变量、`renderLogFile` 等当前代码块。
4. **`util.go` 风险过高**：把所有 IO、字符串和通用 helper 放进 `util.go` 会制造新的杂物间，不利于后续收敛。
5. **测试依赖私有符号名**：`tests/internal/pipeline/pipeline_test.go` 使用 `go:linkname` 直连 package-private 函数。第一阶段必须保持函数名、签名和 `package pipeline` 不变。

---

## 重构策略

本次只做 **Phase 1：同包机械拆分**。

目标是恢复职责边界和可读性，不改变行为、不修 bug、不引入抽象、不拆子包。后续如果要真正降低耦合，应在 Phase 1 通过后再做 Phase 2。

### Phase 1 原则

1. **只移动，不改逻辑**：不改任何函数内部行为，不趁机修 bug，不重写算法。
2. **保持同包**：所有新增文件都使用 `package pipeline`，不移动到 `internal/pipeline/compose` 等子包。
3. **保持私有符号稳定**：不改函数名、类型名和签名，避免破坏 `go:linkname` 测试。
4. **不新增公共接口**：不导出新类型，不增加接口层，不做依赖注入改造。
5. **避免“万能 util”**：按修改原因命名文件，例如 `artifact_io.go`、`runtime_evidence.go`，而不是把杂项塞进 `util.go`。
6. **每步 gofmt + 可验证**：每次移动后运行 `gofmt`，能跑测试时跑测试；如果依赖下载阻塞，记录为验证阻塞而不是假装通过。

### Phase 2 暂不做

这些方向可以在 Phase 1 通过后单独规划：

- 把 Compose 逻辑下沉到 `internal/docker` 或独立子包。
- 把 `go:linkname` 测试迁回同包测试或改为公共行为测试。
- 减少不必要的 `Runner` receiver，让纯函数更纯。
- 用小型配置结构替代 D/E 的字符串参数组。

---

## 目标结构

Phase 1 目标结构：

```text
internal/pipeline/
├── pipeline.go            # Runner、Run、阶段选择、状态机、manifest/status
├── render.go              # 已存在：终端日志渲染；补入 renderLogFile
├── artifact_io.go         # artifact 写入、快照复制、基础文件存在性检查
├── text_helpers.go        # 少量跨阶段字符串/时间 helper
├── finding.go             # findings 提取、ID 分配、排序、风险统计
├── compose.go             # Docker Compose 命令构造、端口解析、健康探测
├── runtime_evidence.go    # port_map 读取、service URL 环境变量生成
├── stage_a.go             # Stage A 结构与规则脚本检查
├── stage_b.go             # Stage B Docker 运行证据采集
├── stage_c.go             # Stage C run_tests 运行证据采集
├── stage_codex.go         # Stage D/E 共享 Codex 静态审查引擎与安全边界
├── stage_f.go             # Stage F 汇总报告 + short_comment
└── model/
    └── model.go           # 已存在：阶段状态、artifact、finding schema
```

`stage_d.go` / `stage_e.go` 不作为 Phase 1 必选项。两个文件只会承载极薄 wrapper，解耦价值有限。若后续必须严格贴合 `docs/p2r_cli_设计.md` 的文件清单，可以在不改行为的前提下单独添加。

### 各文件职责

#### `pipeline.go` — Runner 调度核心

- `RunOptions`, `Result`, `Runner`
- `NewRunner()`, `SelfTestReportPath()`
- `normalizeRunOptions()`
- `Run()`
- `executeStage()`
- `selectedStages()`, `initialStages()`, `stageName()`, `stageTimeout()`
- `startStage()`, `finishStage()`, `skippedStage()`, `blockedStage()`
- `materializeSkippedStage()`
- `writeRunManifest()`, `writeStageStatus()`, `runStatus()`

#### `render.go` — 日志截图渲染

- 现有 `renderTerminalLog()`
- 现有 ANSI stripping / basic font 转换函数
- 移入 `renderLogFile()`

#### `artifact_io.go` — Artifact 与文件 IO

- `writeJSON()`, `writeText()`, `appendText()`
- `copyPackageSnapshot()`, `copyFile()`
- `dirExists()`, `fileExists()`

不放入 `readBoundedText()`。它服务于 Codex 安全上下文，应留在 `stage_codex.go` 附近。

#### `text_helpers.go` — 小型跨阶段 helper

- `truncateString()`
- `firstNonEmpty()`

`minDuration()` 目前只服务 Stage B 健康检查，优先放入 `stage_b.go`。如果后续出现第二个使用点，再移动到 `text_helpers.go`。

#### `finding.go` — Findings 处理

- `assignFindingIDs()`
- `severityShort()`
- `extractFindingsFromReport()`
- `countSeverity()`
- `highestRisk()`
- `sortFindings()`
- `severityRank()`

#### `compose.go` — Docker Compose 工具

- `portMapping`, `probeResult`, `composePSService`
- `findCompose()`
- `readmeComposeCommand()` 与 `readmes()`
- `composeArgsWithProject()`
- `composePSArgs()`, `composePSQArgs()`, `composeServicesArgs()`, `composeGlobals()`
- `composeCommandIndex()`
- `splitNonEmptyLines()`, `hasFlag()`, `indexOf()`
- `parseComposePS()`
- `parseDockerPort()`
- `probeMappings()`

`readmes()` 放在这里，因为它只支撑 README 中 Compose 命令发现。

#### `runtime_evidence.go` — 运行时证据与服务 URL

- `runtimeEvidence`
- `readRuntimeEvidence()`
- `serviceURLEnv`, `serviceURL`
- `serviceURLEnvironment()`
- `preferredServiceURL()`
- `normalizeServiceURL()`
- `normalizeHost()`
- `sanitizeEnvKey()`

这部分不放入 `compose.go`。它的职责不是解析 Compose，而是把 Stage B 产物转换成 Stage C 可消费的运行时环境。

#### `stage_a.go` — 结构与规则检查

- `stageA()`
- `structuralFindings()`
- `runStageAScripts()`, `runStageAScript()`
- `acceptanceScriptArgs()`
- `scriptExecution` + `logBlock()` + `summary()`
- `pythonInvocation()`
- `projectTypeArgs()`
- `promptLooksEnglish()`
- `acceptanceFindings()`
- `issueFindings()`, `issueString()`, `acceptanceSeverity()`
- `hasHardStageAFailure()`
- `validationMarkdown()`

#### `stage_b.go` — Docker 运行证据采集

- `stageB()`
- `dockerPortFallback()`
- `failB()`
- `minDuration()`

#### `stage_c.go` — run_tests 运行证据采集

- `stageC()`
- `containerRunTestsCommand()`
- `findRunTests()`

#### `stage_codex.go` — Codex 静态审查与安全边界

- `stageCodex()`
- `codexPrompt()`
- `codexContext()`
- `readBoundedText()`
- `untrustedDocument()`
- `safeCodexExtraArgs()`
- `sha256Text()`
- `configuredEnvKeys()`
- `staticUnavailableReport()`

这些函数一起保留，因为它们共同表达 Codex 调用的信任边界。

#### `stage_f.go` — 汇总报告

- `stageF()`
- `repairMarkdown()`
- `shortComment()`

---

## 推荐执行步骤

按叶子函数先行、阶段入口后移的顺序执行，降低 import 调整和冲突概率。

| 步骤 | 操作 | 验证 |
|------|------|------|
| 0 | 建立基线：确认当前 `go test` 是否能跑；若依赖下载超时，记录阻塞原因 | `go test ./...` 或记录网络/module cache blocker |
| 1 | 将 `renderLogFile()` 移入已有 `render.go` | `gofmt -w internal/pipeline`; pipeline 相关测试能跑则执行 |
| 2 | 创建 `artifact_io.go`，移入 artifact/file IO 函数 | 同上 |
| 3 | 创建 `text_helpers.go`，移入跨阶段文本 helper | 同上 |
| 4 | 创建 `finding.go`，移入 findings 处理函数 | 同上 |
| 5 | 创建 `compose.go`，移入 Compose 类型、命令构造、端口解析、健康探测 | 同上 |
| 6 | 创建 `runtime_evidence.go`，移入 runtime evidence 与 service URL 环境变量逻辑 | 同上 |
| 7 | 创建 `stage_b.go`，移入 Stage B 入口和失败处理 | 同上 |
| 8 | 创建 `stage_c.go`，移入 Stage C 入口和 run_tests 发现逻辑 | 同上 |
| 9 | 创建 `stage_a.go`，移入 Stage A 入口、脚本执行和 acceptance 解析 | 同上 |
| 10 | 创建 `stage_codex.go`，移入 D/E Codex 共享引擎和安全边界函数 | 同上 |
| 11 | 创建 `stage_f.go`，移入汇总报告逻辑 | 同上 |
| 12 | 清理 `pipeline.go` imports，确认只剩调度核心 | `gofmt`; `go test ./internal/pipeline ./tests/internal/pipeline`; 最后跑全量验证 |

每一步只移动完整函数或完整类型定义，不在移动过程中编辑函数体。

### 依赖关系

```text
pipeline.go
  ├── stage_a.go
  │   └── artifact_io.go
  ├── stage_b.go
  │   ├── compose.go
  │   ├── artifact_io.go
  │   ├── render.go
  │   └── text_helpers.go
  ├── stage_c.go
  │   ├── runtime_evidence.go
  │   ├── artifact_io.go
  │   └── render.go
  ├── stage_codex.go
  │   ├── artifact_io.go
  │   └── text_helpers.go
  ├── stage_f.go
  │   ├── finding.go
  │   ├── artifact_io.go
  │   └── text_helpers.go
  ├── finding.go
  ├── compose.go
  ├── runtime_evidence.go
  ├── render.go
  └── artifact_io.go
```

Go 同包文件没有 import 层面的循环问题；该依赖图只用于表达修改原因和阅读路径。

---

## 验证计划

### 基线验证

实施前先运行：

- [ ] `go test ./...`

如果失败原因是依赖下载或 Go proxy 超时，应记录为环境阻塞。不要把这种失败解读为重构风险，也不要在未恢复依赖缓存时声称测试通过。

当前已观察到的阻塞形态：

- `github.com/spf13/cobra`
- `modernc.org/sqlite`
- `github.com/charmbracelet/*`
- `golang.org/x/image/*`

这些依赖从 `proxy.golang.org` 下载超时时，会导致多包 setup failed。

### 每步验证

每个移动步骤后执行：

- [ ] `gofmt -w internal/pipeline`
- [ ] 能跑时执行 `go test ./internal/pipeline ./tests/internal/pipeline`

如果 module cache 尚不可用，至少执行：

- [ ] `go test` 命令并记录失败是否仍然是依赖下载问题
- [ ] 检查新增文件 import 是否被 `gofmt` / `go test` 暴露出语法问题

### 最终验证

重构完成后执行：

- [ ] `gofmt -w internal/pipeline`
- [ ] `go build ./...`
- [ ] `go test ./...`
- [ ] `go vet ./...`
- [ ] `internal/pipeline/pipeline.go` 只保留 Runner 调度核心
- [ ] 新增文件均为 `package pipeline`
- [ ] 未新增公开接口
- [ ] `tests/internal/pipeline/pipeline_test.go` 中 `go:linkname` 指向的函数名和签名未改变
- [ ] `internal/pipeline/model/` 未被修改

---

## 不改的内容

- 不修改任何函数内部逻辑，包括已知 bug。
- 不引入新依赖。
- 不新增或修改测试。
- 不修改 `internal/pipeline/model/` 下的类型定义。
- 不移动到新子包。
- 不导出新 API。
- 不与 `docs/MVP_FIX_PLAN.md` 中 US-001~US-010 的修复交叉。

---

## 完成标准

- `pipeline.go` 从 2235 行收敛为只表达 Runner 调度和状态机的文件。
- Docker Compose、runtime evidence、Codex 安全边界、findings、artifact IO 各自有清晰文件归属。
- 代码行为保持不变。
- 测试如果无法全量通过，唯一可接受的原因是实施前已记录的依赖下载/module cache 环境阻塞。
- 后续读者能根据文件名判断“为什么要改这个文件”。
