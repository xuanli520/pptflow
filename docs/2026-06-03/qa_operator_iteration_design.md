# p2r 质检端与 p2ro 作业端迭代实现设计

## 结论

第一阶段不再大改质检端。p2r 质检端已经具备 Codex-guided Playwright 黑盒前端 E2E 阶段，用真实浏览器检查 Docker 启动后的页面是否可访问、可交互、无空白页、无明显控制台和网络错误。

Stage G 使用 Codex 自主浏览器测试，但 Codex 不直接执行 shell，也不直接调用 Playwright CLI。正确边界是：Codex 负责自主判断和规划；p2r 负责校验动作、限制权限、执行 Playwright wrapper；Playwright wrapper 只执行结构化 allowlist action。这样保留 Codex 处理复杂 runtime URL、入口页、路由、登录页和交互路径的能力，同时避免把浏览器阶段变成权限绕过入口。

p2ro 作业端不是 Claude Code 聊天壳，而是全自动 prompt-to-repo 生产流水线。它把初始化、测试冻结、开发、联调、自检、修复、打包变成有限状态机，用权限策略取代 Claude Code 中反复出现的 yes/no 确认。

质检员题目管理系统里的“质检通过 / 质检打回 / 上传 AI 报告 / 输入质检意见”属于后续网页自动化或抓包分析范围，不和作业员 `run_quality.py` 自测附件 API 混用。

## 当前边界

### p2r 质检端

当前 p2r 已有流水线：

```text
A 结构与规则检查
D 测试有效性静态审查
E prompt 到代码静态审查
F 修复报告静态审查
B Docker runtime evidence
C run_tests runtime evidence
```

第一阶段只新增：

```text
G Codex-guided Playwright browser E2E
```

推荐运行顺序：

```text
A -> D -> E -> F -> B -> G -> C
```

G 不并入 C。C 只负责执行交付包自己的 `repo/run_tests.sh`；G 负责站在质检员视角，用浏览器黑盒验证前端真实可用性。

B 失败或没有可用 runtime 时，不能只跳过紧邻的 G。必须显式阻断所有 runtime dependent stages：

```text
B failed/no runtime -> G blocked + C blocked
```

### p2ro 作业端

p2ro 采用独立 fork 仓库中的作业端产品层，但共享 p2r 已稳定的 repo、pipeline 基础设施、artifact 结构和质检能力：

```text
prompt -> repo -> self-check -> repair -> recheck -> submit for QA
```

MVP 从 p2r fork 起步，保留 upstream 同步；生产逻辑放入 p2ro 专属目录，不把 P0-P10 塞进 p2r A-G 质检阶段。后续当 shared 文件频繁双向修改时，再抽出 `p2r_core`。

### 两套上传链路

作业员自测 API：

```text
p2ro native self-test attachment adapter
external self-test attachment API
```

用途是作业员上传自测附件，不能等同质检员裁决。`run_quality.py` / `quality-runner.exe` 只作为已验证 API 行为的历史参考，P10 由 p2ro 接管上传实现。

已实测的 self-test attachment API：

```text
POST /api/submissions/external/self-test-attachments/exists
Content-Type: application/json
X-API-Key: <configured key>

{
  "external_task_id": "TASK-...",
  "dimension_type": "ai_test_report",
  "file_name": "codex_report.md"
}
```

```text
POST /api/submissions/external/self-test-attachments
Content-Type: application/json
X-API-Key: <configured key>

{
  "external_task_id": "TASK-...",
  "dimension_type": "ai_test_report"
}
```

```text
POST /api/submissions/external/self-test-attachments/batch-presigned-url
Content-Type: application/json
X-API-Key: <configured key>

{
  "external_task_id": "TASK-...",
  "files": [
    {
      "dimension_type": "ai_test_report",
      "file_name": "codex_report.md",
      "file_size": 54690,
      "file_type": "text/markdown"
    }
  ]
}
```

```text
PUT <presigned upload_url>
Content-Type: text/markdown
Body: file bytes
```

```text
POST /api/submissions/external/self-test-attachments/batch-confirm
Content-Type: application/json
X-API-Key: <configured key>

{
  "attachment_ids": ["<attachment_id>"]
}
```

