# Claude runtime spike report

## 结论

Claude Code 可以作为 p2ro producer runtime，但 MVP 不应依赖 `claude app-server`，也不应直接把 Claude 写权限全局打开。

推荐路径：

1. 首选 `ClaudeSDKAdapter`，以 TypeScript sidecar 形式接入 Go TUI。
2. `ClaudeCLIAdapter` 作为 smoke test 和 fallback。
3. `CodexAppServerAdapter` 只保留为临时降级后端，不能复用 p2r read-only reviewer 配置。

当前验证结果：

| 能力 | Claude SDK | Claude CLI |
| --- | --- | --- |
| 启动会话 | 通过 | 通过 |
| session id | 通过 | 通过 |
| 结构化事件流 | 通过 | 通过 |
| 工具限制 | 通过 | 通过 |
| workspace 写入 | bypass 模式通过，默认受本机策略拦截 | 通过但需要处理工具调用后收尾 |
| 越界写入拦截 | 通过，产生 permission denial | 需要 p2ro policy 自行包一层 |
| raw stdout/stderr 落盘 | 通过 | 通过 |
| 标准化事件样例 | 通过 | 通过 |
| 取消/中断 | streaming input 的 `query.interrupt()` 通过；simple `AbortController` 未通过 | 可用进程级 kill，未验证协议级 cancel |

总体判断：`ClaudeSDKAdapter` 可进入 p2ro MVP 的实现前置路径，但还需要补一个专门的权限配置/取消语义 spike。`ClaudeCLIAdapter` 能作为 fallback，但不适合作为长期首选，因为权限决策、取消、工具调用收尾都比 SDK 粗。

## 环境

```text
date = 2026-06-09
workspace = h:\project\mindflow\p2r_tui
evidence_root = h:\project\mindflow\p2r_tui\.tmp\claude-runtime-spike
global claude = 2.1.143 (Claude Code)
npm @anthropic-ai/claude-code latest = 2.1.169
npm @anthropic-ai/claude-agent-sdk installed = 0.3.169
```

本机 `claude --help` 暴露：

```text
-p / --print
--output-format json|stream-json
--input-format text|stream-json
--allowedTools / --disallowedTools / --tools
--permission-mode
--session-id / --continue / --resume
--mcp-config / --strict-mcp-config
--include-partial-messages
```

本机未发现：

```text
claude app-server --listen stdio://
Codex JSON-RPC initialize/thread/start/turn/start/turn/steer
无需 claude.ai 登录的 Remote Control 本地编排 API
```

## 已执行探针

### CLI JSON 基础输出

命令形态：

```text
claude.exe -p --output-format json --max-budget-usd 0.5
```

结果：

- `cli_json_basic_success.stdout.json`
- exit code: `0`
- 输出 `type=result`、`subtype=success`、`session_id`、`usage`、`total_cost_usd`

低预算探针也验证了预算错误结构：

- `cli_json_basic_stdin.stdout.json`
- `subtype=error_max_budget_usd`
- `errors=["Reached maximum budget ($0.05)"]`

### CLI stream-json 基础输出

命令形态：

```text
claude.exe -p --verbose --output-format stream-json --max-budget-usd 0.5
```

结果：

- `cli_stream_basic_success.stdout.jsonl`
- 事件包含 `system/init`、`assistant`、`result`
- `system/init.cwd` 指向 spike workspace
- `result.subtype=success`
- `session_id=2fd6c40d-fd25-43b2-9e41-fdc56bcdaa8d`

约束：

- 本机 CLI 明确要求 `--output-format stream-json` 必须搭配 `--verbose`。
- PowerShell 外层命令有时超时，但事件文件已经完整写出；Go adapter 应直接管理子进程 stdout/stderr，不依赖 shell 管道收尾。

### CLI 工具限制

命令形态：

```text
claude.exe -p --output-format json --tools Read --max-budget-usd 0.5
```

结果：

- `cli_json_tool_restricted.stdout.json`
- prompt 要求运行 shell，结果返回 `BASH_UNAVAILABLE`
- 说明 `--tools` 能限制可用内置工具

注意：`--allowedTools` 不是工具白名单，只是自动批准列表。限制工具必须用 `--tools` 和 `--disallowedTools`。

### CLI workspace 写入

命令形态：

```text
claude.exe -p --verbose --output-format stream-json --tools Write,Read --permission-mode acceptEdits --max-budget-usd 0.8
```

结果：

