# p2r Stage G 与 p2ro 迭代开发实现计划

## 目标

把 `docs/2026-06-03/qa_operator_iteration_design.md` 修正后的设计落到可实现切片。

第一轮实现重点是 p2r `Stage G`：

```text
A -> D -> E -> F -> B -> G -> C
```

G 第一版直接支持 Codex 自主浏览器测试，但 Codex 不直接执行 shell、不直接调用 Playwright CLI、不访问任意 URL。实现形态是：

```text
Codex action JSON -> p2r validator -> Playwright wrapper -> observation -> Codex next action
```

## 现有代码事实

- 阶段定义由 `internal/pipeline/model/stages.go` 的 `stageSpecs` 驱动。
- 执行入口由 `internal/pipeline/stage.go` 的 `stageRegistry()` 和 `executeStage()` 驱动。
- B 的 runtime 会通过 `StageOutcome.Runtime` 写回 `run_lifecycle.go` 的内存态，再供 C 消费。
- 当前 B 失败的 `SkipNextStage` 只跳过紧邻下一个阶段，插入 G 后需要改成显式 runtime dependency。
- D/E/F 的 `CodexReviewStage` 不适合 G。它强制 read-only、no network，且属于静态审查。
- app-server client 当前不支持工具分发通道，不应直接承载浏览器 action 执行。
- `ArtifactWriter` 已有 artifact root 逃逸保护，G 的所有输出必须走同一套写入边界。
- TUI 多数阶段列表来自 `model.AllStages()`，新增 G 后大多会自动展示，但本地化和测试断言需要补。

## 架构决策

### ADR：Stage G 采用 Codex 自主规划 + p2r 受控执行

Decision：

第一版 Stage G 就引入 Codex 自主浏览器探索，不先做纯硬编码 smoke。

Drivers：

- runtime URL、入口页、SPA fallback、登录页和后端健康页误判太复杂。
- p2r 不能靠固定程序穷举真实前端可用性。
- 浏览器动作有权限风险，不能交给 Codex 任意执行。

Alternatives considered：

- 纯硬编码 browser smoke：实现简单，但会漏复杂入口和交互路径。
- Codex 直接运行 Playwright CLI：能力强，但权限边界不可控。
- Codex 规划 + p2r 执行：能力和边界平衡最好。

Consequences：

- 需要新增 action schema、validator、browser wrapper、observation loop。
- 需要把 URL allowlist、artifact path、network route 都做成硬约束。
- 第一版复杂度高于 smoke，但不会推迟最有价值的智能探索能力。

## 实现切片

### 1. Stage plumbing

修改文件：

```text
internal/pipeline/model/stages.go
internal/pipeline/stage.go
internal/config/config.go
assets/config.yaml
cmd/run.go
internal/tui/localize.go
tests/internal/tui/stage_plan_test.go
tests/internal/tui/viewmodel_test.go
tests/internal/config/config_test.go
```

实现内容：

- 新增 `StageG StageID = "G"`。
- `stageSpecs` 顺序改为 `A,D,E,F,B,G,C`。
- G 标记为 runtime stage。
- G log name 为 `G_frontend_e2e.log`。
- 默认 timeout 增加 `G`，建议 600 秒。
- TUI 本地化增加 “浏览器前端 E2E”。
- static-only 提示从 `B/C` 改成 runtime stages 泛化文案。
- 所有写死 6 个 stage 的测试改为跟随 `model.AllStages()`。

验收：

- `p2r run --stage G` 是合法参数。
- TUI 阶段列表显示 G。
- static-only 模式跳过 G。
- 从 B 起始运行时显示 `B, G, C`。

### 2. Runtime dependency graph

修改文件：

```text
internal/pipeline/run_lifecycle.go
internal/pipeline/stage.go
internal/pipeline/testhooks.go
tests/internal/pipeline/lifecycle_persistence_test.go
tests/internal/pipeline/runtime_evidence_test.go
```

实现内容：

- 新增 runtime dependency helper：

```text
blockedDependents("B") = ["G", "C"]
```

- B 失败或 `StageOutcome.Runtime` 不可用时，materialize G/C 为 blocked。
- 保留 C 缺失 runtime 的防御性失败，但正常链路不应靠 C 自己兜底。
- `materializeSkippedStage` 增加 G 的 blocked artifact。

