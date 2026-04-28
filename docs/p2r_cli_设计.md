# p2r QA CLI 详细设计

## 1. 目标与范围

### 1.1 工具定位

`p2r` 是 prompt2repo 生态的命令行质控编排工具，为质检员提供：

- 自动发现并索引 `projects-qa/` 目录下的 p2r 交付包
- 一键执行结构检查、运行证据采集、静态 AI 审查、修复建议汇总
- TUI 交互式面板：项目总览、执行监控、失败定位、报告入口
- 零服务端单二进制分发，CI 和本地均可用

`p2r` 不替代质检员的最终判断。CLI 只负责收集证据、整理报告、预填建议。最终 `PASS / REWORK / FAIL` 仍由质检员人工确认。

### 1.2 MVP 边界

**本轮交付：**

- `p2r scan` — 项目发现 + 索引
- `p2r run <task-id>` — 执行 6 阶段质检流水线
- `p2r status <task-id>` — 查询项目运行历史、阶段状态、关键 findings
- `p2r tui` — 启动交互式面板（项目总览 + 执行面板）

**本轮不交付：**

- 批量并行执行
- TUI Report 面板和 Prompt 面板完整编辑能力
- 自动勾选 `PASS / REWORK / FAIL`
- 替代人工静态审计报告模板

### 1.3 关键澄清：静态报告模板是权威报告入口

质检员静态质检报告已有固定模板。`p2r` 必须把模板作为内置 prompt profile 执行，不得自造一套不兼容的审查结构。

内置模板：

| 模板 | 用途 | 运行边界 | 模板要求输出 |
|------|------|----------|--------------|
| `static_acceptance_audit.md` | Delivery Acceptance and Project Architecture Audit | 纯静态；不启动项目、不运行 Docker、不运行测试、不改代码 | `./.tmp/static_acceptance_audit_report.md` |
| `tests_coverage_report.md` | Review the effectiveness of project tests | 纯静态；按项目类型检查 API/前端测试有效性 | `./.tmp/tests_coverage_report.md` |

阶段 D/E 可以读取源码、文档、测试文件和既有证据文件，但不得在 Codex 审查过程中启动项目、运行 Docker、运行测试或修改代码。任何依赖真实运行结果的结论必须标注 `Cannot Confirm Statistically` 或 `Manual Verification Required`，除非只是引用 B/C 阶段已经采集到的外部运行证据。

---

## 2. 技术选型

### 2.1 语言与框架