观测结论：

- `quality-runner.exe run` 支持 `--path`、`--task-id`、`--timeout`、`--report-name`、`--quiet`、`--verbose`、`--skip-ui`、`--skip-ui-interaction`、`--model`，没有 `--skip-upload`。
- `exists` 对不存在任务返回 HTTP 400，body 为任务不存在。
- 附件列表接口可能返回 HTTP 200 但业务码 407，需要同时检查 HTTP 状态和业务码。
- 已对真实任务做只读验证，`ai_test_report/codex_report.md` 可返回存在状态、文件大小、`text/markdown` 类型和 `download_url`。
- `download_url` 可直接读取报告内容。
- 已对真实业务任务完成 `batch-presigned-url -> PUT OBS presigned URL -> batch-confirm` 写入验证。
- `batch-presigned-url` 返回 `attachment_id`、`upload_url`、`object_path`、`expires_in`；`upload_url` 是短时有效签名 URL，日志和文档必须脱敏。
- `batch-confirm` 返回 `success_ids` 和 `failed_items`，必须按业务字段判断确认结果。
- 官方 sample 对 `operator_codex_report.md`、`run_tests.log`、`docker_startup.log`、`prompt_requirements_verification.md`、`codex_report_issues_verification.md`、`test_effectiveness_report.md`、`ui_interaction_steps.md`、`ui_interaction_report.md`、`final_validation_report.md` 都使用同一个 `dimension_type=ai_test_report` 查询。
- 这里的字段名是 `ai_test_report`，不是 `api_test_report`。

### 认证方式与凭据边界

API 使用 `X-API-Key` 请求头进行认证。P10 由 p2ro 原生 adapter 读取显式配置的上传凭据，默认环境变量名为 `API_KEY`，也允许通过配置覆盖为其他变量名。

安全要求：

- `API_KEY` 不要在日志、stdout、manifest、DB 中明文打印或持久化。
- p2ro 只能读取自身进程可见的显式配置，不从 quality-runner 二进制、日志、进程环境或内存中提取密钥。
- quality-runner 内部只读任务列表 key 不是上传凭据，p2ro 不依赖、不抽取、不复用。
- `batch-presigned-url` 返回的 `upload_url` 包含 `X-Amz-Signature`、`X-Amz-Credential` 等签名参数，日志中同样需要脱敏。
- `API_KEY` 不可见或未授权时，P10 进入 `manual_required`。

p2ro adapter 边界：

- 配置项包括 provider、base URL、timeout、external task ID、dimension type、本地 artifact manifest path 和上传凭据 env 名。
- MVP provider 是 `native_http`：p2ro 直接调用 exists/list/batch-presigned-url/PUT/batch-confirm。
- quality-runner 只作为兼容验证对象，不作为 P10 前置依赖。
- p2ro 不通过反编译、日志、内存或进程环境扫描从 quality-runner 中提取上传密钥。
- report/log/summary 类附件首期统一使用 `dimension_type=ai_test_report`。
- 首期支持已实测文件名的 exists/list/download_url/upload/confirm。
- 写入必须只上传当前任务的正式报告，不上传 probe 文件。
- 上传失败只记录 adapter finding，不删除本地 artifact，不影响 P9 package 留存。
- adapter 必须显式维护 local path 到平台 `file_name` 的映射；不能把 `logs/B_docker.log`、`logs/C_tests.log` 这类本地路径直接作为平台文件名。

平台附件映射：