验收：

- B 无 runtime 时 G/C 都 skipped 或 blocked，不执行 C。
- `stage_status.json` 中 G/C 状态一致。
- blocked G 也有 summary/report，artifact 合同完整。

### 3. URL candidates 和 allowlist

新增文件：

```text
internal/pipeline/browser_url.go
```

实现内容：

- 从 `RuntimeState.Probes` 和 `RuntimeState.Mappings` 生成 `BrowserURLCandidate`。
- URL host 固定为 `127.0.0.1`。
- 保留 service、source、probe status、container port、host port。
- 对所有 candidates 生成 allowlist origin。
- 不再只依赖 `firstFrontendURL(runtime)`。

验收：

- 多 service 时 summary 记录所有 candidates。
- `0.0.0.0`、`localhost`、`[::]` 都归一到 `127.0.0.1`。
- 非 Docker published port 不进入 allowlist。
- pure_backend 且无前端 expectation 时 G 可 `not_applicable`。

### 4. Browser action schema 和 validator

新增文件：

```text
internal/pipeline/browser_action.go
internal/pipeline/browser_policy.go
```

实现内容：

- 定义 action schema：

```text
open_candidate
wait
snapshot
collect_console
collect_network
click_navigation
click_button
fill_input
submit_local_form
go_back
finish
```

- 定义 risk：

```text
read_only
navigation
local_stateful
destructive
```

- 第一版允许 `read_only/navigation/local_stateful`。
- 第一版禁止 `destructive`。
- validator 校验 action 名称、target 类型、selector/text 长度、url_id、输出路径和 reason。

验收：

- 任意 shell action 被拒绝。
- 任意 URL 被拒绝。
- 任意绝对输出路径被拒绝。
- destructive action 被记录到 `blocked_actions`。

### 5. Playwright wrapper

新增目录：

```text
internal/browser/
```

建议文件：

```text
internal/browser/runner.go
internal/browser/playwright_wrapper.go
internal/browser/observation.go
internal/browser/network_policy.go
```

实现内容：

- 封装 Playwright CLI 或 Node wrapper。
- wrapper 只接收 validated action。
- Playwright route 拦截所有非 allowlist origin。
- 收集 DOM 摘要、可见文本摘要、控件摘要、console error、pageerror、network 4xx/5xx、screenshot、current URL。
- 所有输出路径由 p2r 传入，不接受 Codex 指定路径。

验收：

- 打开 allowlist URL 成功截图。
- 外网 request 被阻断并写入 observation。
- console error 被采集。
- pageerror 被采集。
- wrapper 不读 workspace 外路径。

### 6. Codex browser session

新增文件：

```text
internal/pipeline/stage_g.go
internal/pipeline/browser_codex_session.go
assets/prompt_profiles/frontend_e2e_browser.md
```

实现内容：

- G stage 构建 Codex prompt：metadata、docs、README、url candidates、action schema、禁止动作、summary schema。
- Codex 每轮输出一个 action JSON。
- p2r 校验 action。
- wrapper 执行 action。
- observation 反馈给 Codex。
- 达到 stop 条件后 Codex 输出 final summary。

停止条件：

- Codex 输出 `finish`。
- 达到最大动作数，建议 30。
- 达到 stage timeout。
- 连续 invalid actions 超过阈值，建议 3。
- 浏览器 wrapper 失败。

验收：

- Codex 能自主选择 URL candidate。
- Codex invalid action 不会被执行。
- 达到最大动作数时生成 partial summary。
- summary schema invalid 时 G failed infra finding。

### 7. Summary schema 和 finding 映射

新增文件：

```text
internal/pipeline/frontend_e2e_schema.go
```

实现内容：

- 定义 `frontend_e2e_summary.json` Go struct。
- 校验 `schema_version == p2r.frontend_e2e.v1`。
- 校验 severity 枚举。
- summary finding 映射为 `model.Finding`。
- screenshot 字段映射到 `SourcePath` 或 Evidence 文本。

验收：

- 空白页 summary 产生 High finding。
- network 5xx summary 产生 High finding。
- console runtime error summary 产生 Medium/High finding。
- invalid schema 产生 infra High finding。
- findings 插入 DB。

### 8. Artifact 和源码不变校验

新增文件：

```text
internal/pipeline/repo_snapshot.go
```

