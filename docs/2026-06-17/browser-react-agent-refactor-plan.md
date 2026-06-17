# Browser ReAct Agent Refactor Plan

## 1. 背景

Stage G 已经具备浏览器 ReAct 雏形：Codex planner 产出 JSON action，Go 层校验 action，Playwright 执行动作并返回 observation，Explorer 再把 observation 回灌给 planner。

原始问题不是单纯文件过大，而是边界混乱：Stage G 同时承担 pipeline stage 适配、planner 调用、浏览器执行、action policy、observation evidence、截图物化、repo snapshot、artifact 写入和 `model.StageRecord` 收敛。继续在 `internal/pipeline` 平铺文件会让 pipeline 包失去可读边界。

本轮重构的第一目标已经明确为：Stage G orchestration、artifact、evidence、record 逻辑收拢到独立目录 `internal/pipeline/stageg/`，`internal/pipeline/stage_g.go` 只保留薄入口。`internal/pipeline/browser_codex_session.go`、`internal/pipeline/browser_url.go`、`internal/pipeline/browser_policy.go` 仍是后续 `browserplanner`、`browserpolicy` 阶段的迁移对象。

## 2. 目标

### 2.1 本轮目标

- 将 Stage G orchestration、artifact、evidence、record 逻辑从 `internal/pipeline` 平铺区移入 `internal/pipeline/stageg/`。
- 生产入口只能通过 `stageg.Request` 调用 Stage G，不允许 `stageg` 反向 import `pipeline`。
- 保持 `frontend_e2e_summary.json`、`frontend_e2e_report.md`、`frontend_e2e_observations.json`、legacy screenshot artifact 的路径和 schema。
- 保持 `internal/pipeline/testhooks.go` 暴露的 `StageG*ForTest` 测试 API，但这些 facade 只服务现有外部测试，不作为长期架构兼容层。
- 不为了旧坏边界保留长期 alias、循环依赖或跨层私有函数访问。

### 2.2 后续目标

后续再把 Stage G 中可复用的浏览器能力抽成通用包：

```text
internal/pipeline/stageg
  └─ Stage G adapter：QA prompt、verdict、artifact、pipeline record

internal/browseragent
  └─ ReAct loop、blocked action、finish orchestration、events

internal/browserplanner
  └─ planner interface、Codex planner、prompt rendering、JSON extraction

internal/browserpolicy
  └─ action validation、risk、origin、dialog、download、upload、artifact/state policy

internal/browserobserve
  └─ observation analysis、state fingerprint、evidence tags、history compaction

internal/browsercore
  └─ browser session、Playwright driver、low-level action execution
```

## 3. 当前落地边界

### 3.1 `internal/pipeline/stage_g.go`

只负责把 `pipeline.StageContext` 和 `Runner` 的依赖拆成 `stageg.Request`：

- run/project/runtime candidates
- artifact writer
- stage timeout
- prompt profile directory
- planner callback
- browser action runner callback
- progress callback

它不再保存 Stage G 证据裁决、artifact 写入、repo snapshot 或 screenshot 逻辑。

### 3.2 `internal/pipeline/stageg`

当前子包职责分布：

- `request.go`：`Request`、`Runner`、planner/action/progress callback 类型。
- `run.go`：Stage G 主流程编排。
- `runtime.go`：planner timeout、runtime cleanup、action screenshot policy。
- `finish.go`：finish policy、最终 record 收敛、repo snapshot finding 合并。
- `evidence.go`：deterministic evidence profile、业务 UI/API 证据、auth/session blocker 证据。
- `observation.go`：observation failure/noise/recovery/state-key 判断。
- `artifacts.go`：summary/report/observations 写入、截图筛选和物化。
- `summary.go`：`FrontendE2ESummary` 构造、model finding 转换、report 文本。
- `log.go`：Stage G log rendering。
- `repo_snapshot.go`：源码变更快照。
- `stage_record.go`：stage record 和 artifact warning 写入辅助。
- `skipped.go`：skipped/blocked Stage G artifact 物化。
- `api.go`：外层 pipeline 和现有外部测试所需 facade。
- `constants.go`、`types.go`：Stage G 常量和兼容类型别名。

