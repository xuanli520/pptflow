# CodeEdge 一阶段生产执行白名单与不可变运行说明书设计

状态：核心安全模型、流程策略和三 bundle 的源码契约已确认。每次本地生产
打包前，受控 lock 生成器从干净提交和本机实际 Harbor/Codex/Docker 安装探测
可执行文件路径、版本和 SHA-256；这些运行时事实只写入同包的 lock，绝不由
worker、CLI/TUI 请求或实现默认值猜测。

约束来源：`【CodeEdge】 一阶段专家培训文档.md`（2026-07-07 v1.1）和
`WORKFLOW_STABILITY_DECISIONS.md`。

## 1. 目标与边界

本设计为 Harbor Flow 的一阶段任务生产流程建立两份不同、但相互绑定的
权威文件：

1. **生产执行白名单（Deployment Operation Catalog）**：发布级、只读、
   版本化的“此部署能执行什么”。
2. **不可变运行说明书（Frozen Run Manifest）**：Run 级、一次性冻结的
   “本次实际要执行什么、用什么版本、产生了什么证据”。

它们共同确保 Harbor Flow 不会从用户输入动态允许任意 shell 命令、Docker
镜像、模型、端点、秘密或 prompt；也不会在 Run 开始后静默换用新的模型、
镜像、可执行文件或验证规则。

本轮仅产生并记录本地 immutable package，不上传、不发布、不复制到外部
目的地。模型凭证仅以受控 secret reference 出现，绝不写入 catalog、Run
manifest、日志、截图或 artifact。

## 2. CodeEdge 一阶段要求到系统不变量的映射

| CodeEdge 要求 | 强制不变量 | 由谁执行 | 必须保留的证据 |
| --- | --- | --- | --- |
| ZIP 可解压且只有一个 Harbor task 根 | package 仅含一个受管 task root；核心路径完整 | `task-layout.preflight` | ZIP digest、根目录清单、校验回执 |
| 必须有 instruction、task.toml、environment、solution、tests、tests 分析 | 缺任一必需内容即不可进入评测；CodeEdge profile 要求 Dockerfile 与 compose 二者择一 | `task-layout.preflight` | canonical task manifest、缺失项诊断 |
| 环境不得预置 tests/solution/reward | Dockerfile/compose、build context、volume 与镜像层均通过隔离检查 | `environment-isolation.preflight` | 静态扫描报告、build-context digest、镜像层检查回执 |
| 非 0-1 任务的公开仓库 URL/commit 与 Docker 一致 | metadata、Docker clone URL 与 checkout/reset 的 commit 精确一致 | `repo-provenance.preflight` | URL、commit、解析后的 Docker 指令、来源摘要 |
| Docker 稳定 build、不能依赖私有凭证/本地绝对路径 | build 只能使用允许的 Docker/compose operation；失败归为 infra | `environment-build` | tool/version、build log digest、image digest 或失败回执 |
| Oracle 可运行，执行后 tests 通过 | `solution/solve.sh` 后的 `tests/test.sh` 必须成功；禁止篡改 tests/reward | `oracle.verify` | 前后快照 digest、命令收据、tests 报告 |
| 初始环境能暴露问题 | 初始 verifier 结果必须符合该 task 的冻结预期 | `initial.verify` | 初始 tests 报告与预期结果 |
| instruction、tests 与 tests 分析一致 | tests 分析必须包含培训文档规定的三段结构，且检查点可从 instruction/environment 推导 | `tests-analysis.validate` 和 durable review | 分析结构报告、review decision、引用的 artifact digest |
| Qwen 与 Opus 各独立运行 4 次 | 两个 evaluator profile 各恰好生成 4 个逻辑 Trial；技术 retry 不增加样本数 | `qwen.pass-at-four`、`opus.pass-at-four` | trial IDs、结果、命令收据、单张完整截图 artifact |
| Qwen pass@4 不超过 1/4、平均轮数不少于 20 | Qwen 汇总器在可信完成结果上校验 `pass_count <= 1`、`trial_count = 4`、平均轮数阈值 | `qwen.pass-at-four` | 结构化聚合报告、截图 digest、原始 Harbor result digest |
| Docker/网络/权限等 infra 失败不得算模型失败 | provider failure classifier 只能产生 `infra_failed`/`in_doubt`，不产生业务 verdict | 所有 provider | 分类依据、stderr 摘要、runtime receipt |
| 不得靠隐藏信息、hack 或篡改 verifier 通过 | evaluator workspace 与 Oracle/test workspace 分离；所有写入范围白名单化 | execution sandbox + `solution.verify` | mount/checkout receipt、变更清单、违规报告 |

## 3. 两层不可变性模型

### 3.1 Deployment Operation Catalog：部署能做什么