- 生成文件：`.tmp\claude-runtime-spike\workspace\p2ro_probe.txt`
- 内容：`p2ro claude runtime write ok`
- `cli_stream_write_workspace.stdout.jsonl` 包含 `Write` tool_use
- `system/init.tools=["Read","Write"]`
- `system/init.permissionMode="acceptEdits"`

风险：

- 该探针没有自然产出最终 `result` 行，外层等待超时后进程已结束。
- CLI adapter 必须有 stage-level timeout、stdout heartbeat、进程树 kill、部分成功状态判断。

### SDK 安装和导出 API

命令形态：

```text
npm install @anthropic-ai/claude-agent-sdk@0.3.169
node --input-type=module -e "import * as sdk from '@anthropic-ai/claude-agent-sdk'; ..."
```

结果：

- 安装成功，`found 0 vulnerabilities`
- `package.json`：`@anthropic-ai/claude-agent-sdk@0.3.169`
- 导出包含 `query`、`tool`、`createSdkMcpServer`、session 管理函数
- 类型声明包含 `cwd`、`tools`、`disallowedTools`、`allowedTools`、`canUseTool`、`permissionMode`、`abortController`、`Query.interrupt()`

### SDK 基础 query

代码形态：

```text
query({
  prompt,
  options: { cwd, tools: [], maxTurns: 1, maxBudgetUsd: 0.5 }
})
```

结果：

- `sdk_query_basic.stdout.jsonl`
- 事件包含 `system/init`、大量 `thinking_tokens`、`assistant`、`result`
- `result.subtype=success`
- `session_id=b8a4dbaf-9215-435d-9c64-98d63231ad5f`
- SDK 使用 bundled Claude Code `2.1.169`

### SDK 默认权限写入

代码形态：

```text
query({
  prompt: "Create a file in current working directory...",
  options: { cwd, tools: ["Write", "Read"], permissionMode: "acceptEdits" }
})
```

结果：

- 未生成 `p2ro_sdk_probe.txt`
- 模型尝试写到 `C:\Users\Administrator\p2ro_sdk_probe.txt`
- SDK 产生 tool_result 错误：未授予该路径写权限
- 证明 SDK 能暴露越界写入风险

### SDK `canUseTool` 受控写入

代码形态：

```text
query({
  options: {
    cwd,
    tools: ["Write", "Read"],
    permissionMode: "default",
    canUseTool: async (...) => allow only workspace path
  }
})
```

结果：

- `sdk_query_write_allowed.permissions.jsonl` 记录两次 `Write` allow 决策
- 但最终 `permission_denials` 仍拒绝写入 workspace
- 未生成 `p2ro_sdk_probe_ok.txt`
- stdout 中出现本机 OMC/Claude 权限限制说明

判断：

- `canUseTool` 能进入权限决策链，但本机还有更高层权限配置拦截写入。
- p2ro 不能只靠 `canUseTool`，还需要显式配置 Claude/OMC 权限边界，或把 SDK sidecar 放进独立可写沙箱。

### SDK bypass 写入

代码形态：

```text
query({
  options: {
    cwd,
    tools: ["Write", "Read"],
    permissionMode: "bypassPermissions",
    allowDangerouslySkipPermissions: true
  }
})
```

结果：

- 生成文件：`.tmp\claude-runtime-spike\workspace\p2ro_sdk_probe_bypass.txt`
- 内容：`p2ro claude sdk bypass write ok`
- `sdk_query_write_bypass.stdout.jsonl` 包含 `Write` tool_use、tool_result、最终 `result`
- `permission_denials=[]`

判断：

- SDK 技术上能完成 workspace 写入。
- `bypassPermissions` 只能在 p2ro 自己提供外层隔离时使用，例如独立临时 workspace、路径防逃逸、网络限制、命令白名单、进程级超时。
- 默认产品策略不应把 bypass 暴露给用户或作为全局配置。

### SDK simple AbortController 取消

代码形态：

```text
const abortController = new AbortController()
setTimeout(() => abortController.abort(), 200)
query({ options: { abortController } })
```

结果：

- `sdk_query_abort.stdout.jsonl`
- exit code: `2`
- 探针自然输出 `completed_without_abort`
- 未验证 simple `query()` 的 `AbortController` 可中止

判断：

- simple `query()` 的 `AbortController` 探针未通过。
- 不能把 `AbortController` 作为 p2ro 的主取消机制。

### SDK streaming input `query.interrupt()` 控制闭环

