# p2r QA CLI MVP 实施计划

## Summary
- 状态：DONE_WITH_CONCERNS。已读取 `p2r_cli_设计.md`，Ralph 当前处于 planning；`gstack` skill 预检已运行，`browse/dist/browse` 未构建。
- 本计划按绿地实现处理：当前仓库只有设计文档，目标是交付 Go 单二进制 `p2r`，覆盖 `scan/run/status/tui` MVP。
- 实施必须先补齐 Ralph planning gate：创建 `.omx/plans/prd-p2r-cli.md` 和 `.omx/plans/test-spec-p2r-cli.md`，再进入代码实现循环。

## Public Interfaces
- Go module 默认使用远端路径 `github.com/xuanli520/p2r_tui`；CLI binary 名称固定为 `p2r`。
- CLI 契约：`p2r scan [--path]`、`p2r run <task-id> [--stage A..F] [--from A..F] [--static-only]`、`p2r status <task-id> [--run]`、`p2r tui [--path]`。
- 配置契约：`.p2r.yaml`，优先级为 command flag > config file > built-in defaults。
- 数据契约：实现 `projects`、`runs`、`run_stages`、`findings` SQLite schema；artifact 文件仍为完整证据源。
- Artifact 契约：每次 run 写入 `run_manifest.json`、`stage_status.json`、`port_map.json`、`repair_summary.json`、`short_comment.txt` 和 A-F 阶段输出。
- 状态枚举固定：stage status 为 `pending/running/done/failed/blocked/skipped`；run status 为 `running/completed_clean/completed_with_findings/aborted/crashed`；manual verdict 只允许 `unset/pass/rework/fail`，CLI 不自动设置最终判定。

## Ralph Execution Plan
- Iteration 0：生成 Ralph context snapshot、PRD、test spec，记录约束：不替代人工 verdict，不引入 Docker SDK，不把 README 当运行事实，不让 D/E 启动项目或改代码。
- Iteration 1：由 executor 完成 Go scaffold、Cobra 命令、配置加载、embed assets、SQLite store、scanner。
- Iteration 2：实现 pipeline engine、阶段选择逻辑、状态机、artifact writer、finding ID 分配、run manifest 和 status projection。
- Iteration 3：实现 A/B/C：脚本执行封装、Docker compose CLI 管理、真实端口读取、probe、run_tests 执行、失败/blocked/skipped 传播。
- Iteration 4：实现 D/E/F：Codex 静态沙箱命令封装、模板输出收集、findings 提取、repair summary、`short_comment.txt` Go 模板拼接。
- Iteration 5：实现 Bubble Tea TUI：OverviewPanel、ExecutionPanel、搜索/过滤、阶段栏、日志 viewport、重跑确认和依赖链规则。
- Final Ralph gates：`go test ./...`、`go vet ./...`、`go build ./...`、关键 CLI fixture smoke test、architect verification、ai-slop-cleaner changed-files pass、回归重跑、最后清理 Ralph state。

## Implementation Details
- Assets：复用本机 `prompt2repo-qa-en` skill 中已有脚本作为 A 阶段种子；缺失的 `run_validate.py`、prompt profiles、`annotator_fix.md` 按设计文档补齐到 `assets/`，首次 `scan/run` 释放到 `projects-qa/.qa-control/` 并记录 hash。
- Scanner：递归扫描 `scan_path`，目录同时含 `docs/`、`repo/`、`original_sessions/`、`metadata.json` 即索引；`metadata.json.prompt` 缺失不阻止索引，但 A 阶段产生 blocking finding。
- Pipeline：A 是 hard gate 只阻塞 B/C；B 失败阻塞 C；D/E 只要求文件可读；F 永远执行并只聚合，不改写 D/E 结论。
- Docker：只 shell 调用 `docker`/`docker compose`；compose project 和容器 label 使用 `p2rqa_<task_id>_<run_id>`；端口来自 compose inspection 或 `docker port`，不使用先占后放端口探测。
- Codex sandbox：项目只读、无 Docker socket、无宿主凭证、临时 HOME、默认无网络，只允许 `.tmp/` 与当前 run artifact 写入；D 后清理 workdir/cache 再执行 E。
- TUI：MVP 只显示 `[项目总览] [执行]`，不渲染 Report/Prompt 死入口；`r` 重跑前列出受影响阶段并要求确认。
- gstack：不作为 `p2r` 运行时依赖；用于 Ralph 验证阶段对样例 web 交付包的 B 阶段端口做浏览器外部证明。当前 browse binary 缺失，执行时先跑 gstack setup gate，再用 browse 打开 `port_map.json` 中 host port、采集 snapshot/console/network/screenshot。

## Test Plan
- Unit：配置优先级、scanner 识别、SQLite CRUD、stage 状态转换、`--stage/--from/--static-only` 选择、finding ID、short_comment 拼接。
- Integration：用 temp p2r package 跑 `scan/status/run`；用 fake docker/codex/executor 验证 A fail、B fail、C blocked、D/E static-only、F always-run。
- Artifact golden：校验 `stage_status.json`、`run_manifest.json`、`repair_summary.json`、兼容中文文件名输出和 skipped/blocked 原因。
- TUI model tests：键盘导航、搜索过滤、阶段选择、日志 viewport、重跑依赖链确认。
- Optional real-runtime：有 Docker 时跑一个最小 sample compose；有 gstack browse 时访问映射端口并保存外部截图证据。
- Release checks：`go test ./...`、`go vet ./...`、`go build ./...`、`goreleaser check`（若本机安装）。

## Assumptions
- 不新增设计文档外的核心依赖；Cobra、Bubble Tea、Lip Gloss、Bubbles、go-sqlite3、goreleaser 视为已由设计批准。
- `p2r` 不自动勾选 `PASS/REWORK/FAIL`，只预填前三条 `short_comment.txt` 文案。
- ReportPanel、PromptPanel、批量并行执行不进入 MVP。
- 若真实 Docker、Codex 或 gstack 环境不可用，测试用 fake executor 保证逻辑覆盖，真实集成项在最终报告中标为 Not-tested。
