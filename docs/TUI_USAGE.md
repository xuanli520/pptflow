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
| `d` | 打开任务详情，分区查看来源、当前 Run、失败原因、运行记录、日志路径和审核状态。 |
| `l` | 在任务详情中读取当前 Run 的受控本地日志；日志页支持滚动和刷新。 |
| `t` | 对可续跑的 Run 打开恢复/重试确认；创题 Run 使用专属恢复契约。 |
| `x` | 对当前可取消的 Run 打开取消确认。 |
| `n` | 创建 Standard 创题任务；仅在当前部署已配置该能力时显示。 |
| `a` / `r` | 在详情中打开批准或要求修改的审核原因确认输入。 |
| `Esc` | 关闭当前输入或详情，不提交 mutation。 |
| `q` / `Ctrl+C` | 从详情返回；在看板中先补发 queued Run 激活，再退出。 |

打开看板和每次轮询都会尝试交接尚未启动的 queued Run 给受控 child worker。没有配置本地 launcher 时，这一步是安全的 no-op；不会改写已持久化的 Run。

## 创建任务

按 `n` 后填写 HTTPS/SSH Git URL、完整 40 或 64 位 commit SHA、digest 固定的基础镜像、slug、标题、task type、application、单行 objective 和原因。TUI 将这些事实交给 Standard authoring application service；它负责冻结源码和 authoring brief，创建 draft Task、AuthoringSession 和 queued Run。

表单可见时，所有键都只属于表单。输入 `d`、`a`、`r`、方向键或 `Tab` 不会在后台打开详情、移动选择或提交审核。

## 详情、日志与运行操作

详情页将来源、当前运行、失败原因和运行历史分区呈现。当前运行区会显示日志文件路径；按 `l` 后，TUI 只读取该 Run 的 durable worker handoff 已记录的本地日志，并展示最多 64 KiB 的尾部内容。它不会接受或打开任意用户提供的文件路径。

`t` 和 `x` 都要求填写原因。取消按一次 `Enter` 即确认，会写入 durable termination control，不会由 TUI 直接终止进程。任务修订 Run 使用既有 no-content continuation；Standard 创题 Run 在 `failed_recoverable` 或 `paused` 时显示“恢复/重试”：第一次按 `Enter` 会获取并展示不落库的断点恢复计划，列出复用与重新调度阶段、输入校验和计划原因；确认计划后第二次按 `Enter` 才提交恢复。提交会再次绑定 checkpoint 与语义计划指纹，过期预览必须重新核验。恢复通过冻结的 source/session checkpoint 重新排队失败阶段及其下游，不会修改模型、推理强度、源码快照或 Run 定义；模型或推理强度变更不能原地恢复旧的冻结 Run，必须通过新部署新建创题 Task、Session 和 Run，可复用同一 immutable source snapshot，但不得复用旧 artifacts。已物化题目或已交接 Phase-1 的 Run 不会提供该操作。

CLI 可执行同一恢复路径：

```text
harbor-factory --root .harbor-factory authoring recover \
  --run <authoring-run-uuidv7> \
  --idempotency-key <uuidv7> \
  --reason "recover transient provider failure"
```

附加 `--dry-run` 只返回恢复计划，不写入 command、plan、execution 或 worker job。网络响应丢失后，使用相同的幂等键重试；服务会重放原计划或执行回执，而不是重新解释较新的 Run 状态。

## 审核

详情只对恰好一个 open review 显示可执行操作。按 `a` 或 `r` 后必须填写审核原因并按 `Enter`，服务会根据 review 类型走 AuthoringSession 或 TaskRevision 的正确审核契约。

每个创建和审核命令都使用 TUI 生成的 UUIDv7 幂等键。请求已经持久化但 worker 激活暂时失败时，保留原表单或审核原因并重试，会复用同一键和原始 durable 回执，而不是创建第二个 Task 或第二个 decision。多个 open review 会显示为不可直接操作，请使用 CLI 选择明确的审核请求。

TUI 在刷新或 mutation 仍在进行时拒绝退出，避免关闭控制面数据库后仍有在途命令访问它。退出前的 queued Run 激活失败时，`q` 会重试；确认没有在途 mutation 后再次按 `Ctrl+C` 可强制退出。成功 mutation 的 durable 摘要会显示在看板中。

其他生命周期操作，例如导入、归档、恢复、阶段级 run control、package、删除和终止性拒绝，继续通过显式 CLI 命令执行。