代码位置：

```text
.tmp\claude-runtime-spike\sdk-ts\streaming_interrupt_probe.mjs
```

代码形态：

```text
query({
  prompt: AsyncIterable<SDKUserMessage>,
  options: { cwd, tools: [], maxBudgetUsd: 0.8, maxTurns: 4 }
})

input.push(long_task)
await query.interrupt()
input.push(followup)
```

结果：

- `sdk_streaming_interrupt_control.jsonl`
- `sdk_streaming_interrupt.stdout.jsonl`
- `sdk_streaming_interrupt_summary.json`
- `session_id=f8504142-3da3-4a2a-a73f-8ba8bf08fbbc`
- 第一次 long task 在 `interrupt()` 后返回 `result.subtype=error_during_execution`
- 第一次 result 的 `terminal_reason=aborted_streaming`
- 同一 session 随后接收 follow-up 输入
- follow-up 返回 `result.subtype=success`
- follow-up result：`{"probe":"streaming_interrupt_followup","ok":true}`

控制事件时序：

```text
03:41:58.114 input.sent long_task
03:41:58.745 session.started f8504142-3da3-4a2a-a73f-8ba8bf08fbbc
03:42:00.626 interrupt.requested
03:42:00.628 interrupt.resolved
03:42:00.630 result error_during_execution
03:42:00.630 input.sent followup
03:42:02.370 result success
03:42:02.375 probe.finished
```

判断：

- streaming input/output 模式下 `query.interrupt()` 可用。
- p2ro 可以在不中止整个 sidecar 进程的情况下打断当前 turn。
- 被 interrupt 的 turn 应映射为 `stage.interrupted` 或 `turn.interrupted`，不应按普通失败处理。
- 同一 session 可继续接收后续 user input，这满足 TUI 的“暂停/打断/继续”核心控制闭环。
- sidecar 仍应保留进程级 hard cancel，处理 SDK 卡死、进程无响应、工具执行长时间不返回等情况。

## 标准化事件样例

已生成：

```text
.tmp\claude-runtime-spike\logs\normalized_events_sample.jsonl
```

样例 schema：

```json
{
  "schema_version": "p2ro.runtime_event.v0.spike",
  "seq": 1,
  "source": "sdk_query_basic.stdout.jsonl",
  "runtime": "claude_sdk",
  "session_id": "",
  "type": "session.started",
  "raw_type": "system",
  "raw_subtype": "init",
  "tool": null,
  "is_error": null,
  "result_subtype": null,
  "raw_event_ref": ""
}
```

事件统计：

| normalized type | count |
| --- | ---: |
| `claude_cli/session.started` | 2 |
| `claude_cli/message.thinking` | 2 |
| `claude_cli/message.completed` | 1 |
| `claude_cli/tool.call` | 1 |
| `claude_cli/stage.completed` | 1 |
| `claude_sdk/session.started` | 5 |
| `claude_sdk/usage.updated` | 509 |
| `claude_sdk/message.thinking` | 7 |
| `claude_sdk/message.completed` | 4 |
| `claude_sdk/tool.call` | 3 |
| `claude_sdk/tool.result` | 4 |
| `claude_sdk/stage.completed` | 4 |
| `claude_sdk/stage.interrupted` | 1 |

## 证据文件

核心证据：

```text
.tmp\claude-runtime-spike\logs\cli_json_basic_success.stdout.json
.tmp\claude-runtime-spike\logs\cli_stream_basic_success.stdout.jsonl
.tmp\claude-runtime-spike\logs\cli_json_tool_restricted.stdout.json
.tmp\claude-runtime-spike\logs\cli_stream_write_workspace.stdout.jsonl
.tmp\claude-runtime-spike\workspace\p2ro_probe.txt
.tmp\claude-runtime-spike\sdk-ts\node_modules\@anthropic-ai\claude-agent-sdk\package.json
.tmp\claude-runtime-spike\logs\sdk_query_basic.stdout.jsonl
.tmp\claude-runtime-spike\logs\sdk_query_write_allowed.permissions.jsonl
.tmp\claude-runtime-spike\logs\sdk_query_write_allowed.stdout.jsonl
.tmp\claude-runtime-spike\logs\sdk_query_write_bypass.stdout.jsonl
.tmp\claude-runtime-spike\workspace\p2ro_sdk_probe_bypass.txt
.tmp\claude-runtime-spike\logs\sdk_query_abort.stdout.jsonl
.tmp\claude-runtime-spike\sdk-ts\streaming_interrupt_probe.mjs
.tmp\claude-runtime-spike\logs\sdk_streaming_interrupt.stdout.jsonl
.tmp\claude-runtime-spike\logs\sdk_streaming_interrupt_control.jsonl
.tmp\claude-runtime-spike\logs\sdk_streaming_interrupt_summary.json
.tmp\claude-runtime-spike\logs\normalized_events_sample.jsonl
.tmp\claude-runtime-spike\logs\evidence_hashes.json
```

