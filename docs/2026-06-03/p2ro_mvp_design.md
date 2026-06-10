# p2ro MVP 设计方案

## 设计结论

p2ro MVP 定位为独立 fork 仓库中的作业端 TUI，不再作为 p2r 质检端的一个 mode 实现。p2r 继续聚焦质检、运行时证据和审查能力；p2ro 聚焦 prompt-to-repo 生产、测试冻结、开发、自检、打包和待质检交付。

两仓不能长期靠整仓 fork diff 维护。MVP 可以先 fork 起步，但必须从第一版开始把共享边界写清楚：运行模型、artifact/schema、prompt2repo 包校验、Docker runtime、Stage B/G/C、浏览器 E2E、finding 映射、执行器和基础配置属于共享内核；p2r 的质检裁决 UI、p2ro 的生产状态机和作业端 TUI 属于各自产品层。

## 依据

- `docs/2026-06-03/qa_operator_iteration_design.md` 已定义 p2ro 是 producer 流水线，流程为 `prompt -> repo -> self-check -> repair -> recheck -> submit for QA`。
- `docs/2026-06-03/qa_operator_iteration_implementation_plan.md` 已给出 Stage G、B/G/C runtime 复用和 p2ro 本地生产 MVP 范围。
- `H:\project\mindflow\prompt2repo\项目流程提示词` 给出实际作业流程：初始化、测试冻结、文档驱动开发、前端联调、静态审查、循环修复、修复确认。
- 当前源码已有 p2r 的可复用基础：`internal/pipeline/model`、`internal/pipeline`、`internal/browser`、`internal/docker`、`internal/codex`、`internal/scheduler`、`internal/tui`、`internal/projectlayout`、`assets/scripts`。
- 当前源码已落地 Stage G：`internal/pipeline/model/stages.go` 定义 `A,D,E,F,B,G,C`，`internal/pipeline/stage_g.go` 和 `internal/browser/` 已提供 Codex-guided browser E2E、URL allowlist、Playwright wrapper、summary/finding 映射和源码不变校验。

## Claude Code runtime 可行性调研

调研日期：2026-06-09。

本机环境：

```text
claude --version = 2.1.143 (Claude Code)
global npm package = @anthropic-ai/claude-code 2.1.143
latest npm @anthropic-ai/claude-code = 2.1.169
latest npm @anthropic-ai/claude-agent-sdk = 0.3.169
latest PyPI claude-agent-sdk = 0.2.94
```

结论：当前不能把 p2ro MVP 建在“Claude app-server 等价于 Codex app-server”这个假设上。本机 `claude --help` 没有 `app-server` 子命令；`claude app-server --help` 不暴露 Codex 风格 `--listen stdio://`；官方 Claude Code 文档的可编程入口是 Agent SDK 和 `claude -p` 的 headless/stream-json 模式。Claude Remote Control 是面向 claude.ai 网页/移动端控制本机会话的交互功能，且本机验证需要 claude.ai 登录，不适合作为 p2ro TUI 的本地编排 API。

现有 p2r 的 `internal/codex/appserver` 使用的是 Codex 专用协议：

```text
codex app-server -c approval_policy="never" -c sandbox_mode="read-only" --listen stdio://
initialize -> thread/start -> turn/start -> turn/steer
```

这个协议不能直接迁移到 Claude。p2ro 必须抽象 `AgentRuntimeAdapter`，把 Codex app-server、Claude Agent SDK、Claude CLI stream-json 分成不同实现。

### 可行性矩阵