| 层面 | 选型 | 理由 |
|------|------|------|
| 语言 | Go 1.22+ | 单二进制分发、强并发控制、适合封装 docker/codex/python 子进程 |
| CLI | [Cobra](https://github.com/spf13/cobra) | Go 生态标准，子命令 + flag 管理 |
| TUI | [Bubble Tea](https://github.com/charmbracelet/bubbletea) | Elm 架构，适合面板式 TUI |
| 样式 | [Lip Gloss](https://github.com/charmbracelet/lipgloss) | 声明式终端样式 |
| 组件 | [Bubbles](https://github.com/charmbracelet/bubbles) | 内建 table/viewport/spinner/textinput |
| 存储 | [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | 纯 Go SQLite driver；保留 `database/sql` 与 SQLite schema，同时避免 Windows 默认构建依赖 CGO/GCC |
| CLI 进度 | [log/slog](https://pkg.go.dev/log/slog) | 标准库结构化日志，CI 友好 |

### 2.2 选型关键

- 现有 QA 脚本通过 `exec.Cmd` 调用，不做 Go 重写
- Docker 操作通过 `docker` CLI 调用，不引入 Docker SDK
- Codex 调用通过隔离沙箱执行，Codex 阶段始终是静态审查
- Python 脚本、Prompt 模板通过 `embed.FS` 随二进制分发，首次运行释放到 `.qa-control/`

### 2.3 分发方式

- `goreleaser` 交叉编译：linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64
- `go install github.com/.../p2r@latest` 作为备选渠道

---

## 3. 项目布局

```text
p2r/
├── main.go
├── go.mod / go.sum
├── .goreleaser.yaml
│
├── cmd/
│   ├── root.go
│   ├── scan.go
│   ├── run.go
│   ├── status.go
│   └── tui.go
│
├── internal/
│   ├── scanner/
│   │   └── scanner.go
│   ├── pipeline/
│   │   ├── pipeline.go
│   │   ├── model.go             # 阶段状态、artifact、finding schema
│   │   ├── stage_a.go           # 结构与规则脚本检查
│   │   ├── stage_b.go           # Docker 运行证据采集
│   │   ├── stage_c.go           # run_tests.* 运行证据采集
│   │   ├── stage_d.go           # 测试有效性静态审查
│   │   ├── stage_e.go           # 质检员静态审计报告
│   │   └── stage_f.go           # 汇总报告 + short_comment
│   ├── docker/
│   │   └── manager.go           # Compose/容器/端口/GC
│   ├── codex/
│   │   └── sandbox.go           # 静态审查沙箱
│   ├── executor/
│   │   └── cmd.go               # exec.Cmd 封装
│   ├── db/
│   │   └── store.go
│   ├── config/
│   │   └── config.go
│   └── tui/
│       ├── app.go
│       ├── overview.go
│       ├── execution.go
│       └── components/
│           ├── searchbox.go
│           ├── filterchips.go
│           ├── stagebar.go
│           └── logviewport.go
│
└── assets/
    ├── config.yaml
    ├── scripts/
    │   ├── run_acceptance.py
    │   ├── run_validate.py
    │   ├── check_required_artifacts.py
    │   ├── check_readme_alignment.py
    │   ├── check_local_dependency.py
    │   ├── check_fake_impl.py
    │   └── check_english_only.py
    └── prompt_profiles/
        ├── static_acceptance_audit.md
        ├── tests_coverage_report.md
        └── annotator_fix.md
```

---

## 4. CLI 命令设计

### 4.1 命令树

```text
p2r
├── scan [--path <dir>]
├── run <task-id> [--stage A..F] [--from A..F] [--static-only]
├── status <task-id> [--run <run-id>]
└── tui [--path <dir>]
```

### 4.2 `p2r scan`

```text
p2r scan --path ./projects-qa
```

行为：

- 递归扫描指定目录
- 识别同时包含 `docs/`、`repo/`、`original_sessions/`、`metadata.json` 的 p2r 交付包
- 将新项目写入 `index.db`
- 记录 `task_id`、批次、路径、最近运行状态

输出示例：

```text
[scan] 扫描 ./projects-qa/batches/ ...
[scan] 发现 12 个目录，识别 8 个 p2r 项目
[scan] 新增 3: TASK-20260408-A1B2C3, TASK-20260408-D4E5F6, TASK-20260409-G7H8I9
[scan] 已存在 5 (跳过)
```

### 4.3 `p2r run`

```text
p2r run TASK-20260408-A1B2C3
p2r run TASK-20260408-A1B2C3 --stage D
p2r run TASK-20260408-A1B2C3 --from C
p2r run TASK-20260408-A1B2C3 --static-only
```

`--static-only` 只执行 A、D、E、F，并将 B/C 标记为 `skipped`。该模式用于只需要生成质检员静态审计报告的场景，严格不启动项目、不运行 Docker、不运行测试。

CI 输出示例：

```text
[run] task=TASK-20260408-A1B2C3 run_id=run-20260428-001
[A] 结构与规则检查 ...................... done (2.3s)
[B] Docker 运行证据 ...................... failed (30.0s)
[C] run_tests.* 运行证据 ................. blocked (B failed)
[D] 测试有效性静态审查 .................. done (18.4s)
[E] 质检员静态审计报告 .................. done (25.2s)
[F] 汇总报告 + short_comment ............. done (1.8s)
[run] completed_with_findings, 输出: TASK-.../qa/runs/run-20260428-001/
```

### 4.4 `p2r status`

```text
p2r status TASK-20260408-A1B2C3
p2r status TASK-20260408-A1B2C3 --run run-20260428-001
```

输出应包含：

- run 状态：`running`、`completed_clean`、`completed_with_findings`、`aborted`、`crashed`
- 每个阶段状态：`pending`、`running`、`done`、`failed`、`blocked`、`skipped`
- blocking/high finding 数量
- 人工结论状态：`unset`、`pass`、`rework`、`fail`
- 关键 artifact 路径

### 4.5 `p2r tui`

```text
p2r tui --path ./projects-qa
```

MVP 只展示项目总览和执行面板。Report/Prompt 完整面板后续版本交付，不在 MVP TabBar 中显示死入口。

---

## 5. 6 阶段流水线

### 5.1 执行原则

流水线是“证据收集 + 静态审查 + 汇总”的组合，不是单纯测试 runner。

- A 是结构与基础规则 hard gate
- B 是运行证据采集，依赖 A 的基础结构存在
- C 依赖 B 成功提供可执行容器或服务
- D/E 是纯静态 Codex 审查，不依赖 B/C 成功
- F 永远执行，汇总 `done / failed / blocked / skipped` 输入
- Codex 阶段必须遵守静态报告模板的边界：不启动项目、不运行 Docker、不运行测试、不改代码

### 5.2 阶段状态模型

状态枚举：

| 状态 | 语义 |
|------|------|
| `pending` | 未开始 |
| `running` | 正在执行 |
| `done` | 阶段完成并产出 artifact |
| `failed` | 阶段执行过但失败 |
| `blocked` | 前置条件不满足，不能执行 |
| `skipped` | 用户模式或 flag 明确跳过 |

`stage_status.json` 必须是结构化文件：

```json
{
  "run_id": "run-20260428-001",
  "stages": [
    {
      "stage": "C",
      "name": "run_tests runtime evidence",
      "status": "blocked",
      "blocked_by": ["B"],
      "started_at": null,
      "finished_at": null,
      "duration_ms": 0,
      "log_path": "logs/C_tests.log",
      "artifact_paths": [],
      "findings": [
        {
          "id": "P2R-C-BLOCKED-001",
          "severity": "High",
          "rule": "Runtime tests require a runnable service/container.",
          "evidence": "Stage B failed.",
          "impact": "No runtime test evidence was collected.",
          "minimum_fix": "Fix Docker startup or rerun in --static-only mode."
        }
      ]
    }
  ]
}
```

### 5.3 QA 规则映射

| 规则 / 风险点 | 负责阶段 | 工具或模板 | 主要输出 |
|---------------|----------|------------|----------|
| 根结构：`docs/`、`repo/`、`original_sessions/`、`metadata.json` | A | `run_acceptance.py`、`run_validate.py` | `validation_report.md`、`acceptance.json` |
| 必需文档和测试目录 | A | `check_required_artifacts.py` | `required_artifacts.json` |
| README 与实际结构/命令静态一致性 | A/E | `check_readme_alignment.py`、`static_acceptance_audit.md` | finding |
| 脏依赖、缓存、数据库文件 | A | `check_local_dependency.py` | finding |
| 英文题交付语言 | A/E | `check_english_only.py`、静态审计模板 | finding |
| Docker 能否启动并暴露服务 | B | `docker compose` + probes | `port_map.json`、logs、截图 |
| `run_tests.*` 是否可运行 | C | 容器内执行统一测试入口 | test logs、截图 |
| API 测试是否真调实际端点、覆盖率是否超过 90% | D | `tests_coverage_report.md` | `tests_coverage_report.md` |
| 纯前端是否正确豁免 API 测试要求 | D | `tests_coverage_report.md` | `tests_coverage_report.md` |
| Prompt 到代码的业务一致性 | E | `static_acceptance_audit.md` | `static_acceptance_audit_report.md` |
| 安全边界、鉴权、授权、数据隔离 | E | `static_acceptance_audit.md` | Security Review Summary |
| mock/stub/fake 风险 | E | `static_acceptance_audit.md`、`check_fake_impl.py` | finding |
| 最终人工可读修复建议和 short_comment | F | `annotator_fix.md` | repair report、`short_comment.txt` |

### 5.4 阶段详细定义

#### 阶段 A — 结构与规则检查

| 项 | 内容 |
|----|------|
| 输入 | `<task-path>/` |
| 依赖 | 无 |
| 动作 | 运行 `run_acceptance.py`、`run_validate.py`、`check_required_artifacts.py`、`check_readme_alignment.py`、`check_local_dependency.py`，英文题追加 `check_english_only.py` |
| 输出 | `acceptance.json`、`validation_report.md`、`required_artifacts.json`、`readme_alignment.json`、`local_dependency.json` |
| 超时 | 60s |
| 失败影响 | A 失败时 B/C 标记 `blocked`；D/E 仍可静态审查已存在文件；F 必跑 |

#### 阶段 B — Docker 运行证据采集

| 项 | 内容 |
|----|------|
| 输入 | `repo/docker-compose.yml` 或 README 声明的容器启动方式 |
| 依赖 | A 未出现结构阻断 |
| 动作 | 启动服务、读取实际端口映射、执行基础 probe、采集 docker logs 首屏和健康检查输出 |
| 输出 | `port_map.json`、`5_Docker启动截图.png`、`logs/B_docker.log` |
| 超时 | 120s |
| 失败影响 | C 标记 `blocked`；D/E/F 继续 |

B 阶段不得把 README 当作唯一事实来源。README 是被审查对象，只能作为候选启动说明。实际端口与服务状态必须来自 Docker/Compose inspection 或 probe 输出。

#### 阶段 C — `run_tests.*` 运行证据采集

| 项 | 内容 |
|----|------|
| 输入 | `repo/run_tests.sh`、`repo/run_tests.ps1` 或 `repo/run_tests.py` |
| 依赖 | B 成功提供可执行服务或测试容器 |
| 动作 | 在容器或 Compose 网络内执行统一测试入口 |
| 输出 | `6_run_tests.sh运行截图.png`、`logs/C_tests.log`、`test_runtime_summary.json` |
| 超时 | 300s |
| 失败影响 | 不阻塞 D/E/F；F 必须把失败作为运行证据缺口 |

C 阶段只说明测试命令的实际执行结果，不替代 D 阶段对测试有效性的静态审查。

#### 阶段 D — 测试有效性静态审查

| 项 | 内容 |
|----|------|
| 输入 | Prompt、`docs/`、`repo/`、测试文件、A/B/C artifact |
| 依赖 | 项目文件可读 |
| 分析方式 | 纯静态，不启动服务，不运行 Docker，不运行测试 |
| 模板 | `prompt_profiles/tests_coverage_report.md` |
| Codex 动作 | `codex exec <rendered static prompt>`，其中 prompt 内容由内置 `prompt_profiles/tests_coverage_report.md` 渲染 |
| 模板输出 | `./.tmp/tests_coverage_report.md` |
| CLI 收集输出 | `tests_coverage_report.md`、兼容文件 `4_测试有效性报告_api端点真实性.md` |
| 超时 | 300s |

D 阶段必须按项目类型处理：

- 后端/API/全栈项目：检查 API 测试是否真正调用实际项目端点，列出未测或弱测端点，评估端点覆盖率是否超过 90%
- 纯前端且无真实 API 项目：豁免 API 测试存在性和 API 端点覆盖率，不得仅因缺 API 测试判缺陷；改查 UI 行为、路由、状态变化、localStorage/sessionStorage、表单校验和关键用户流

#### 阶段 E — 质检员静态审计报告

| 项 | 内容 |
|----|------|
| 输入 | Prompt、完整交付包、A/B/C/D artifact |
| 依赖 | 项目文件可读 |
| 分析方式 | 纯静态，不启动服务，不运行 Docker，不运行测试，不修改代码 |
| 模板 | `prompt_profiles/static_acceptance_audit.md` |
| Codex 动作 | `codex exec <rendered static prompt>`，其中 prompt 内容由内置 `prompt_profiles/static_acceptance_audit.md` 渲染 |
| 模板输出 | `./.tmp/static_acceptance_audit_report.md` |
| CLI 收集输出 | `static_acceptance_audit_report.md`、兼容文件 `1_质检AI测试报告.md` |
| 超时 | 600s |

E 阶段报告必须按模板要求组织：

1. Verdict
2. Scope and Static Verification Boundary
3. Repository / Requirement Mapping Summary
4. Section-by-section Review
5. Issues / Suggestions
6. Security Review Summary
7. Tests and Logging Review
8. Test Coverage Assessment
9. Final Notes

所有强结论必须有 `file:line` 证据。任何运行时相关结论必须标注 `Cannot Confirm Statistically` 或 `Manual Verification Required`，除非只是引用 B/C 已采集 artifact。

#### 阶段 F — 汇总报告 + `short_comment.txt`

| 项 | 内容 |
|----|------|
| 输入 | A/B/C/D/E 全部输出 |
| 依赖 | 无，永远执行 |
| 动作 | 汇总阶段状态、提取 Blocker/High findings、生成修复建议、预填 `short_comment.txt` |
| 输出 | `3_标注员AI报告问题的修复报告.md`、`repair_summary.json`、`short_comment.txt` |
| 超时 | 60s |

F 阶段不得改写 E/D 的审查结论，只能聚合、去重、压缩和标注来源。

`short_comment.txt` 的三项 AI 预填由 Go 侧模板拼接生成，不额外调用 Codex：

| 字段 | 拼接来源 |
|------|----------|
| 可运行性结论 | B/C 状态 + 日志摘要；若 B/C blocked/skipped 则填缺失原因 |
| 验收匹配度结论 | E 的 Verdict 章节摘要 + Blocker/High finding 计数 |
| 最高风险问题 | 跨阶段 findings 按 severity 排序，取最高一条的 title + impact |

拼接逻辑在 `internal/pipeline/stage_f.go` 中实现。

### 5.5 Codex 静态审查沙箱

Codex 阶段处理的是不可信交付包内容，沙箱必须满足：

- 项目目录只读挂载
- 不挂载 Docker socket
- 不挂载宿主机凭证目录、浏览器 cookie、SSH key、token 文件
- `HOME` 指向临时目录
- MVP 通过 stdout 收集 D/E 报告，默认 `writable_tmp: false` 并使用 Codex `read-only` sandbox；如后续改为模板直接写 `.tmp/`，必须先实现不扩大项目写权限的 artifact-only 写入策略
- 默认禁用网络；如确需网络，必须在配置中显式开启并记录到 `run_manifest.json`
- D/E 复用同一沙箱容器顺序执行（先 D 后 E），两阶段之间清理 workdir/cache 并重置 HOME 指向的临时目录。复用原因记录到 `run_manifest.json`
- 限制 stdout/stderr 最大字节数，超限时截断并保留尾部

### 5.6 Findings ID 生成策略

D/E 阶段的 Codex 输出中提取 findings 后，由 Go 侧统一分配 ID，格式：

```text
P2R-{stage}-{severity_short}-{seq:03d}
```

示例：`P2R-E-BLK-001`、`P2R-D-HIGH-002`。

- `severity_short`：`BLK`（Blocker）、`HIGH`（High）、`MED`（Medium）、`LOW`（Low）
- `seq`：同一 run 内该阶段同 severity 的自增序号
- ID 在写入 `stage_status.json` 和 `findings` 表前由 Go 侧统一生成，不依赖 Codex 输出格式

### 5.7 固定交付文件（每次 run 必产或必说明）

| 文件 | 来源阶段 | 说明 |
|------|----------|------|
| `run_manifest.json` | run 初始化 | 记录配置、工具版本、模板版本、启动参数 |
| `stage_status.json` | 全阶段 | 阶段状态权威记录 |
| `acceptance.json` | A | 基础规则脚本结果 |
| `validation_report.md` | A | 结构校验报告 |
| `port_map.json` | B | B skipped/blocked 时仍生成空结构并说明原因 |
| `5_Docker启动截图.png` | B | B 成功时生成；失败时记录缺失原因 |
| `6_run_tests.sh运行截图.png` | C | C 成功执行时生成；blocked/skipped 时记录原因 |
| `tests_coverage_report.md` | D | 测试有效性静态报告 |
| `4_测试有效性报告_api端点真实性.md` | D | 兼容旧命名，内容来自 D |
| `static_acceptance_audit_report.md` | E | 质检员静态审计报告 |
| `1_质检AI测试报告.md` | E | 兼容旧命名，内容来自 E |
| `3_标注员AI报告问题的修复报告.md` | F | 修复建议汇总 |
| `repair_summary.json` | F | 机器可读修复摘要 |
| `short_comment.txt` | F | 人工勾选入口 |

### 5.8 `short_comment.txt` 格式

```text
1.<可运行性结论: [AI预填，注明来自 B/C 或缺失原因]>
2.<验收匹配度结论: [AI预填，来自 E 的 Verdict 与 Blocker/High 摘要]>
3.<最高风险问题: [AI预填，引用 finding id 或报告章节]>
<[ ] PASS  [ ] REWORK  [ ] FAIL>
```

AI 只预填前三项文字。复选框必须由质检员人工勾选。

### 5.9 每次运行目录结构

```text
TASK-xxxxxx/qa/runs/<run_id>/
├── run_manifest.json
├── stage_status.json
├── port_map.json
├── short_comment.txt
├── repair_summary.json
├── logs/
│   ├── A_validate.log
│   ├── B_docker.log
│   ├── C_tests.log
│   ├── D_tests_coverage_static.log
│   ├── E_static_audit.log
│   └── F_repair.log
├── acceptance.json
├── validation_report.md
├── 5_Docker启动截图.png
├── 6_run_tests.sh运行截图.png
├── tests_coverage_report.md
├── 4_测试有效性报告_api端点真实性.md
├── static_acceptance_audit_report.md
├── 1_质检AI测试报告.md
└── 3_标注员AI报告问题的修复报告.md
```

---

## 6. TUI 设计

### 6.1 MVP 信息架构

MVP 只提供两个可用面板：

```text
App
├── TabBar tabs: [项目总览] [执行]
│
├── OverviewPanel
│   ├── SearchBox
│   ├── FilterChips
│   ├── ProjectTable
│   │   列: task_id | batch | last_run | run_status | failed_stage | blocking | high | manual_verdict
│   └── FooterHint
│
└── ExecutionPanel
    ├── HeaderLine
    ├── StageProgressBars
    ├── FindingsSummary
    ├── LogViewport
    └── RunControl
```

Report/Prompt 面板后续版本再加入 TabBar，MVP 不展示不可用入口。

### 6.2 导航

| 操作 | 键 |
|------|-----|
| 切换面板 | `Tab` / `Shift+Tab` |
| 面板内移动 | `↑` `↓` `←` `→` |
| 执行选中项目 | `Enter` |
| 重跑当前阶段 | `r` |
| 打开当前 run 目录 | `o` |
| 打开 `short_comment.txt` 路径提示 | `s` |
| 返回 | `Esc` |
| 退出 TUI | `q` / `Ctrl+C` |
| 过滤 | SearchBox 直接输入 |

### 6.3 总览面板

```text
┌────────────────────────────────────────────────────────────────────────────┐
│ [项目总览]  [执行]                                                        │
├────────────────────────────────────────────────────────────────────────────┤
│ Search: [________________]  [批次▼]  [只看 blocking]                       │
│                                                                            │
│ ┌──────────────┬────────────┬────────────┬──────────────┬─────┬─────┬─────┐ │
│ │ task_id      │ batch      │ last_run   │ run_status   │ bad │ blk │ hi  │ │
│ ├──────────────┼────────────┼────────────┼──────────────┼─────┼─────┼─────┤ │
│ │ TASK-20260.. │ 2026-04..  │ 04-28 10:05│ with_findings│ B   │  2  │  5  │ │
│ │ TASK-20260.. │ 2026-04..  │ 04-15 09:17│ clean        │ -   │  0  │  0  │ │
│ └──────────────┴────────────┴────────────┴──────────────┴─────┴─────┴─────┘ │
│                                                                            │
│ Enter:执行/查看  r:重跑  o:打开run目录  Tab:切面板  q:退出                  │
└────────────────────────────────────────────────────────────────────────────┘
```

### 6.4 执行面板

```text
┌────────────────────────────────────────────────────────────────────────────┐
│ Task: TASK-20260408-A1B2C3  Run: run-20260428-001  completed_with_findings │
├────────────────────────────────────────────────────────────────────────────┤
│ [A] 结构与规则检查        ████████████████  done ✓     2.3s               │
│ [B] Docker运行证据        ███████░░░░░░░░░  failed ✗   30.0s              │
│ [C] run_tests运行证据     ░░░░░░░░░░░░░░░░  blocked !  by B                │
│ [D] 测试有效性静态审查    ████████████████  done ✓     18.4s              │
│ [E] 质检员静态审计报告    ████████████████  done ✓     25.2s              │
│ [F] 汇总报告              ████████████████  done ✓     1.8s               │
│                                                                            │
│ Findings: Blocker 2 | High 5 | Medium 4                                    │
│                                                                            │
│ ┌────────────────────────────────────────────────────────────────────────┐ │
│ │ [B] docker compose up timed out after 120s                              │ │
│ │ [C] blocked because B failed                                            │ │
│ │ [E] Blocker: README command and actual entrypoint mismatch              │ │
│ └────────────────────────────────────────────────────────────────────────┘ │
│                                                                            │
│ ←/→:切阶段  Enter:展开日志  r:重跑阶段  o:打开run目录  q:返回               │
└────────────────────────────────────────────────────────────────────────────┘
```

**重跑依赖链规则：** `r` 重跑某阶段时，若下游阶段依赖该阶段，则自动触发依赖链重跑。具体规则：

- 重跑 A → 自动重跑 B（若 A 失败导致 B blocked）→ 自动重跑 C（若 B 失败导致 C blocked）
- 重跑 B → 自动重跑 C（若 B 失败导致 C blocked）
- 重跑 C/D/E/F → 无下游依赖，仅重跑自身
- D/E 之间无依赖关系，重跑 D 不影响 E
- 重跑前弹出确认提示，列出将受影响的所有阶段

### 6.5 颜色语义

| 状态 | 颜色 | 图标 |
|------|------|------|
| `pending` | 灰色 | `·` |
| `running` | 黄色 + spinner | `⠋` |
| `done` | 绿色 | `✓` |
| `failed` | 红色 | `✗` |
| `blocked` | 红色/黄色 | `!` |
| `skipped` | 灰色 | `-` |

---

## 7. 数据库设计

### 7.1 Schema

```sql
CREATE TABLE projects (
  task_id       TEXT PRIMARY KEY,
  batch         TEXT NOT NULL,
  path          TEXT NOT NULL,
  run_count     INTEGER DEFAULT 0,
  last_run_id   TEXT,
  last_run_at   TEXT,
  created_at    TEXT DEFAULT (datetime('now'))
);

CREATE TABLE runs (
  run_id          TEXT PRIMARY KEY,
  task_id         TEXT NOT NULL REFERENCES projects(task_id),
  started_at      TEXT,
  finished_at     TEXT,
  status          TEXT DEFAULT 'running',
  manual_verdict  TEXT DEFAULT 'unset',
  static_only     INTEGER DEFAULT 0,
  duration_ms     INTEGER DEFAULT 0,
  artifact_root   TEXT NOT NULL,
  tool_versions   TEXT,
  prompt_versions TEXT
);

CREATE TABLE run_stages (
  run_id        TEXT NOT NULL REFERENCES runs(run_id),
  stage         TEXT NOT NULL,
  status        TEXT NOT NULL,
  started_at    TEXT,
  finished_at   TEXT,
  duration_ms   INTEGER DEFAULT 0,
  blocked_by    TEXT,
  log_path      TEXT,
  artifact_json TEXT,
  error_summary TEXT,
  PRIMARY KEY (run_id, stage)
);

CREATE TABLE findings (
  id             TEXT PRIMARY KEY,
  run_id         TEXT NOT NULL REFERENCES runs(run_id),
  stage          TEXT,
  severity       TEXT NOT NULL,
  title          TEXT NOT NULL,
  rule           TEXT,
  evidence       TEXT,
  impact         TEXT,
  minimum_fix    TEXT,
  source_path    TEXT
);
```

### 7.2 设计约束

- 数据库保存索引和状态投影，artifact 文件仍是完整证据源
- `runs.status` 表示流水线执行状态，不表示人工质检结论
- `manual_verdict` 只由质检员设置，CLI 不自动写 `pass/rework/fail`
- `findings` 保存 D/E/F 提取出的关键问题，便于 TUI 筛选和排序
- `tool_versions` 与 `prompt_versions` 记录脚本和模板 hash，保证报告可追溯

---

## 8. Docker 管理

### 8.1 命名与标记

```text
compose project: p2rqa_<task_id>_<run_id>
容器名:          p2rqa_<task_id>_<run_id>_<service>
标签:
  managed_by=p2rqa
  task_id=<task_id>
  run_id=<run_id>
```

### 8.2 端口策略

不使用 `net.Listen(":0")` 先占后放的端口探测方案，避免 TOCTOU 竞态。

MVP 策略：

1. 优先使用 Compose 随机 host port 发布。
2. 启动后通过 `docker compose ps --format json` 或 `docker port` 读取真实映射。
3. 将真实映射写入 `port_map.json`。
4. README 中声明的端口只用于 README 对齐检查，不作为服务已启动的唯一判断依据。

`port_map.json` 示例：

```json
{
  "run_id": "run-20260428-001",
  "compose_project": "p2rqa_TASK-20260408-A1B2C3_run-20260428-001",
  "mappings": {
    "web": {"container": 3000, "host": 34152},
    "api": {"container": 8080, "host": 34153}
  }
}
```

### 8.3 健康检查策略

B 阶段拆分两类检查：

- service probe：服务是否有监听、容器是否健康、基础 URL 是否响应
- README alignment：README 中的端口、URL、账号、启动命令是否与静态结构和运行证据一致

service probe 不得只依赖 README。README alignment 不得把 README 当事实来源。

### 8.4 清理策略

| 时机 | 动作 |
|------|------|
| Run 开始时 | 清理同 task_id + managed_by=p2rqa 且 exited 的容器 |
| Run 结束时 | 默认保留失败容器短时间用于排查，成功容器自动停止 |
| 每日 GC | `docker container prune --filter label=managed_by=p2rqa` |
| 每周 GC | 清理 7 天前的 p2rqa 镜像和网络 |

---

## 9. 配置系统

### 9.1 配置文件 `.p2r.yaml`

```yaml
scan_path: "./projects-qa"
db_path: "./projects-qa/.qa-control/index.db"

pipeline:
  static_only: false
  stage_timeouts:
    A: 60
    B: 120
    C: 300
    D: 300
    E: 600
    F: 60

docker:
  managed_label: "managed_by=p2rqa"
  compose_project_prefix: "p2rqa"
  keep_failed_containers_minutes: 60
  health_check_timeout_seconds: 60
  gc:
    daily_prune_exited: true
    weekly_image_retention_hours: 168

codex:
  sandbox_image: "codex:latest"
  prompt_profiles_dir: "./projects-qa/.qa-control/prompt_profiles"
  network: "none"
  max_output_bytes: 1048576
  writable_tmp: false

tui:
  refresh_interval_ms: 100
  log_max_lines: 10000
```

### 9.2 优先级

命令行 flag > `.p2r.yaml` > 内建默认值

---

## 10. 项目识别规则

目录内同时存在以下四项即视为 p2r 交付包：

```text
docs/
repo/
original_sessions/
metadata.json
```

扫描规则：

- 递归扫描 `scan_path` 下所有子目录
- 识别后记录绝对路径和批次
- 若 `metadata.json.prompt` 不存在或不可读，项目仍可索引，但 A 阶段必须产生 blocking finding

---

## 11. 分发

### 11.1 Go Module

```text
module github.com/<org>/p2r
```

### 11.2 Goreleaser 目标

| OS | Arch |
|----|------|
| linux | amd64, arm64 |
| darwin | amd64, arm64 |
| windows | amd64 |

### 11.3 嵌入资源

辅助脚本和默认 Prompt 模板使用 `embed.FS` 随二进制分发：

```go
//go:embed assets/*
var embeddedAssets embed.FS
```

首次运行 `p2r scan` 或 `p2r run` 时释放到：

```text
projects-qa/.qa-control/scripts/
projects-qa/.qa-control/prompt_profiles/
```

释放时记录模板 hash 到 `run_manifest.json`。

---

## 12. MVP 交付清单

- [x] 技术选型
- [x] 项目布局
- [x] `p2r scan` — 项目发现 + 索引
- [x] `p2r run` — 6 阶段流水线，含 `--stage`、`--from`、`--static-only`
- [x] `p2r status` — 运行历史、阶段状态、finding 摘要
- [x] `p2r tui` — OverviewPanel + ExecutionPanel
- [x] SQLite 数据存储：projects/runs/run_stages/findings
- [x] Docker 命名、标签、随机端口映射读取、GC
- [x] Codex 静态审查沙箱
- [x] 内置静态审计模板和测试覆盖有效性模板
- [x] `.p2r.yaml` 配置系统
- [x] `goreleaser` 单二进制分发
- [ ] 后继版本：ReportPanel + PromptPanel
- [ ] 后继版本：批量并行执行

---

## 附录 A: 与现有 p2r-qa skill 的关系

`p2r` CLI 不替代现有 `p2r-qa` skill，而是作为其编排层：

- `p2r-qa` skill 提供 QA 规则、脚本和参考文档
- `p2r` CLI 负责发现项目、执行阶段、采集证据、落盘报告、索引 findings
- 质检员静态审计报告由既有模板生成，不由 CLI 临时拼接
- 质检员最终判定仍在 CLI 之外完成，CLI 只预填 `short_comment.txt`

## 附录 B: p2r CLI 命令速查

```text
# 扫描项目
p2r scan --path ./projects-qa

# 对单个项目运行完整流水线
p2r run TASK-20260408-A1B2C3

# 只生成静态审计相关证据
p2r run TASK-20260408-A1B2C3 --static-only

# 只跑测试有效性静态审查
p2r run TASK-20260408-A1B2C3 --stage D

# 从 run_tests 运行证据阶段续跑到汇总
p2r run TASK-20260408-A1B2C3 --from C

# 查看运行历史
p2r status TASK-20260408-A1B2C3

# 查看指定 run
p2r status TASK-20260408-A1B2C3 --run run-20260428-001

# 启动 TUI
p2r tui
p2r tui --path ./projects-qa
```