| 本地产物 | 平台 file_name | dimension_type | file_type |
| --- | --- | --- | --- |
| `codex_report.md` | `codex_report.md` | `ai_test_report` | `text/markdown` |
| `codex_report_verification.md` | `codex_report_verification.md` | `ai_test_report` | `text/markdown` |
| `operator_codex_report.md` | `operator_codex_report.md` | `ai_test_report` | `text/markdown` |
| `operator_prompt_requirements_verification.md` | `operator_prompt_requirements_verification.md` | `ai_test_report` | `text/markdown` |
| `prompt_requirements_verification.md` | `prompt_requirements_verification.md` | `ai_test_report` | `text/markdown` |
| `operator_codex_report_issues_verification.md` | `operator_codex_report_issues_verification.md` | `ai_test_report` | `text/markdown` |
| `codex_report_issues_verification.md` | `codex_report_issues_verification.md` | `ai_test_report` | `text/markdown` |
| `test_effectiveness_report.md` | `test_effectiveness_report.md` | `ai_test_report` | `text/markdown` |
| `test_effectiveness_verification.md` | `test_effectiveness_verification.md` | `ai_test_report` | `text/markdown` |
| `final_validation_report.md` | `final_validation_report.md` | `ai_test_report` | `text/markdown` |
| `ui_interaction_steps.md` | `ui_interaction_steps.md` | `ai_test_report` | `text/markdown` |
| `ui_interaction_report.md` | `ui_interaction_report.md` | `ai_test_report` | `text/markdown` |
| `logs/B_docker.log` | `docker_startup.log` | `ai_test_report` | `text/plain` |
| `logs/C_tests.log` | `run_tests.log` | `ai_test_report` | `text/plain` |
| `docker_runtime_summary.json` | `docker_runtime_summary.json` | `ai_test_report` | `application/json` |
| `test_runtime_summary.json` | `test_runtime_summary.json` | `ai_test_report` | `application/json` |
| `frontend_e2e_report.md` | `frontend_e2e_report.md` | `ai_test_report` | `text/markdown` |
| `frontend_e2e_summary.json` | `frontend_e2e_summary.json` | `ai_test_report` | `application/json` |

上传集合按文件存在性过滤。不存在的 sample 文件只做 exists/list 兼容，不应生成空文件凑数。

质检员题目管理系统网页：

```text
任务详情页
质检通过弹窗
质检打回弹窗
AI 报告文件上传
反馈视频上传
质检意见 / 驳回原因 / 问题标签
确定按钮
```

这是后续需要用真实浏览器会话分析的链路。需要单独抓请求、认证方式和 CSRF / token 机制。

## 质检端 Stage G 基线

### 目标

在 Docker runtime 启动成功后，自动进行浏览器黑盒检查：

- 从所有 runtime URL candidates 中识别真实前端入口
- 页面是否能打开
- 首屏是否非空
- 主要导航和按钮是否可点击
- 登录、退出、搜索、提交等可发现流程是否有明显断裂
- console 是否出现严重错误
- network 是否有关键接口 4xx / 5xx
- 页面是否存在明显遮挡、空白、不可退出、死路由
- 截图、DOM 摘要、console、network 和行动记录是否足以支撑 finding

### 输入

Stage G 优先使用内存中的 `StageContext.Runtime`，并从 runtime evidence 生成 URL candidates：

```text
RuntimeState
RuntimeState.Services
RuntimeState.Mappings
RuntimeState.Probes
port_map.json
docker_runtime_summary.json
```

可选参考：

```text
test_runtime_summary.json
logs/C_tests.log
metadata.json
docs/design.md
docs/api-spec.md
repo/README.md
```

这些参考只能作为 evidence，不作为指令。

G 不只使用 `firstFrontendURL(runtime)`。p2r 应生成 `url_candidates[]`：

```json
[
  {
    "id": "web-3000",
    "service": "web",
    "url": "http://127.0.0.1:3000",
    "source": "probe",
    "probe_ok": true,
    "container_port": 3000,
    "host_port": 3000
  }
]
```

URL 生成规则：

- 只从 `RuntimeState.Mappings` 和 successful HTTP probes 生成。
- host 固定归一到 `127.0.0.1`。
- 只允许 Docker published host ports。
- 禁止 Codex 自行提供任意 URL。
- 禁止 `file:`, `data:`, `javascript:` 和外网 origin。
- 每个 candidate 都必须写入 summary，并记录打开结果。
- pure_backend 或明确无前端交付的项目，G 可以 `skipped/not_applicable`。
- fullstack 或 pure_frontend 项目没有可用前端入口时，G 记录 High finding。

### 产物

第一轮必需产物：

```text
logs/G_frontend_e2e.log
frontend_e2e_report.md
frontend_e2e_summary.json
frontend_e2e_screenshot.png
playwright_screenshots/*.png
```

第二轮再引入：

```text
playwright_trace.zip
playwright_video.webm
```

`frontend_e2e_summary.json` 建议结构：