| Runtime | 可行性 | MVP 定位 | 关键判断 |
| --- | --- | --- | --- |
| Claude Agent SDK | 高 | 首选 spike 对象 | 官方定位为编程式 agent 接口，支持 TypeScript/Python，适合长期抽象成 runtime adapter。 |
| Claude CLI `-p --output-format stream-json` | 中高 | SDK 不稳定时的 fallback | 本机已暴露 `--print`、`--input-format stream-json`、`--output-format stream-json`、`--allowedTools`、`--disallowedTools`、`--tools`、`--permission-mode`、`--session-id`、`--continue`、`--resume`。适合进程级编排，但事件语义要适配。 |
| Claude Remote Control | 低 | 不作为 MVP 依赖 | 官方说明是远程控制交互式会话，本机验证要求 claude.ai 登录；不是可嵌入 TUI 的本地 JSON-RPC app-server。 |
| `claude mcp serve` | 低 | 不作为 producer runtime | 它启动 Claude Code MCP server，适合把 Claude Code 暴露给 MCP 客户端，不等价于让 p2ro 控制 Claude 写码会话。 |
| Codex app-server | 已验证 | 保留为 p2r 参考实现或可选 adapter | p2r 当前 D/E/F/G 已依赖它，但它是 Codex 专用，且现有配置强制 read-only，不适合直接做 p2ro 写码生产。 |

### MVP runtime 设计约束

- p2ro 不直接依赖 `claude app-server`。
- MVP 前必须先完成 Claude runtime spike，验证 SDK 或 CLI 能启动会话、流式输出、写入 workspace、限制工具、处理权限、取消进程、落盘日志、恢复或关联 session。
- `AgentRuntimeAdapter` 只暴露统一事件，不泄漏 SDK/CLI/Codex 的内部协议。
- 权限策略由 p2ro policy engine 决策，Claude/Codex 只作为执行后端。
- 如果 Claude SDK spike 未通过，MVP 降级为 Claude CLI stream-json adapter；如果 CLI 也不能稳定处理权限和会话，MVP 只能把 Codex producer adapter 作为临时后端，Claude 支持延后。

调研依据：

- Anthropic Claude Code CLI reference：`https://docs.anthropic.com/en/docs/claude-code/cli-usage`
- Anthropic Claude Code Agent SDK：`https://docs.anthropic.com/en/docs/claude-code/sdk`
- Anthropic Claude Code Remote Control：`https://docs.anthropic.com/en/docs/claude-code/remote-control`
- 本机 `claude --help`、`claude mcp serve --help`、`claude remote-control --help`
- 本机 `C:\nvm4w\nodejs\node_modules\@anthropic-ai\claude-code\package.json`

## 产品目标

p2ro MVP 要让作业员在 TUI 中完成一个本地 prompt2repo 包的生产闭环：

```text
业务 prompt -> 初始化骨架 -> 冻结测试 -> 实现代码 -> 自检 -> 打包 -> 待质检
```

作业员主要看到：

- 当前作业和阶段状态
- 流式 agent 输出
- 结构化权限请求
- 当前 diff 和关键文件变更
- 测试、自检、打包结果
- Stage B/G/C 或后续质检复用结果
- 最终可提交的 prompt2repo 包路径

作业员不进入自由聊天壳，不反复回答无上下文 yes/no。所有人工输入都进入结构化 pipeline input，并记录到 run manifest。

## 非目标

MVP 不做以下内容：

- 自动平台最终提交
- 自动质检通过或打回
- 多模型调度
- 多人协作
- 质检员题目管理系统网页自动化
- 完整修复闭环的多轮自动 recheck

MVP 可以预留 P6/P7/P8，但默认只实现 P0/P1/P2/P3/P5/P9/P10。

作业员自测附件 API 已完成协议 spike，包含 exists/list/download_url 和 `batch-presigned-url -> PUT presigned URL -> batch-confirm` 写入链路。MVP 将其作为 P10 核心能力接入，并由 p2ro 原生 adapter 接管 quality-runner 当前承担的自测附件上传职责。P10 必须和 P9 package 解耦：P9 只生成本地 artifact、manifest 和待质检包；P10 消费 manifest 上传平台自测附件。认证、上传开关和失败处理不能混入 P9 package。

## 双仓维护策略

### 仓库关系