`stageg` 可以依赖 `internal/pipeline/model`，因为它仍是 pipeline 内部 adapter；但它不能依赖 `internal/pipeline`。

`type StageContext = Request` 是临时内部 alias。完成 `browseragent/browserplanner` 迁移后必须删除，避免新子包继续继承 pipeline 命名。

## 4. 不变量

本轮重构必须保持：

- Stage G unavailable 时不写 fake screenshot。
- 浏览器 runner error 必须回灌 ReAct loop。
- 弱 passed/failed finish 没有 observation-backed evidence 时仍被拒绝。
- 成功业务证据不能被 planner timeout 丢弃。
- repo snapshot failure 不阻断浏览器探索。
- repo mutation finding 不被 passed outcome 覆盖。
- screenshot artifact 最多 10 张，保留 legacy `frontend_e2e_screenshot.png`。
- `frontend_e2e_summary.json` schema 继续由 `frontende2e.ParseSummary` 校验。

这些属于对外行为契约，不是对旧代码结构的兼容。

## 5. 当前强耦合点

### 5.1 `frontende2e.Explorer` 仍是半抽象 loop

`frontende2e.Explorer` 已抽出 planner/action callback，但仍绑定：

- `model.StageRecord`
- `model.Finding`
- Stage G finishers
- Stage G schema failure finding

后续抽 `browseragent` 时，Result 必须是中性 DTO，不能返回 `model.StageRecord`。

### 5.2 Planner 仍绑定 StageContext 语义

`browser_codex_session.go` 中的 planner 调用仍知道 Stage G log、prompt profile、sandbox 和 progress。后续 `browserplanner.CodexPlanner` 不能接收 `StageContext`，只能接收中性 request。

### 5.3 Policy 分散

当前策略仍分布在：

- `internal/frontende2e/action.go`
- `internal/pipeline/browser_url.go`
- `internal/pipeline/browser_policy.go`
- `internal/browser/playwright_wrapper.go`
- `internal/pipeline/stageg/runtime.go`
- `internal/pipeline/stageg/evidence.go`
- `internal/pipeline/stageg/artifacts.go`

后续 `browserpolicy` 必须成为可执行边界，不能只依赖 prompt 约束 planner。

## 6. 安全策略要求

### 6.1 action risk 必须 deny-by-default

现有 `read_only/navigation/local_stateful/destructive` 不足以表达同源业务风险。后续风险类型至少包括：

```text
read_only
navigation
local_stateful
transaction
download
upload
session_ending
destructive
```

Stage G 默认拒绝 `transaction`、`download`、`upload`、`session_ending`、`destructive`。PPTflow 只能在 mock/sandbox policy 下允许受控 `transaction` 和 `download` 子集。

ActionPolicy 不能只按 action 名称分类，必须联合目标控件、`href`、`form/action/formaction`、URL path/query/fragment、locator 文案、`aria-label`、`title`、网络 method/path 判断风险。

### 6.2 session-ending classifier 必须统一

Go validator 和 Playwright runner 不能维护两套分叉规则。统一 classifier 必须覆盖：

- `logout`、`signout`、`sign-off`
- `退出登录`、`登出`、`注销`、`切换账号`
- route/path/query/fragment、SPA hash route、form action、formaction、href、button text、locator 文案、`aria-label`、`title`
- camelCase、snake_case、kebab-case

验收不能只写 “logout/signout 被拒绝”，必须用共享测试向量覆盖 Go 和 JS 执行层。

### 6.3 artifact path 必须 observation-backed