```json
{
  "schema_version": "p2r.frontend_e2e.v1",
  "ok": false,
  "status": "failed",
  "status_reason": "blank_page",
  "project_role": "qa_review",
  "started_at": "",
  "duration_ms": 0,
  "url_candidates": [],
  "start_url": "http://127.0.0.1:3000",
  "routes_checked": [],
  "actions": [],
  "screenshots": [],
  "console_errors": [],
  "page_errors": [],
  "network_errors": [],
  "blocked_actions": [],
  "artifacts": {
    "log": "logs/G_frontend_e2e.log",
    "report": "frontend_e2e_report.md",
    "summary": "frontend_e2e_summary.json",
    "primary_screenshot": "frontend_e2e_screenshot.png"
  },
  "findings": [
    {
      "severity": "High",
      "title": "",
      "route": "",
      "evidence": "",
      "screenshot": "",
      "minimum_fix": ""
    }
  ],
  "schema_validation_error": ""
}
```

### Codex 与 Playwright 执行方式

Stage G 不应复用 D/E/F 的静态审查执行器。现有静态 Codex app-server 固定 read-only、no network，而 G 需要访问本地 Docker published ports，并写截图和 browser evidence。

推荐新增 runtime browser harness：

```text
profile: runtime_browser_e2e
repo access: read-only
artifact access: writable
network: 127.0.0.1 + allowlisted Docker published ports only
approval: never for validated browser actions
```

Codex 的职责：

- 根据 prompt、metadata、docs、README、runtime candidates 制定浏览器检查策略
- 选择最可能的前端入口
- 基于浏览器观察结果自主决定下一步动作
- 汇总失败证据和 findings
- 输出 `frontend_e2e_summary.json` 和 `frontend_e2e_report.md`

p2r 的职责：

- 启动 Docker
- 从 `RuntimeState` 生成 URL allowlist
- 启动受控 browser harness
- 校验 Codex action JSON
- 执行 Playwright wrapper
- 拦截非 allowlist network request
- 落盘日志、截图、summary、report
- 校验 summary schema
- 将 findings 写入现有 `StageRecord`
- 校验 Stage G 前后没有修改交付包源码

Playwright wrapper 的职责：

- 只接受 p2r 定义的结构化 action
- 不接受任意 shell、任意 CLI 参数、任意 JS eval
- 不读取 workspace 外文件
- 不写 artifact root 以外路径
- 不暴露 host env、HOME、USERPROFILE、CODEX_HOME、API key、token

Codex 每一步只能输出 action JSON：

```json
{
  "action": "click_button",
  "target": {
    "kind": "role",
    "role": "button",
    "name": "Login"
  },
  "risk": "local_stateful",
  "reason": "login is required before the main workspace is visible",
  "expect": "workspace or dashboard becomes visible"
}
```

允许的 Playwright wrapper 动作：

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

动作风险分级：

```text
read_only       打开页面、截图、等待、读取 DOM、读取 console/network
navigation      点击链接、菜单、标签页、路由跳转
local_stateful  在本地 Docker runtime 内登录、搜索、筛选、提交测试表单
destructive     删除、支付、购买、发送邮件、上传真实文件、最终平台提交
```

禁止动作：

```text
git
docker
npm install
curl arbitrary URL
shell command
arbitrary Playwright CLI
arbitrary JS eval
访问 workspace 外路径
修改交付包源码
读取 secret/env/user home
访问非 allowlist origin
上传真实本地文件
点击外部平台最终提交
```

Stage G 不使用现有静态 Codex sandbox env。需要独立 `BrowserAgentEnv`：

```text
PATH
SYSTEMROOT / WINDIR / COMSPEC
TEMP / TMP
LANG / TZ
必要的 Playwright runtime 变量
```

默认不传：

```text
HOME
USERPROFILE
CODEX_HOME
XDG_CONFIG_HOME
XDG_CACHE_HOME
XDG_DATA_HOME
OPENAI_API_KEY
CODEX_API_KEY
TOKEN / SECRET / PASSWORD
HTTP_PROXY / HTTPS_PROXY with credentials
```

### 失败判定

Stage G package finding 条件：