```text
p2r_tui        质检端主仓，上游共享内核来源
p2ro_tui       作业端 fork 仓，独立产品入口和 TUI
p2r_core       中期抽出的共享 Go module，MVP 后进入稳定化
```

MVP 第一阶段采用 fork 加上游同步：

- p2ro 从 p2r 当前仓库 fork。
- p2ro 保留 `upstream` 指向 p2r。
- p2ro 新增生产逻辑放在新目录，尽量不改 p2r 共享文件。
- 每周或每个 milestone 从 p2r cherry-pick 共享内核修复。
- 一旦共享文件被两仓频繁修改，立即抽成 `p2r_core` module。

中期目标是把共享内核从 `internal/` 中抽出。Go 的 `internal` 不能被独立仓直接 import，长期双仓维护不能依赖复制 `internal/pipeline`。

### 共享内核

应保持同源或抽入 `p2r_core`：

- `Run / Stage / Finding` 基础模型
- artifact writer 和 artifact path 防逃逸
- prompt2repo package layout 校验
- `metadata.json`、`docs/`、`repo/`、`original_sessions/` 结构检查
- Stage A 的通用质量脚本
- Docker runtime harness
- Stage B runtime evidence
- Stage G browser URL candidates、action validator、Playwright wrapper、summary schema
- Stage C run_tests runtime evidence
- executor 命令执行与流式输出
- preflight 检查
- BrowserAction、FrontendE2E summary、static review schema
- finding severity、ID、映射规则

应留在 p2r 产品层：

- 质检任务三栏状态
- 质检补充文档规则
- D/E/F 质检审查默认阶段顺序
- 质检员裁决、通过、打回、上传链路
- p2r CLI 命令和文案

应留在 p2ro 产品层：

- P0-P9 producer 状态机
- 生产作业 TUI
- prompt intake 和项目类型选择
- workspace 写权限策略
- agent 写代码执行 adapter
- 权限队列
- diff 视图
- 打包和待质检交付流程
- p2ro prompt profiles
- 作业员 self-test attachment adapter

### 同步规则

- 共享内核变更 upstream-first：先在 p2r 或 p2r_core 修改，再同步到 p2ro。
- p2ro 不私改共享核心校验逻辑，避免“作业端能打包、质检端不通过”。
- schema 必须版本化，例如 `p2r.frontend_e2e.v1`、`p2r.static_review.v1`、`p2ro.run.v1`。
- 两仓保留共同 fixtures：有效 prompt2repo 包、缺失 metadata 包、runtime summary、Stage G summary、finding 映射样例。
- CI 必须跑共享契约测试，确认 p2ro 产物能被 p2r scanner 和 Stage A 识别。

## 系统分层

```text
cmd/
  p2ro root, new, run, tui, status

internal/operator/
  p2ro 作业模型、状态机、stage registry、run lifecycle

internal/producer/
  P0/P1/P2/P3/P5/P9/P10 阶段实现

internal/agent/
  producer agent session、权限策略、事件流、diff 捕获

internal/workspace/
  作业 workspace、package skeleton、snapshot、cleanup、manifest

internal/qaadapter/
  复用 p2r 的 A/B/G/C 或共享内核能力

internal/tui/
  作业端 TUI，复用布局思想但重写产品流

assets/prompt_profiles/p2ro/
  intake.md
  scaffold.md
  test_freeze.md
  implement.md
  self_check.md
  package.md
```

MVP 不应把 P0-P9 塞进现有 `internal/pipeline` 的 A-G 阶段体系。A-G 是质检语义，P0-P9 是生产语义；两者通过共享模型和 artifact/schema 对接。

## 作业状态机

MVP 状态：

```text
queued
intaking
scaffolding
test_freezing
implementing
self_checking
packaging
ready_for_qa
manual_required
failed_blocked
cancelled
```

后续状态：

```text
contract_testing
runtime_checking
reviewing
repairing
qa_failed
qa_passed
```

状态转换：

```text
queued -> intaking -> scaffolding -> test_freezing -> implementing -> self_checking -> packaging -> ready_for_qa
```