受管包路径：

```text
deployments/
  standard-authoring/
    operation-catalog.v1.json
    contract-assets.v1.json
    execution-profile.v1.json
    operation-catalog.lock.json            # 本机生成，随包分发
  codeedge-phase1/
    operation-catalog.v1.json
    execution-profile.v1.json
    preflight-profile.v1.json
    final-compliance-policy.v1.json
    operation-catalog.lock.json            # 本机生成，随包分发
  codeedge-evaluator-child/
    operation-catalog.v1.json
    contract-assets.v1.json
    contracts/harbor-pass-at-four.v0.18.json
    schemas/harbor-run-bundle.v0.18.json
    execution-profile.v1.json
    operation-catalog.lock.json            # 本机生成，随包分发
```

三份 catalog/lock 仅按冻结的 `TemplateReference` 路由，不能按 stage key、
provider、路径或默认配置交叉授权。源码只跟踪可审阅的 catalog/profile/policy/
contract assets；三份 lock 被排除出源码 manifest 以避免自引用，但包构建器会
在链接前同时验证它们与同一提交的 manifest。常规 CLI/TUI 不提供在线编辑
能力。启动时以 canonical JSON 计算 catalog fingerprint；每份
`operation-catalog.lock.json` 绑定：

- catalog 格式与版本；
- canonical catalog SHA-256；
- Harbor Flow build/source revision；
- 每个可执行文件的绝对解析路径、版本与内容/发布 fingerprint；
- 每个容器镜像的完整 `image@sha256:...`；
- 每个 Agent/model/prompt schema 的固定 ID、版本和 fingerprint；
- 本地 Harbor CLI 的版本与受支持的结果格式版本。

任何 lock、catalog、二进制、镜像、prompt 或 schema 不匹配，worker 必须在
**开始外部副作用前**拒绝执行。它不能降级到 PATH 搜索、最新 tag、默认模型、
环境变量默认值、stage-name switch 或旧 Runner。

### 3.2 Frozen Run Manifest：本次 Run 实际做什么

每次 `StartRun` 在调度 durable job 前写入受管 Run 目录：

```text
runs/<run-id>/
  profile.json
  execution-spec.json
  run-manifest.json
  deployment-catalog.receipt.json
```

`run-manifest.json` 除现有 task/revision、workflow、profile、quota、计划、
execution spec digest 外，必须冻结：

- `deployment_catalog_id`、版本、fingerprint 与 lock fingerprint；
- 每个 stage 的 provider、operation、payload、runtime、checkout 与 secret
  reference **身份**；
- 已解析的 executable/image/model/prompt/schema fingerprints；
- CodeEdge evaluator profile、`trial_count=4`、聚合规则、截图规则、平均轮数
  阈值及 Qwen pass@4 阈值；
- task snapshot digest、package/source digest、repo URL/commit（适用时）；
- 环境 build receipt、Oracle/tests/evaluator/review artifact 引用；
- 冻结的失败分类规则版本。

运行中只允许追加 StageAttempt、Trial、checkpoint、artifact、receipt 和 audit
事实；不得覆盖上述字段。任何继续处理均使用已冻结 manifest；若需要新
catalog/profile/model/prompt/image，必须创建新的 child Run。

## 4. Catalog 的严格数据模型

catalog 是强类型文档，不允许 `map[string]any`、未知字段、重复 JSON key、
未排序的可比集合或不固定版本的引用。每个 operation 至少包含：

```text
catalog_id / catalog_version / format
stage_key / stage_group / stage plugin id+version
provider id+kind+version
operation id+version
typed payload (local.command | container.command | agent.turn | durable.review)
runtime contract (runtime id+kind+version, checkout purpose, allowed secret references)
input / output artifact schemas and required evidence
effect, quota claims, budget contract, retry/failure classifier version
CodeEdge policy contract (如适用：trial 规则、截图、repo provenance、隔离规则)
```

其中 `RunExecutionSpec` 只能选择 catalog 中一条逐字段相同的 registration：

```text
provider ID/kind/version
stage key + plugin ID/version
operation ID/version
canonical typed payload
runtime ID/kind/version
checkout ID/purpose
secret reference ID/provider/version（只比身份，不读取值）
```

注册表不得根据调用方 spec 临时生成条目；测试 fixture 可以动态构造，但生产
代码绝不可这样做。

## 5. CodeEdge 一阶段受控 operation 族

以下是目录必须覆盖的语义 operation，而非尚未确认的真实 executable、image
或 model 值。实际 ID/版本/路径/digest 将在部署值确认后填入。