- fullstack/pure_frontend 项目没有可用前端 URL
- 页面打不开或首屏空白
- Codex 找不到任何可用入口页
- 核心交互路径无法完成
- 严重 console/runtime error
- 关键 API 4xx / 5xx 导致主流程不可用
- 页面存在明显遮挡、空白、不可退出、死路由

Stage G infra blocked 条件：

- Playwright wrapper 不可用
- browser runtime 无法启动
- artifact root 不可写
- summary schema 无法落盘
- action schema validator 不可用
- Codex 输出 summary schema 无效

Stage G 失败时不修改交付包，只记录 finding。G 前后需要对 `repo/` 做 hash 或 snapshot 检查；发现源码变更时记录 infra High finding。

## 质检端后续：网页提交自动化

截图中的质检员操作属于题目管理系统前端，不属于作业员自测 API。

后续分析顺序：

1. 用真实质检员账号打开任务详情页。
2. 点击“质检通过”和“质检打回”。
3. 用 Playwright 记录 network requests。
4. 分析上传 AI 报告、上传反馈视频、保存草稿、确认提交的 endpoint。
5. 分析认证方式：cookie、Bearer token、CSRF token、隐藏 form 字段。
6. 建立最小自动化：填文本、上传文件、保存草稿。
7. 最后才考虑自动点击“确定”提交通过或打回。

这部分默认不进入第一阶段。

安全边界：

- 默认只自动保存草稿，不自动最终提交。
- 自动最终提交必须显式开启。
- 每次提交前展示将上传的文件、评语、问题标签和目标任务 ID。
- 保留人工 fallback。

## p2ro 作业端生产流水线

### 目标

p2ro 取代作业员在 Claude Code 中手动交互的方式，提供自动化生产流水线：

```text
业务 prompt -> 可交付 prompt2repo 包 -> 自检 -> 修复 -> 待质检
```

作业员看到的是：

- 当前阶段
- 流式日志
- diff
- 失败 findings
- 截图和测试结果
- 少量权限队列

作业员不需要反复回答 Claude Code 的 yes/no。

### 参考流程映射

来自 `H:\project\mindflow\prompt2repo\项目流程提示词` 的流程可拆成：

```text
初始化：只搭建仓库框架、设计文档、冻结文档，不写业务代码
测试冻结：根据文档和 prompt 写测试，测试能运行即可，允许失败
循环开发：分析、编码、测试，直到核心模块完成
前端联调：按后端契约实现可交付前端并联调
循环审查：静态交付验收和架构审计
循环修复：按审查报告修复 high / medium / low
修复确认：回归检查和最终收口
```

### p2ro Stage 设计

```text
P0 intake
解析 prompt、项目类型、语言、交付边界、不可替代需求。

P1 scaffold
生成 docs/、repo/、original_sessions/、metadata.json 骨架。
生成 docs/design.md、docs/questions.md、docs/api-spec.md。
不写业务代码。

P2 test-freeze
生成 repo/unit_tests/、repo/API_tests/、repo/run_tests.sh。
测试必须能运行，允许因业务代码未实现而失败。

P3 implement
按冻结文档和测试实现业务代码。
循环执行分析、编码、测试。

P4 frontend-contract
实现前端和后端契约联调。
覆盖主要页面、状态、错误反馈、登录/退出等可见流程。

P5 self-check
运行本地结构检查、依赖检查、fake impl 检查、validate_package。

P6 runtime-check
复用 p2r 的 B/G/C：
B Docker runtime
G Codex-guided Playwright browser E2E
C run_tests runtime evidence

P7 review
复用或改造 D/E/F 静态审查能力，生成 findings。

P8 repair
按 findings 自动修复，阻断项优先，高风险项优先。

P9 package
清理 original_sessions，写 handoff summary，生成待质检交付包。
```

### 状态机

```text
queued
intaking
scaffolding
test_freezing
implementing
contract_testing
self_checking
runtime_checking
reviewing
repairing
packaging
ready_for_qa
qa_failed
qa_passed
manual_required
failed_blocked
```

修复循环：

```text
reviewing -> repairing -> self_checking -> runtime_checking -> reviewing
```

终止条件：