人工介入：

```text
任何阶段 -> manual_required -> 当前阶段继续或 failed_blocked
```

后续修复闭环：

```text
self_checking -> runtime_checking -> reviewing -> repairing -> self_checking
```

## MVP 阶段契约

### P0 intake

输入：

- 原始业务 prompt
- 项目类型：`fullstack`、`pure_backend`、`pure_frontend`
- 语言和框架偏好，可为空
- 交付边界
- 不可替代需求

输出：

- `p2ro_intake.json`
- `metadata.json` 初稿
- `docs/questions.md`
- 阶段计划和风险标记

要求：

- 原始 prompt 必须完整写入 `metadata.json.prompt`。
- 不确定事项写入 `docs/questions.md`，不能隐式丢弃。
- prompt 不能直接作为 shell 指令执行。

### P1 scaffold

对应 `初始化提示词.txt`。

目标是只搭建仓库框架、主要设计文档和冻结开发文档，不写业务代码。

输出：

```text
docs/design.md
docs/api-spec.md
docs/questions.md
docs/test-plan.md
repo/
original_sessions/
metadata.json
```

要求：

- `repo/` 只允许最小启动骨架和占位结构。
- 不实现业务逻辑。
- 设计文档必须能指导 P2/P3。
- 记录 agent session 到 `original_sessions/`。

### P2 test-freeze

对应 `raplh循环编写测试提示词.txt`。

目标是根据文档和 prompt 写测试，测试必须能运行，不要求通过。

输出：

```text
repo/unit_tests/
repo/API_tests/
repo/run_tests.sh
repo/run_tests.ps1
p2ro_test_freeze_report.md
```

要求：

- 测试覆盖核心行为和失败路径。
- 因业务代码未实现而失败是允许状态。
- 不能为了通过测试而弱化测试。
- 运行测试命令必须稳定返回可解释日志。

### P3 implement

对应 `文档-测试驱动的循环开发流程提示词.txt`。

目标是基于文档和冻结测试完成业务实现。

循环：

```text
分析 -> 编码 -> 运行测试 -> 修复 -> 再测试
```

输出：

- 业务代码
- `p2ro_implementation_report.md`
- `diff_summary.json`
- `logs/P3_implement.log`

要求：

- 不扩大需求范围。
- 不绕过冻结测试。
- 不写 fake logic、硬编码成功响应、静态页面冒充真实功能。
- 所有高风险文件变更写入 diff 摘要。

### P4 frontend-contract

MVP 可选，fullstack 或 pure_frontend 项目优先启用。

对应 `前端开发和前后端联调流程提示词.txt`。

目标是实现企业级前端页面，和后端契约一致，能真实交付。

输出：

- `p2ro_frontend_contract_report.md`
- 前端联调记录
- 关键截图

要求：

- 后端接口和前端调用必须一致。
- 页面不能只是 demo 或静态假实现。
- 登录、退出、错误反馈、空状态、主要业务流要可见。

### P5 self-check

目标是机器化 prompt2repo 交付红线。

复用：

- `assets/scripts/run_validate.py`
- `assets/scripts/run_acceptance.py`
- `assets/scripts/check_fake_impl.py`
- `assets/scripts/check_local_dependency.py`
- `assets/scripts/check_required_artifacts.py`
- `assets/scripts/check_readme_alignment.py`

输出：

- `p2ro_self_check_summary.json`
- `p2ro_self_check_report.md`
- `logs/P5_self_check.log`

要求：

- package root 只能包含允许项。
- README 必须匹配真实启动和测试方式。
- fullstack/backend 默认支持 Docker compose。
- 测试目录和 run_tests 必须存在。
- English prompt 交付内容保持英文。
- 清理依赖缓存、编辑器配置、虚拟环境和数据库文件。

### P9 package

目标是生成待质检交付包。

输出：

- 标准 prompt2repo package root
- `handoff_summary.md`
- `p2ro_package_manifest.json`
- `p2ro_run_manifest.json`