| Operation family | 建议 typed payload | 作用与不可绕过条件 |
| --- | --- | --- |
| `codeedge.task-layout.preflight` | `local.command` | 解包/检查唯一 task 根、必需文件、严格文件策略和 ZIP/package digest；不得修复或补造文件。 |
| `codeedge.environment-isolation.preflight` | `local.command` | 扫描 Dockerfile/compose/build context/volumes；拒绝 tests、solution、reward 泄露以及不安全 wildcard/root context。 |
| `codeedge.repo-provenance.preflight` | `local.command` | 比对 GitHub URL、commit、Docker clone 与 checkout/reset；拒绝 branch/tag/default-head 代替固定 commit。 |
| `codeedge.environment-build` | `container.command` | 使用固定 Docker/compose runner 建立 task environment；记录 build log 与解析后 image digest；禁止把测试或 Oracle 挂给模型环境。 |
| `codeedge.initial-verify` | `local.command` 或 `container.command` | 在初始受控 checkout 执行 verifier，确认题目能暴露预期问题；失败分类必须可解释。 |
| `codeedge.oracle-verify` | `local.command` 或 `container.command` | 仅在隔离 Oracle checkout 执行 `solution/solve.sh` 后再运行 tests；禁止修改 tests、verifier、reward 或无关目录。 |
| `codeedge.tests-analysis.validate` | `local.command` + durable review | 校验三段 tests 分析结构、引用关系与“无隐藏要求”声明；不能把测试内部答案泄给模型。 |
| `codeedge.task-authoring` / `codeedge.repair` | `agent.turn` | 仅允许经固定 prompt/schema 的 authoring 或 repair；输入输出、turn checkpoint、模型、最大轮数均需冻结。 |
| `codeedge.qwen.pass-at-four` | `local.command` | 固定 Harbor CLI、Qwen profile、4 个独立 Trial、结果解析和单张完整截图 capture；密钥仅通过 reference 注入。 |
| `codeedge.opus.pass-at-four` | `local.command` | 固定 Harbor CLI、Opus profile、4 个独立 Trial、结果解析和单张完整截图 capture。 |
| `codeedge.package` | `local.command` | 生成唯一可解压的本地 package，记录 package/source/evidence digest；不上传。 |
| `codeedge.*.review` | `durable.review` | 仅通过可审计 ReviewRequest/Decision 处理质量、归因或发布门；不得伪造 pass。 |

`harbor_run_qwen` 与 `harbor_run_opus` 不能退化为“任意容器命令”或“任意模型
调用”。它们是目录中独立、静态的 operation，固定经确认的 Harbor CLI
invocation policy（它必须归一化为四个逻辑 Trial）、结果格式、截图捕获范围及
结果解析器。技术 retry 仅是同一 Trial 的内部 reconcile，不能把样本数从 4
扩大到 5。

本机已核验的 Harbor `0.18.0` 中，`-k/--n-attempts=4` 是每个 task/agent
组合生成四个 attempt/trial，`-n/--n-concurrent=1` 是当前生产 profile 的串行
并发度；培训材料把这两个 flag 的说明写反了。生产 operation 冻结用户确认的
`--n-attempts 4 --n-concurrent 1 --max-retries 3`，并且 Qwen 完成后才允许
Opus 开始。receipt 必须记录真实 CLI 语义、四个产生的 logical Trial ID 和
结果，而不能把并发度误当作样本数。

## 6. 已确认的执行顺序与隔离边界

每一步只有在所有前置证据有效时才可推进。最终 local package 必须位于评测和
最终合规之后：

```text
task layout / repo provenance / environment isolation
  → controlled environment build
  → initial verification
  → Oracle + tests verification
  → tests-analysis validation + review
  → Qwen independent Trial 1..4 → Qwen aggregation + screenshot
  → Opus independent Trial 1..4 → Opus aggregation + screenshot
  → submission checks → final compliance review
  → immutable local package
```

该顺序由独立、闭合且版本化的 `CodeEdge Phase-1` descriptor 执行。它绝不
默默套用或修改 `StandardWorkflowTemplate`；`pkg/workflowkit` 继续保持领域无关。

需要满足：

- **作者/Oracle/验证/评测 checkout 分离**。评测模型的 workspace 不得预置
  `tests/`、`solution/`、reward 或 verifier 内部断言。
- Docker build context 由 catalog 指定的安全路径构建，不能默认使用 task 根。
- 公开仓库 clone 可在受控 build 阶段使用，但必须以冻结 URL 与 commit 执行，
  并将解析结果写入 receipt。
- 评测凭证只能按 catalog 允许的 secret reference 注入子进程，日志/manifest
  仅记录 reference ID 与版本；环境变量值必须过滤。
- Docker、网络、下载、权限、PATH、CLI 协议错误被归为 `infra_failed` 或
  `in_doubt`，绝不计入 Qwen/Opus 的任务失败或 content verdict。

