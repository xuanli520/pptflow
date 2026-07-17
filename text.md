# Durable Job Failure Design

将失败诊断、状态转换和恢复授权统一到 durable job，避免错误只存在于 worker 日志。

## 规则

- 任何进入 `failed` 或 `in_doubt` 的 job 必须原子持久化 failure record。
- Run 状态只是 failure record 的投影，不是唯一信息源。
- 已有 failure record 不得被重复执行、lease 丢失或 worker 崩溃覆盖。

## 数据模型

在 `jobs` 增加：

```sql
failure_code         TEXT NOT NULL DEFAULT '',
failure_message      TEXT NOT NULL DEFAULT '',
failure_details_json TEXT NOT NULL DEFAULT '{}'
```

- `failure_code`：稳定机器码，如 `handoff.artifact_lineage_invalid`。
- `failure_message`：面向操作者的脱敏摘要。
- `failure_details_json`：仅保存 ID、阶段、校验项和 expected/actual digest；不得保存密钥、路径或模型输出。

## API

handler 显式返回终态与失败信息：

```go
type DurableJobResult struct {
    State   store.JobState
    Failure *store.DurableJobFailure // 成功时为 nil
}
```

`TransitionDurableJob` 在同一事务中写入 job 状态、failure record 和审计事件。不要由 worker 根据 `(JobState, error)` 猜测失败语义。

## 状态与恢复

- `in_doubt`：外部副作用、进程中断或临时依赖不可用，结果未知；只允许显式 `reconcile` 或 `redrive`。
- `failed`：合同、输入、部署或数据校验已确定失败；禁止普通 `redrive`，需修复或新建运行。

Standard Authoring handoff 规则：

| 场景 | 状态 | 示例 code | 操作 |
| --- | --- | --- | --- |
| 部署定义缺失或无效 | `in_doubt` | `handoff.definition_unavailable` / `handoff.definition_invalid` | 修复后 `redrive` |
| artifact lineage、receipt、snapshot、materialization 校验失败 | `failed` | 对应 `handoff.*` | 修复或新建运行 |
| DB / 对象存储暂时不可读 | `in_doubt` | 明确的 storage code | `reconcile` 或 `redrive` |

Handoff 使用受控 typed errors，例如 `handoff.artifact_lineage_invalid`、`handoff.admission_receipt_missing`、`handoff.snapshot_digest_mismatch`。`failMalformedJob` 仅处理不可解析 payload 或存储完整性错误；其余失败走统一诊断接口。

## 展示与验收

TUI、CLI 和 `run attach` 直接读取 failure record，展示失败阶段、错误码、脱敏摘要、可用恢复操作、job/artifact ID 与审计时间。

- 每个终态 durable job 均可查询稳定错误码和摘要。
- 重试与崩溃不覆盖既有 failure record。
- `redrive` 仅接受明确可恢复的 `in_doubt` handoff。
- 可直接定位具体失败条件，不再只显示“requires reconciliation”。