要求：

- 不把 p2ro 控制目录打进交付包。
- `original_sessions/` 只保留必要原始过程记录。
- 交付包能被 p2r `scan` 识别。
- P5 必须通过或人工确认进入 `manual_required`。

### P10 upload-self-test-attachments

目标是把 P9 manifest 中的 report/log/summary 类产物上传为作业员自测附件。

输出：

- `p2ro_upload_manifest.json`
- `logs/P10_upload.log`

要求：

- 只上传 `dimension_type=ai_test_report` 的已存在文件。
- P10 由 p2ro 原生 self-test attachment adapter 直接调用平台 API，接管 quality-runner 现有上传职责。
- p2ro 只读取显式配置的 `API_KEY` 环境变量，不从 quality-runner 二进制、日志、进程环境或内存中提取凭据。
- p2ro 不输出、不持久化上传凭据。
- 走 `batch-presigned-url -> PUT presigned URL -> batch-confirm`。
- 上传前后执行 exists/list 对比。
- 上传失败不删除本地 artifact，不改变 P9 package。
- 日志脱敏 API key、Authorization、presigned URL、`X-Amz-Signature` 和 `X-Amz-Credential`。
- 不把自测附件上传等同自动平台最终提交或质检通过。

## 权限策略

权限不走自由文本 yes/no，统一走 policy。

| 行为 | 策略 |
| --- | --- |
| 读取 workspace 内文件 | auto allow |
| `rg`、列目录、查看 diff | auto allow |
| 写当前作业 workspace 内文件 | auto allow |
| 格式化、lint、test、build | isolated workspace 内 auto allow |
| 创建交付包内文件 | auto allow |
| 安装依赖、联网拉代码 | require human |
| 删除、批量移动、覆盖大范围文件 | require human |
| 修改 p2ro 控制目录外文件 | require human |
| git push/reset/rebase/merge | require human |
| 访问 workspace 外路径 | deny |
| 读取通用 secret/env/user home | deny |
| 调用 p2ro self-test upload adapter | require human |
| 读取显式配置的 `API_KEY` | require human |
| self-test attachment exists/list | auto allow |
| self-test attachment upload/confirm | require human |
| 访问生产数据库、付费 API、非白名单生产接口 | deny |
| 扩大需求范围 | require human |

权限事件结构：

```json
{
  "schema_version": "p2ro.permission.v1",
  "run_id": "",
  "stage": "P3",
  "action": "install_dependency",
  "risk": "network",
  "decision": "require_human",
  "reason": "",
  "requested_at": ""
}
```

## AgentRuntimeAdapter

现有 p2r 的 Codex app-server session 强制 read-only，适合 D/E/F 静态审查，不适合 p2ro 生产写码。Claude Code 当前也没有确认可用的 Codex 风格 app-server，因此 p2ro 的 agent 层必须以 runtime adapter 方式设计，先统一 p2ro 需要的能力，再分别接入 Claude SDK、Claude CLI stream-json 或 Codex app-server。

p2ro 需要新增 producer adapter：

```text
StartSession(job, policy)
SendInput(sessionID, input)
StreamEvents(sessionID)
RequestToolDecision(event)
Cancel(sessionID)
GetArtifacts(sessionID)
GetUsage(sessionID)
```

统一事件：

```text
message.delta
tool.call
tool.result
permission.request
permission.decision
file.changed
diff.updated
stage.failed
stage.completed
run.completed
```

adapter 只做协议转换，不承载业务 policy。是否允许写文件、联网、安装依赖由 p2ro policy engine 判断。

MVP adapter 优先级：

1. `ClaudeSDKAdapter`：首选实现，基于官方 Agent SDK，目标是长期稳定、事件语义清晰、权限决策可控。
2. `ClaudeCLIAdapter`：fallback 实现，基于 `claude -p --input-format stream-json --output-format stream-json`，由 p2ro 管理进程生命周期、stdin/stdout、session ID、取消和日志。
3. `CodexAppServerAdapter`：兼容参考实现，只在 Claude 路径 spike 不达标时作为临时生产后端；不复用 read-only reviewer 配置。

