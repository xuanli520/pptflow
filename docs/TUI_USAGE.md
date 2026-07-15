# Harbor Flow V2 Task Hub 使用指南

TUI 是 V2 生命周期控制面的视图。它不接收 workspace 路径，不启动旧 Runner，不克隆 workspace，不直接编辑 live task 文件，也不直接删除 workspace。

使用受管控制面根目录启动：

```text
harbor-factory --root .harbor-factory tui
```

根命令不再提供 `--workspace`、`--workspace-root`、`--rescan`、`--task-concurrency` 或 `--auto-approve` 等旧 TUI 参数。持久化生命周期操作使用 `task`、`revision`、`run`、`review`、`release`、`budget` 与 `workspace` 命令组。

## Task Hub

首屏是 Task Hub。它通过 application service 投影不可变的 Task、TaskRevision、WorkflowRun、持久化队列、Review、Repair 与本地 package 事实。workspace 只是可清理的受管执行环境，不是 TUI 选择的生命周期身份。

| 键位 | 作用 |
| --- | --- |
| `Tab` / `Right` | 在 `Tasks`、`Runs` 和 `Queue` 标签间前进。 |
| `Shift+Tab` / `Left` | 切换到上一个标签。 |
| `Up` / `Down`、`j` / `k` | 在当前标签内移动 Task 或 Run 选择。 |
| `/` | 过滤 Task Hub 投影；`Enter` 应用，`Esc` 取消。 |
| `Enter` | 打开只读详情；计划预览存在时进入原生确认表单。 |
| `Esc` | 取消待输入的前缀、确认表单或当前计划预览；从不提交 mutation。 |
| `q`、`Ctrl+Q`、`Ctrl+C` | 退出；存在 active durable run 时打开逐 Run 的受控 worker 交接面板，不是取消。 |

Queue 标签显示观测到的运行中和排队数量。后端未暴露容量池时，容量显示为 `未配置`；`0` 不会被解释为已配置的零容量池。

退出交接面板会枚举所有 active Run，并默认勾选每个当前可交接的 Run。可用 `Up`/`Down` 选择并用 `Space` 单独取消勾选；没有选中任何 Run 时，`Enter` 会直接退出，不启动任何 worker，也不会出现第二次确认。每个已选 Run 都保留自己的 UUIDv7 操作 ID、幂等键和已观察的 Run checkpoint，由 application service 按项交给受控 child worker。`Esc`、`q` 或 `Ctrl+C` 在面板内返回 Task Hub；它们不会取消 TUI 根 context、任何其他 Run 或 durable job。

## 生命周期序列

所有生命周期操作均使用两段式命名空间。第一个键只进入命名空间，在 1.2 秒后或按 `Esc` 失效，绝不改变状态。footer 会显示允许的第二键和服务端给出的禁用原因。

| 序列 | 请求的计划 |
| --- | --- |
| `t n`、`t i` | 新建空 draft Task 或导入完整本地 Task 快照。 |
| `t s` | 启动受控 Standard 创题：捕获部署冻结的 Tower HTTP 源码，创建 revision-free draft Task、AuthoringSession 并排队 Standard Run。来源、提交、Codex/profile、模型和 catalog/lock 不从 TUI 输入。 |
| `t e`、`t f`、`t a`、`t d`、`t u` | 创建编辑 candidate、Fork、归档、软删除或恢复选中的 Task。 |
| `x c`、`x n`、`x a` | 继续处理、启动 Run 或 Attach 到 durable Run。 |
| `x k` | 打开选中 Run 的 Run Control。 |
| `v a`、`v c`、`v r` | 批准、要求修改或终止性拒绝选中的 Review。 |
| `p p`、`p w` | 创建受管本地 package 或撤回 release。 |

V2 TUI 向 application layer 请求计划预览，不会在本地推断证据复用、失效范围、TaskRevision 变化、quota 或外部副作用。确认表单必须填写原因，操作员从本机 OS 账户派生，并在表单打开时生成 UUIDv7 幂等键；失败重试保留同一键。TUI 只通过 application service 提交已确认动作，不直接访问 SQLite、受管 snapshot、worker 或 provider。没有已确认服务契约的动作会保持禁用并显示原因。

## Run Control

`Ctrl+X` 或 `x k` 为选中的 Run 打开 Run Control。初始选择为 `返回并保持运行`，因此 `Esc` 与默认路径没有副作用。`P`、`K`、`S` 分别选择暂停、取消选中 Stage 和终止 Run；第一次 `Enter` 请求影响预览，第二次 `Enter` 打开确认表单。仅当 application service 声明完整的控制提交契约时，确认后才创建 `ControlOperation`。

overlay 会展示选中 run/stage、最近 checkpoint、durable control 状态、runtime receipt 数量、quota settlement 引用及任何不确定的外部结果。生命周期服务禁用的控制动作会保持禁用并显示原因。

## 鼠标与可访问性

V2 Task Hub 的选择和生命周期命名空间以键盘为权威交互路径。指针输入绝不会从邻近文本推断或执行生命周期 mutation；它只作用于明确渲染且已启用的控件。当投影行或操作没有显式鼠标目标时，请使用上述键盘序列。

## 旧路径切换

以下 V1 路径已明确不可用：manual retry、workspace clone rerun、repair overlay、直接删除 workspace、直接编辑 workspace、`run retry-stage`、`run rerun` 与 `repair start`。历史 workspace UI 材料只作为归档文档保留，不能作为操作指南。