- planner summary 不能提供任意 screenshot/artifact path。
- summary 中的 screenshot 只能由 Stage G materializer 基于 observation 生成。
- screenshot source 必须来自受控 browser runtime/artifact root，并用 canonical path 二次校验。
- symlink escape 必须拒绝。
- artifact sink 必须提供 root enforcement；不能把 `ArtifactWriter.Path` 的 clean fallback 当安全写入接口。

### 6.4 runtime state 不能进入 artifact root

`storage_state.json`、`session_state.json`、`form_state.json` 都属于敏感 runtime state。后续 StatePolicy 必须要求：

- 默认写入临时非 artifact 目录。
- password、token、cookie、authorization header 不持久化。
- crash residue 有清理策略。
- stateless recovery 只持久化最小 allowlist。

### 6.5 dialog policy 必须进入执行层

alert 可以默认确认；prompt/confirm 必须按 message 和场景分类。包含删除、支付、退款、退出、发布、发送、授权等语义时默认拒绝，不能只因为 planner 提供 value 就接受。

### 6.6 planner raw 和 blocked action 必须统一脱敏

`BlockedBrowserAction.Raw`、planner history、summary、log、warnings 都不能持久化未脱敏 raw JSON。默认策略：

- 不保存 raw planner JSON，除非明确进入 debug artifact。
- 保存前统一清理 `value`、password、token、cookie、authorization、email、OTP。
- blocked action 只保留 action type、risk、reason、selector/path 的脱敏摘要。

### 6.7 scheme、download、popup 默认禁用

Stage G 默认只允许候选 `http/https` origin 和必要的 `about:blank`。默认拒绝：

- top-level `data:`、`blob:`、`file:`、外部 app scheme。
- download。
- upload。
- popup、新窗口、多标签。
- OAuth 或真实第三方支付跳转。

PPTflow 只能在 explicit scenario policy 下启用 download、WebView bridge、mock payment。

### 6.8 state fingerprint 只能保存脱敏结构

`browserobserve` 的 state fingerprint 必须是 path-only URL、normalized text hash、controls hash 和 page kind。不能保存原始 visible text/control text；必须忽略时间戳、随机 ID、nonce、UUID、计数器等噪声。

## 7. 迁移计划

### Phase 0: 基线锁定

目标：确认当前行为安全网。

验收：

- `go test ./tests/internal/pipeline`
- `go test ./tests/internal/pipeline -run "Test(StageBBlocksFrontendAndRuntimeDependents|RunBlocksGAndCWhenStageBPreflightBlocked|RuntimeCleanupPointDoesNotCleanBetweenRuntimeStages)$"`
- `go test ./tests/internal/browser -run TestPlaywrightWrapperReturnsObservationWhenProcessReportsError`
- `go test ./internal/pipeline ./internal/pipeline/stageg`

### Phase 1: Stage G 子包化（已完成）

目标：完成 `internal/pipeline/stageg` adapter 包，清理 pipeline 平铺区的 Stage G orchestration、artifact、evidence、record 逻辑。

工作：

- `internal/pipeline/stage_g.go` 收缩为薄入口。
- Stage G 主流程、finish、evidence、observation、artifact、repo snapshot、log 进入 `internal/pipeline/stageg/`。
- 通过 `stageg.Request` 注入 planner、browser action runner、artifact writer、progress。
- `pipeline/testhooks.go` 保持现有测试 API，内部转发到 `stageg` facade。

验收：

- `internal/pipeline` 不再平铺 Stage G orchestration、artifact、evidence、record 大文件。
- `stageg` 不 import `internal/pipeline`。
- 现有 Stage G 测试全绿。
- `internal/pipeline/browser_codex_session.go`、`internal/pipeline/browser_url.go`、`internal/pipeline/browser_policy.go` 留给后续 planner/policy 迁移，不算 Phase 1 未完成。

### Phase 2: browserpolicy

目标：收口可执行安全策略。

工作：

- 迁移 action schema、validator、risk classification。
- 迁移 URL candidate 和 allowlist resolver。
- 迁移 runtime screenshot capture policy。
- 建立 session-ending、dialog、artifact source/sink、state persistence policy。
- 建立 blocked raw redaction policy。
- 建立 scheme/download/upload/popup policy。