统一事件必须覆盖以下事实，而不是照搬某个后端的原始字段：

- agent 可见文本输出
- tool 调用和结果
- 文件写入和 diff 更新
- 权限请求、策略命中和人工决策
- stage 完成、失败、阻塞、取消
- token/费用/耗时统计
- runtime 崩溃和可恢复 session 标识

## Artifact 合同

p2ro 控制产物放在交付包外：

```text
.p2ro-control/
  index.db
  runs/<run_id>/
    run_manifest.json
    stage_status.json
    permissions.jsonl
    diff_summary.json
    logs/
```

交付包只保留 prompt2repo 标准结构：

```text
TASK-xxx/
  docs/
  repo/
  original_sessions/
  metadata.json
  .gitignore
```

MVP 必须保证 p2ro 控制产物不会污染最终交付包。

## TUI 信息架构

主视图建议使用作业端语义，而不是复用质检端文案：

```text
待生产 | 生产中/待人工 | 待质检/已完成
```

详情视图：

- 作业 prompt 摘要
- 当前阶段 P0-P9
- 流式输出
- diff 摘要
- 权限队列
- artifact 列表
- self-check findings
- 打包结果

快捷操作：

- 新建作业
- 启动/继续作业
- 暂停/取消
- 批准权限请求
- 查看 diff
- 打开交付包路径
- 运行 p2r 自检适配阶段

## 交付红线

p2ro 必须把以下规则机器化：

- 包根目录只能包含 `docs/`、`repo/`、`original_sessions/`、`metadata.json`、可选 `.gitignore`。
- 原始 prompt 必须写入 `metadata.json.prompt`。
- `repo/README.md` 必须匹配真实启动步骤、端口、账号、测试命令。
- fullstack/backend 默认必须支持 `docker compose up`。
- 不允许依赖 host-only 服务、绝对路径、私有镜像、未声明全局工具。
- 不允许 fake logic、硬编码成功响应、静态页面冒充真实功能。
- 必须有 `repo/unit_tests/`、`repo/API_tests/`、`repo/run_tests.*`。
- English prompt 的交付内容必须保持英文。
- 清理 `node_modules/`、`.venv/`、`.codex/`、`.vscode/`、缓存和数据库文件。

## 风险与约束

1. 长期维护整仓 fork 会导致共享 runtime/schema 漂移。
2. p2ro 写码 adapter 如果复用 p2r read-only Codex reviewer，会无法生产代码；如果假设 Claude 存在同构 app-server，会在实现阶段卡死。
3. 如果 p2ro 私改包校验规则，产物可能无法通过 p2r 质检。
4. 权限策略如果做成全局 allow，会带来删除、联网、密钥读取和 git 破坏风险。
5. P1 若过早写业务代码，会破坏测试冻结和文档驱动开发流程。
6. P2 若为了通过而弱化测试，会让 P3 失去约束。
7. TUI 如果直接复用质检端文案，会造成作业员心智错乱。

## MVP 验收

- p2ro 独立仓能编译独立二进制。
- 能创建本地作业并保存 prompt。
- 能执行 P0/P1/P2/P3/P5/P9/P10。
- P1 不写业务代码。
- P2 生成可运行测试，允许失败。
- P3 产生真实业务实现和 diff 摘要。
- P5 能执行 prompt2repo 交付红线检查。
- P9 生成能被 p2r `scan` 识别的交付包。
- P10 能按 artifact mapping 上传 `ai_test_report` 文档并生成脱敏 manifest。
- TUI 显示阶段、流式日志、权限请求、diff、findings 和 artifact。
- 权限策略至少覆盖 auto allow / require human / deny。
- p2ro 产物用 p2r Stage A 可识别并生成结构检查结果。