- 所有必需检查通过
- 同类失败重复超过阈值
- 超过最大修复轮次
- 权限请求需要人工处理
- Docker 或环境不可用
- prompt 本身不可实现

### 权限策略

作业端不能使用无边界的全局 bypass。yes/no 应前置成 policy：

```text
读文件 / rg / 查看 diff              auto allow
写目标 workspace 内任务文件           auto allow
格式化 / lint / test / build          auto allow in isolated workspace
生成临时文件 / 清理测试产物            auto allow
安装依赖 / 联网拉代码                 require human
删除 / 批量移动 / 覆盖大范围文件       require human
git push / reset / rebase / merge      require human
访问 workspace 外路径                 deny
读取 secret / env / user home          deny
生产环境 / 数据库 / 付费 API           deny
扩大需求范围                          require human
```

人工提示词不进入自由聊天模式，而是作为结构化 pipeline input：

```json
{
  "type": "human_prompt_injection",
  "scope": "current_stage",
  "effect": "replan",
  "text": "页面必须支持退出登录，并且退出后不能访问工作区"
}
```

注入后记录版本、影响阶段和审计日志。

## 共享架构

### 共享模块

```text
Job model
Run / Stage / Finding model
Artifact store
AgentRuntimeAdapter
Docker runtime harness
BrowserActionValidator
Playwright E2E harness
Task docs / attachment manifest
Stream event model
Repair brief schema
```

### AgentRuntimeAdapter

统一 Codex、Claude app server 差异：

```text
startSession(job, policy)
sendMessage(sessionId, input)
streamEvents(sessionId)
requestToolDecision(event)
cancel(sessionId)
getArtifacts(sessionId)
getUsage(sessionId)
```

统一事件：

```text
message.delta
tool.call
tool.result
permission.request
permission.decision
browser.action.requested
browser.action.blocked
browser.action.completed
file.changed
stage.failed
stage.completed
run.completed
```

Claude / Codex adapter 只负责协议转换，不承载业务 policy。

## Prompt2Repo 交付红线

p2ro 必须把以下规则机器化：

- 包根目录只能包含 `docs/`、`repo/`、`original_sessions/`、`metadata.json`、可选 `.gitignore`
- 原始 prompt 必须写入 `metadata.json.prompt`
- `repo/README.md` 必须匹配真实启动步骤、端口、账号、测试命令
- fullstack/backend 默认必须支持 `docker compose up`
- 不允许依赖 host-only 服务、绝对路径、私有镜像、未声明全局工具
- 不允许 fake logic、硬编码成功响应、静态页面冒充真实功能
- 必须有 `repo/unit_tests/`、`repo/API_tests/`、`repo/run_tests.*`
- English prompt 的交付内容必须保持英文
- 清理 `node_modules/`、`.venv/`、`.codex/`、`.vscode/`、缓存和数据库文件

这些不应靠作业员记忆，应变成 P5/P9 的硬检查。

## 迭代计划

### Iteration 1：质检端 Stage G 基线

当前源码已落地：

- `internal/pipeline/model/stages.go` 已定义 `A,D,E,F,B,G,C`，G 是 runtime stage。
- `internal/pipeline/stage.go` 已实现 B 阻断 G/C 的 dependency graph 和 G blocked artifact。
- `internal/pipeline/stage_g.go`、`browser_action.go`、`browser_policy.go`、`browser_url.go`、`frontend_e2e_schema.go`、`repo_snapshot.go` 已提供 Codex-guided browser loop、action validator、URL allowlist、summary/finding 映射和源码不变校验。
- `internal/browser/` 已封装 Playwright wrapper 和 observation。
- `assets/prompt_profiles/frontend_e2e_browser.md`、`frontend_e2e_browser_action_prompt.md` 已存在。

后续只保留 hardening 范围：

- 用 p2ro fixture 回归 B/G/C 兼容性。
- 补齐真实 fullstack、pure_frontend、pure_backend 样例。
- 继续收敛 G findings 到 p2ro repair brief。

涉及文件：

```text
internal/pipeline/model/stages.go
internal/pipeline/stage.go
internal/pipeline/stage_g.go
internal/pipeline/browser_action.go
internal/pipeline/browser_runtime.go
internal/pipeline/run_lifecycle.go
internal/config/config.go
internal/preflight/preflight.go
internal/tui/localize.go
cmd/run.go
assets/prompt_profiles/frontend_e2e_browser.md
```