验收：

- transaction/download/upload/session-ending/destructive 默认拒绝。
- 中文 session-ending 测试向量覆盖 Go 和 Playwright runner。
- risky confirm/prompt 被执行层阻断。
- planner-provided screenshot path 被拒绝或忽略。
- blocked raw 不含 password/token/email/OTP/action value。
- 同源 POST/DELETE transaction 默认被 block。
- hash/query logout 和中文 session-ending 默认被 block。
- `data:`、`blob:`、popup、download 默认被 block。
- session-ending 共享向量文件同时驱动 `tests/internal/pipeline` 的 Go validator 测试和 `tests/internal/browser` 的 Playwright runner 测试。
- alert accept；confirm/prompt 删除、支付、退款、退出、发布、授权必须拒绝；只有明确 allow policy 才能接受 planner value。
- runtime state 不在 artifact root。
- crash residue 下次启动前 scrub。
- storage/session/form state 不含 cookie/token/password/email/OTP/raw form value。
- screenshot source canonical path 必须位于受控 runtime root；symlink escape 必须失败。
- `ArtifactWriter.Path` fallback 不能被用于写入或记录成功 artifact。

### Phase 3: browserobserve

目标：把 observation 派生分析从 Stage G 中剥离。

工作：

- 迁移 auth gate/authenticated 判断。
- 迁移 framework noise、auth utility endpoint、network recovery 判断。
- 迁移 repeated state/auth gate stall/session loss 判断。
- 迁移 deterministic positive evidence profile。
- 迁移 key screenshot selection。
- 引入 state fingerprint、page kind、evidence tags、history compaction。
- fingerprint 只保存 hash 和结构化 key，不保存原始可见文本。

验收：

- `TestStageGPositiveEvidenceOutcome*` 全部通过。
- Blazor/Vite framework noise 不算业务证据。
- 登录 500 仍产出 product failure。
- 认证成功后回到 login/logout 不被误判为 passed。

### Phase 4: browserplanner

目标：让 Codex planner 独立于 StageContext。

工作：

- 迁移 prompt rendering、JSON extraction、planner timeout classification。
- `browserplanner.Request` 使用中性字段。
- Stage G adapter 负责读取 profile/template/context 并组装 request。
- 引入 compact history 输入结构。

验收：

- prompt profile asset 渲染测试通过。
- timeout classification 不吞掉已收集 successful evidence。
- Codex env 不继承 secrets/user home。

### Phase 5: browseragent

目标：替换 `frontende2e.Explorer` 为通用 ReAct loop。

工作：

- 迁移 max actions、invalid action count、per-turn timeout。
- 迁移 planner/action/observation loop。
- Result 使用中性 DTO，不返回 `model.StageRecord`。
- Events 只暴露通用 callback。
- Stage G adapter 实现 finishers、verdict、artifact writer。

验收：

- action runner error 仍回灌 planner。
- 弱 finish 仍被 evidence gate 拒绝。
- deterministic evidence verdict 不丢失已有成功证据。

### Phase 6: browsercore

目标：沉淀底层 browser session/driver 接口。

工作：

- 定义 `Browser`、`BrowserSession`、`RunRequest`、`RuntimePolicy`。
- 现有 `PlaywrightWrapper.Run` 可以继续保持每轮 browser process 无状态。
- runtime state 必须落到非 artifact 临时目录，并有清理/脱敏测试。
- persistent session 单独引入，不改变 Stage G 的 conservative policy。

验收：

- wrapper process exit 但 stdout 有合法 observation 时仍返回 observation。
- Stage G 默认仍可使用 stateless recovery，但 runtime state 不进入 artifact root。
- runtime state policy 生效。

### Phase 7: PPTflow adapter

目标：复用 browseragent，不复制 Stage G。

工作：