全部 probe 日志 hash 已写入：

```text
.tmp\claude-runtime-spike\logs\evidence_hashes.json
```

## 子代理结论纳入

子代理对 p2r Codex app-server 的结论：

- 现有 p2r 绑定 Codex JSON-RPC stdio：`initialize -> thread/start -> turn/start -> turn/steer`。
- 当前 D/E/F 是 read-only static reviewer，强制 `approval_policy="never"`、`sandbox_mode="read-only"`、`networkAccess=false`。
- Stage G 也不是让 Codex 操作浏览器，而是让 Codex 规划动作 JSON，再由 Playwright wrapper 执行。
- 该实现没有 producer 所需的 `permission.request`、`permission.decision`、workspace 写入、session resume、崩溃恢复语义。

子代理对 SDK/CLI 的结论：

- SDK 首选，但 Go TUI 应通过 TypeScript sidecar 接入。
- `tools` 才是真正的可用工具集合；`allowedTools` 只是自动批准列表。
- CLI stream-json 可作 fallback，但取消和权限语义弱于 SDK。
- 不建议依赖 `claude app-server` 或 `claude mcp serve`。

## 对 p2ro 的实现建议

### Adapter 选择

MVP 实现顺序：

```text
Go TUI
  -> internal/agent RuntimeAdapter
  -> TypeScript sidecar JSONL protocol
  -> @anthropic-ai/claude-agent-sdk
```

保留：

```text
ClaudeCLIAdapter
  -> smoke test
  -> SDK 不可用时 fallback
```

不采用：

```text
claude app-server
  -> 本机和官方路径均未验证
```

### Sidecar 最小协议

Go -> sidecar：

```json
{"type":"start","run_id":"","stage":"","cwd":"","tools":["Read","Write"],"permission_mode":"default"}
{"type":"user","content":""}
{"type":"permission.decision","request_id":"","decision":"allow"}
{"type":"cancel","reason":""}
```

sidecar -> Go：

```json
{"type":"session.started","session_id":"","cwd":"","tools":[]}
{"type":"message.delta","text":""}
{"type":"tool.call","tool":"","input":{}}
{"type":"tool.result","tool":"","ok":true}
{"type":"permission.request","request_id":"","tool":"","input":{}}
{"type":"permission.decision","request_id":"","decision":""}
{"type":"diff.updated","paths":[]}
{"type":"usage.updated","usage":{}}
{"type":"runtime.error","message":""}
{"type":"stage.completed","result":{}}
```

### 权限策略

默认不使用全局 bypass。

推荐两层控制：

1. p2ro policy engine 决策 read/write/test/install/network/delete/git。
2. sidecar 外层进程沙箱限制 cwd、环境变量、网络、路径、防逃逸和超时。

如果必须使用 SDK `bypassPermissions`，只能在 p2ro 自己已经完成强隔离的 workspace 中启用。

### 仍需补强

- 修正或配置本机 OMC/Claude 权限，使 `canUseTool` allow 的 workspace 写入能在非 bypass 模式通过。
- 设计 Go 管理 sidecar 进程树的 hard cancel。
- 为 CLI adapter 实现 partial result 处理：文件已写但无最终 `result` 时进入 `manual_required` 或 `stage.failed`，不能静默成功。

## 最终判定

Claude runtime 可行，但不是 `claude app-server` 可行。

SDK 路径满足 p2ro 的主要架构需求：会话、事件流、streaming input、`query.interrupt()`、工具集合、usage、权限 denial、workspace 写入能力。生产级落地剩余硬点主要是非 bypass 权限配置。CLI 路径能作为 fallback，但不应承载长期 producer runtime。

下一步应实现 `ClaudeSDKAdapter` 的 TypeScript sidecar spike，并把本报告中的 `.tmp` 证据作为 fixture，驱动 Go 侧 `RuntimeAdapter` 事件解析和权限队列设计。
