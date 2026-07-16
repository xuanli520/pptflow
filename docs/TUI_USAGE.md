# Harbor Flow Task Board 使用指南

TUI 是 V2 生命周期控制面的紧凑看板。它只通过 application service 读取任务和提交已确认的生命周期命令；不会直接访问 SQLite、工作区、快照或 worker。

从受管控制面根目录启动：

```text
harbor-factory --root .harbor-factory tui
```

## 看板

首屏按 `待处理`、`运行中`、`已完成` 三列投影 durable Task、Run 和 Review 状态。`failed_recoverable`、`interrupted` 与 `in_doubt` 的 Run 保留在 `待处理`，不会被伪装成已完成。

| 键位 | 作用 |
| --- | --- |
| `Tab` / `Shift+Tab`、`Left` / `Right` | 切换列。 |
| `Up` / `Down`、`j` / `k` | 选择任务。 |
| `d` | 查看任务的来源、Run、阶段和审核状态。 |
| `n` | 创建 Standard 创题任务；仅在当前部署已配置该能力时显示。 |
| `a` / `r` | 在详情中打开批准或要求修改的审核原因确认输入。 |
| `Esc` | 关闭当前输入或详情，不提交 mutation。 |
| `q` / `Ctrl+C` | 从详情返回；在看板中先补发 queued Run 激活，再退出。 |

打开看板和每次轮询都会尝试交接尚未启动的 queued Run 给受控 child worker。没有配置本地 launcher 时，这一步是安全的 no-op；不会改写已持久化的 Run。

## 创建任务

按 `n` 后填写 HTTPS/SSH Git URL、完整 40 或 64 位 commit SHA、slug、标题和原因。TUI 将这些事实交给 Standard authoring application service；它负责冻结源码、创建 draft Task、AuthoringSession 和 queued Run。

表单可见时，所有键都只属于表单。输入 `d`、`a`、`r`、方向键或 `Tab` 不会在后台打开详情、移动选择或提交审核。

## 审核与重试

详情只对恰好一个 open review 显示可执行操作。按 `a` 或 `r` 后必须填写审核原因并按 `Enter`，服务会根据 review 类型走 AuthoringSession 或 TaskRevision 的正确审核契约。

每个创建和审核命令都使用 TUI 生成的 UUIDv7 幂等键。请求已经持久化但 worker 激活暂时失败时，保留原表单或审核原因并重试，会复用同一键和原始 durable 回执，而不是创建第二个 Task 或第二个 decision。多个 open review 会显示为不可直接操作，请使用 CLI 选择明确的审核请求。

TUI 在刷新或 mutation 仍在进行时拒绝退出，避免关闭控制面数据库后仍有在途命令访问它。退出前的 queued Run 激活失败时，`q` 会重试；确认没有在途 mutation 后再次按 `Ctrl+C` 可强制退出。成功 mutation 的 durable 摘要会显示在看板中。

其他生命周期操作，例如导入、归档、恢复、run control、package、删除和终止性拒绝，继续通过显式 CLI 命令执行。