- 定义微信 H5 scenario policy。
- 定义 payment mock policy。
- 定义 download capture policy。
- 定义 admin safe policy。
- 产出 PPTflow 自己的验证 report。

验收：

- 可模拟 `wx.chooseWXPay` success/cancel/fail。
- 可模拟 `wx.downloadFile` 和 `wx.openDocument`。
- 可验证 WebSocket/SSE 流。
- 可验证订单状态从 paid 到生成完成。

## 8. 测试策略

必须覆盖：

- `TestBrowserURLCandidatesUseLocalhostAllowlist`
- `TestBrowserURLCandidatesDoNotBorrowProbeAcrossPorts`
- `TestBrowserURLCandidatesDeduplicateEquivalentMappings`
- `TestBrowserCodexEnvDoesNotInheritSecretsOrUserHomes`
- `TestBrowserActionPromptTemplateRendersFromPromptProfileAsset`
- `TestBrowserActionValidatorRejectsUnsafeActions`
- `TestExtractJSONObjectAcceptsWrappedPlannerOutput`
- `TestFrontendE2ESummarySchemaValidation`
- `TestFrontendE2EObservationFindingsCanSuppressActionFailureFallbacks`
- `TestRepoSnapshotDetectsSourceChangesAndIgnoresCaches`
- `TestStageGBrowserContextHighlightsReadmeCredentials`
- `TestStageGBrowserContextResolvesReadmeReferencedEnvPassword`
- `TestStageGFinishRequiresMinimumBrowserScreenshots`
- `TestStageGPositiveEvidenceOutcome*`
- `TestStageGRejectsPlannerPassedFinishWithoutDeterministicEvidence`
- `TestStageGRejectsPlannerFailedFinishWithoutObservationBackedEvidence`
- `TestStageGPlannerTimeoutRecognizesGenericTimeoutErrors`
- `TestStageGUsesConfiguredPlannerTurnTimeout`
- `TestStageGActionRunnerErrorFeedsPlannerReactLoop`
- `TestStageGAcceptsModelFinishAfterSuccessfulEvidence`
- `TestStageGRepoSnapshotFailureContinuesBrowserExploration`
- `TestStageGAutoOutcomeKeepsRepoSnapshotMutationFinding`
- `TestStageGMaterializesAtMostTenScreenshotArtifacts`
- `TestStageGUnavailableDoesNotWriteFakeScreenshotArtifact`
- `TestPlaywrightWrapperReturnsObservationWhenProcessReportsError`
- `TestStageBBlocksFrontendAndRuntimeDependents`
- `TestRunBlocksGAndCWhenStageBPreflightBlocked`
- `TestRuntimeCleanupPointDoesNotCleanBetweenRuntimeStages`

新增安全测试必须覆盖：

- `tests/internal/pipeline`: 同源 transaction POST/DELETE block。
- `tests/internal/pipeline`: 中文 session-ending：`退出登录`、`登出`、`注销`、`切换账号`。
- `tests/internal/pipeline`: hash/query/fragment logout block。
- `tests/internal/browser`: confirm delete/pay/refund/logout block。
- `tests/internal/pipeline`: blocked raw redaction。
- `tests/internal/browser`: runtime state 不含 password/token/email/OTP/raw form value。
- `tests/internal/pipeline`: screenshot source path escape 和 symlink escape。
- `tests/internal/browser`: `data:`、`blob:`、popup、download 默认 block。

只跑 `go test ./internal/pipeline` 不够，外部测试集中在 `tests/internal/pipeline`。

## 9. 完成标准

- Stage G 代码集中在 `internal/pipeline/stageg/`。
- `internal/pipeline/stage_g.go` 是薄入口。
- `stageg` 无反向依赖 `pipeline`。
- artifact schema/path 不变。
- Stage G 相关测试全绿。
- 文档中的后续拆包顺序和当前代码状态一致。
- 安全策略不再把旧风险描述为“行为不变”，而是作为后续硬验收。