验收：

- B 成功后 G 能拿到所有 runtime URL candidates
- Codex 能在 allowlist 内自主选择入口并发起浏览器动作
- p2r 能拦截非 allowlist URL
- p2r 能拦截 shell、任意 Playwright CLI、任意 JS eval
- G 能生成截图、summary、report
- 页面空白时产生 High finding
- console runtime error 能进入 summary
- 关键 network 5xx 能进入 summary
- Playwright 不可用时 G 是 blocked/infra finding，不是 package High finding
- B 失败时 G/C 都被阻断，不误跑 C
- G 不修改交付包源码
- C 仍只负责 `run_tests.sh`

### Iteration 2：p2ro 本地生产 MVP

范围：

- 新增 operator 命令入口或 mode
- 新增 P0/P1/P2/P3/P5/P9/P10 最小阶段
- agent session 支持流式输出
- 权限策略支持 auto allow / require human / deny
- 产出标准 prompt2repo 包
- 上传 report/log/summary 类 `ai_test_report` 自测附件

不做：

- 多模型调度
- 自动平台提交
- 自动最终质检通过/打回
- 多人协作

### Iteration 3：p2ro 自检闭环

范围：

- 接入 B/G/C runtime check
- 接入 repair brief
- 支持最多 N 轮自动修复
- 每轮保存 diff、日志、失败证据

验收：

- 前端空白页能被 G 捕获并回传修复
- `run_tests.sh` 失败能进入 repair
- 重复失败能进入 `manual_required`

### Iteration 4：作业员自测 API 上传

范围：

- 复用已分析的作业员 self-test attachment API
- 首期只读验证平台已有 `ai_test_report/codex_report.md`
- 上传 p2ro 产出的 report/log/summary 类 `ai_test_report` 附件
- 不把自测通过等同质检通过

要求：

- 上传凭据通过 p2ro 显式配置的环境变量注入，默认读取 `API_KEY`。
- p2ro 不硬编码、不导出、不打印 token
- `API_KEY` 不可见或未授权时进入 `manual_required`
- 上传失败不影响本地 artifact 留存
- HTTP 200 仍需检查业务码和响应结构
- 写入前后必须记录 exists/list 对比
- 不支持覆盖同名文件，除非官方确认覆盖语义
- 日志必须脱敏 `upload_url`、`X-Amz-Signature`、`X-Amz-Credential`、API key 和 Authorization 值

### Iteration 5：质检员网页提交自动化

范围：

- 用真实题目管理系统网页抓包
- 分析质检通过/打回 endpoint、上传 endpoint、认证机制
- 先实现保存草稿
- 再实现可选最终提交

要求：

- 明确区分“通过意见”和“驳回原因”
- 支持 AI 报告文件和反馈视频上传
- 最终提交前必须展示确认摘要

## 风险

1. Codex runtime browser stage 不能复用现有 read-only static reviewer，必须新增受控 harness。
2. Playwright wrapper 如果允许任意命令，会变成权限绕过入口，必须结构化 action schema 化。
3. Runtime URL 复杂，不能只用 `firstFrontendURL(runtime)`，必须遍历 candidates 并让 Codex 判断入口。
4. B 失败阻断逻辑不能继续依赖 `SkipNextStage`，必须显式阻断 G/C。
5. 作业端自动化不能把所有确认都变成 yes，否则会带来删除、联网、密钥读取和 git 破坏风险。
6. 质检员网页提交接口可能有 CSRF、签名、临时上传凭证，必须实测，不要从作业员 API 推断。
7. p2ro 和 p2r 必须共享 artifact/schema，否则后续修复和复验会割裂。

## 当前推荐落地顺序

Stage G 已经成为当前 p2r runtime pipeline 基线。下一步直接做 p2ro operator MVP，把 prompt2repo 生产流程机器化。作业端复用本地 artifact 和 B/G/C，并把作业员 self-test attachment API 上传作为 P10 原生接管。

最后再做质检员题目管理系统网页自动提交。它和作业员自测 API 上传分开实现、分开认证、分开配置。