## 7. CodeEdge 评测说明书

每个 evaluator operation 需要生成结构化、可复核的 `EvaluationReceipt`：

```text
evaluator_profile_id/version
agent + model identity
harbor_cli identity/version
task package digest + task snapshot digest
trial logical IDs (exactly four)
per-trial terminal state, reward/pass, turn count, elapsed time, result digest
aggregate pass_count, pass_at_four, average_turns
screenshot artifact ID/digest (exactly one canonical screenshot per model)
failure classification and diagnostic artifact IDs
catalog/manifest fingerprint
```

Qwen receipt 的成功条件至少是：可信的四个 Trial、完整单张截图、
`pass_count <= 1`、平均轮数满足冻结阈值、没有被分类为 infra 的 Trial 被作为
模型失败计数。Opus 使用相同的证据完整性规则，但其 pass@4 阈值仅按批准的
定级政策解释，不可擅自套用 Qwen 阈值。

任何 task package、instruction、tests、Oracle、Docker 环境、公开仓库版本或
catalog/profile 改动，都会令旧 evaluation receipt 与截图失效，必须创建新
Run 并重新完成 4 次独立运行。

## 8. 审计、恢复与失败处理

每次 operation 的 durable receipt 至少记录：冻结输入 fingerprint、实际
operation key、worker lease/fencing token、开始/结束时间、工具版本、输出
artifact digest、失败分类和控制/终止回执。失败产物必须进入 lineage，供
TUI、review 和自动 repair 使用；它们不得被下游当作成功输入复用。

外部副作用（Docker build、Harbor CLI 评测、package 创建）在 worker 丢失或
结果未知时进入 `in_doubt`，先通过 receipt/result/package digest reconcile，
再决定 completed、failed_recoverable 或 needs_human。Harbor evaluator 的
reconcile 只读取受管本地 job 目录中的 `result.json`、Trial 结果、`config.json`
和 `lock.json`；它不上传、下载或查询远端结果。禁止盲目重跑而重复计算 Trial 或
制造第二个本地 package。

## 9. 已确认策略与本机探测

已确认：独立、串行的 CodeEdge evaluator child descriptor；冻结
`harbor run --n-attempts 4 --n-concurrent 1 --max-retries 3`；评测受管 task
snapshot；Qwen/Opus 后、最终合规 review 后才创建 package；每次外部副作用前
重验 catalog/lock/attestation；显式 `MetadataFieldMapping`；可信 `result.json`
加每模型一张截图；Opus 仅作参考；本地不实施三小时外部频控。

源码已冻结 Harbor `0.18.0` 结果 ABI、Qwen/Opus agent/model 身份、
`--n-attempts 4 --n-concurrent 1 --max-retries 3`、secret reference 名称、
Standard 的 Codex `gpt-5.5` 契约、父流程 metadata 映射、最终合规策略和所有
prompt/schema fingerprint 输入。lock 生成器在本机严格探测 Git、Codex、Harbor
launcher、Python source tree 和 Docker；只从允许的环境变量名称读取 endpoint
与 secret，并仅保存 endpoint fingerprint 和 secret reference。

任何缺失、版本漂移、路径含 symlink、endpoint 与 catalog fingerprint 不符、
未提交源码、已有 lock 或不完整资产都会明确 fail-closed。系统不会使用测试
占位值、PATH 中任意同名工具、最新容器 tag、默认模型或隐式网络访问。

## 10. 实施与验收清单

实施后必须至少覆盖：

1. catalog/lock 的重复 key、未知字段、digest、版本、binary/image/model drift
   均在 StartRun 前拒绝；
2. CLI、TUI、foreground worker、detached worker 使用同一 catalog fingerprint；
3. Run manifest 中的 catalog/spec/profile bytes 与对应 digest 精确一致；
4. 每项环境隔离规则都有允许与拒绝测试，包括 Dockerfile、compose、build
   context、volume、reward、tests、solution 和固定 commit；
5. Oracle 成功、tests 成功、初始验证和失败分类均有集成测试；
6. Qwen/Opus 各只产生四个逻辑 Trial，截图恰好一张，lost worker/reconcile
   不新增 Trial；
7. infra failure 不计入 pass/fail 聚合，且在 TUI 显示可诊断证据；
8. 修改 task/package/catalog/profile/model/image/prompt 后，旧 screenshot 与
   evaluation evidence 均不允许复用；
9. `pkg/workflowkit` 不出现 CodeEdge、Qwen、Opus、Harbor CLI 或 Docker 的
   领域分支；这些只位于 Harbor deployment adapter；
10. 删除旧 `stageexecutor`/Runner/Scheduler 生产路径后，完整 workflow 仍经
    public `workflowkit.Engine` 和受控 catalog 运行。