实现内容：

- Stage G 前对 `repo/` 做 hash snapshot。
- Stage G 后重新 hash。
- 忽略测试产物和可允许 runtime cache。
- 如果源文件变化，G failed + infra finding。

验收：

- wrapper 写 artifact 不触发源码变更。
- 修改 `repo/src/*` 能被检测。
- artifact root 内写入不被误判。

### 9. Preflight

修改文件：

```text
internal/preflight/preflight.go
tests/internal/preflight/preflight_test.go
```

实现内容：

- 增加 browser runtime check。
- Playwright 不可用时 G blocked。
- 不影响 D/E/F 静态 Codex preflight。
- Node 缺失时，如果 wrapper 自带运行时则 degraded，否则 G blocked。

验收：

- Playwright 缺失只影响 G。
- Codex 静态能力缺失只影响 D/E/F。
- Docker 缺失阻断 B/G/C。

### 10. Submit 和 Stage F 策略

修改文件：

```text
internal/pipeline/submit.go
internal/pipeline/stage_f.go
```

第一轮策略：

- G artifacts 先保留在本地 run artifact 和 DB findings。
- 是否复制进最终 submit 目录单独开关，默认不上传。
- Stage F 是否消费 G findings 默认关闭，避免修复报告语义扩大。

后续策略：

- p2ro repair loop 需要消费 G findings 时，再把 G 加入 repair brief。
- 作业员自测 API 上传时，再明确 `frontend_e2e_report.md` 的 artifact type。

验收：

- 现有 submit 行为不回归。
- G failed 不破坏 A-F/B/C artifact 聚合。

## 测试矩阵

### 单元测试

```text
StageG 加入 AllStages 顺序
stage timeout key 接受 G
static-only 跳过 G
B failed blocks G/C
URL candidates 生成
Action validator 拒绝非法动作
Summary schema 校验
Summary findings 映射
Repo snapshot 检测源码变更
```

### 集成测试

```text
fake Playwright wrapper 返回 blank_page
fake Playwright wrapper 返回 console error
fake Playwright wrapper 返回 network 500
fake Codex 返回 invalid action
fake Codex 返回 valid finish summary
B failed 后 G/C blocked
```

### TUI 测试

```text
阶段数从 6 变为 7
G 本地化显示
重跑配置可选择 G
static-only runtime 提示泛化
从 B 起始运行展示 B,G,C
```

### 手工验证

```text
p2r run TASK --stage G
p2r run TASK --from B
p2r run TASK --static-only
TUI 中重跑 G
查看 frontend_e2e_summary.json
查看 frontend_e2e_screenshot.png
查看 DB findings
```

## 并行开发分工

Lane A：stage plumbing 和 dependency graph

```text
internal/pipeline/model/
internal/pipeline/stage.go
internal/pipeline/run_lifecycle.go
internal/tui/
```

Lane B：browser action/schema/url candidates

```text
internal/pipeline/browser_*.go
internal/pipeline/frontend_e2e_schema.go
```

Lane C：Playwright wrapper

```text
internal/browser/
```

Lane D：Codex browser session 和 prompt profile

```text
internal/pipeline/stage_g.go
assets/prompt_profiles/frontend_e2e_browser.md
```

Lane E：tests and preflight

```text
tests/internal/pipeline/
tests/internal/tui/
tests/internal/preflight/
internal/preflight/
```

合并顺序：

```text
Lane A -> Lane B + C -> Lane D -> Lane E
```

Lane A 必须先合，因为后续都依赖 `StageG` 和 dependency graph。

## 不在第一轮范围

- p2ro operator 命令入口。
- repair loop。
- 自动平台提交。
- 质检员网页最终通过/打回。
- 上传真实 AI 报告或反馈视频。
- 多模型调度。
- Playwright trace/video 强制产出。

## 完成标准

- `Stage G` 已进入默认 runtime pipeline。
- B 成功后 G 在 C 前运行。
- B 失败时 G/C 都不会误跑。
- Codex 能通过 action JSON 自主探索浏览器。
- p2r 能拒绝非法 action。
- Playwright wrapper 只能访问 allowlist URL。
- G 生成 summary/report/screenshot/log。
- G findings 能进入 StageRecord 和 DB。
- G 不修改交付包源码。
- 现有 A-F/B/C 流程不回归。
