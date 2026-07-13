# 已归档：V1 TUI 改造实施审计

> 状态：已由 2026-07-13 的 Workflow Stability and Task Lifecycle V2 硬切换归档。
>
> 本文件记录旧 workspace-runner TUI、StartForm、workspace 索引、clone rerun、repair overlay 和直接文件 mutation 的实现。这些路径不再是公开行为，不能用作实现或验证清单。

当前操作契约：

- [Workflow Stability and Task Lifecycle Refactor Plan](./WORKFLOW_STABILITY_AND_TASK_LIFECYCLE_REFACTOR_PLAN.md)
- [Confirmed Decision Record](./WORKFLOW_STABILITY_DECISIONS.md)
- [V2 Task Hub 使用指南](./TUI_USAGE.md)

V2 通过 `harbor-factory tui` 进入，只调用 lifecycle application-service boundary。它投影 Task、TaskRevision、WorkflowRun、ContinuationPlan、durable control 和本地 package，不再把旧 workspace 当作生命周期身份。
