# TUI 改造实施审计

本文按 `TUI_IMPROVEMENT_PLAN.md` 的 Phase 1–4 逐项记录实现位置和验证证据。只有同时具备可执行实现与测试证据的条目才标记为完成。

## Phase 1：基础

| 要求 | 实现 | 验证 |
|---|---|---|
| 中文本地化 | `internal/tui/localize.go`；页面固定文案均为中文 | `TestLocalizeCoversWorkflowVocabulary`、`TestKnownUIChromeContainsNoLegacyEnglishLabels` |
| Theme 样式系统 | `internal/tui/styles.go` | 页面快照及宽度测试 |
| 消息类型拆分 | `internal/tui/messages.go` | 全量编译与事件测试 |
| Unicode 状态图标 | `statusIcon` | 原有状态渲染测试 |
| 鼠标和焦点报告 | `run.go` 的 `WithMouseCellMotion`、`WithReportFocus` | `TestRunEnablesAltScreenMouseAndFocusReporting`、鼠标交互测试 |
| textinput 启动表单 | `start_form.go`、`page_start.go` | `TestStartTextInputSupportsCursorAndChinese` |
| spinner 进度 | `statusbar.go`、`app.go` | `TestSpinnerComponentAdvancesOnTick` |

## Phase 2：核心重构

| 要求 | 实现 | 验证 |
|---|---|---|
| Page/Overlay Router | `router.go`，Router 实际分发当前页面消息 | `TestRouterDispatchesToActivePage` |
| Focus Manager | `focus.go`，页面、搜索和覆盖层使用焦点栈 | `TestFocusManagerRestoresPreviousArea` |
| 页面拆分 | `page_start.go`、`page_overview.go`、`page_gate.go`、`page_detail.go`、`page_logs.go`、`page_done.go` | 页面测试、固定尺寸快照 |
| 顶层 App | `app.go` 负责 Init/Update/View | 全量集成测试 |
| 上下文 Footer | `keymap.go` 的 `footer` | `TestFooterAndHelpAreContextAware` |
| 确认对话框 | `confirm.go` | Gate、取消、退出、编辑确认测试 |
| 新旧键位 | `keymap.go` 与各 Page Update；保留数字键 | 键盘集成测试 |

## Phase 3：UX 打磨

| 要求 | 实现 | 验证 |
|---|---|---|
| 两步启动向导及高级分组 | `page_start.go`；高级页单组手风琴，F1–F4 切换；输入框按终端宽度进行 ANSI/CJK 安全截断 | `TestStartWizardSeparatesBasicAndAdvancedSteps`、`TestStartWizardFitsTerminalHeight`、两份 golden snapshot |
| Gate 多行备注 | `bubbles/textarea` | `TestGateNotesTextareaSupportsChineseMultiline` |
| 节点详情 Viewport | `bubbles/viewport`，PageUp/Down/首尾与鼠标滚动更新组件状态 | `TestDetailViewportComponentScrolls` |
| Overview Table | `bubbles/table`，组件处理上下翻页并同步选中节点 | Router/Table 导航测试 |
| Checklist 组件 | `checklist.go` | Gate 渲染和筛选测试 |
| Toast | `toast.go` | `TestToastExpirationDoesNotClearNewerMessage` |

## Phase 4：高级特性

| 要求 | 实现 | 验证 |
|---|---|---|
| 四断点响应式布局 | `layout.go`；宽/中屏左右分栏，窄屏堆叠 | `TestResponsiveLayoutBreakpoints`、`TestResponsiveColumnsChangeComposition` |
| 搜索/过滤 | 总览、审查、详情、日志页面 | 键盘搜索、Overview 和 Gate 筛选测试 |
| 帮助面板 | `?` 覆盖层 | Footer/Help 测试 |
| 路径补全 | CJK、目录和逗号分隔目录列表补全 | 两个路径补全测试 |
| Tab 行为 | 页面级下一项/上一项语义 | `docs/TUI_USAGE.md` 和页面键位测试 |
| 滚动指示器 | Overview、Detail、Logs、长表单 | Viewport、日志和快照测试 |

## 额外安全与边界修复

- 外部编辑器直接按 argv 执行，不经过 `sh -c`。
- 文件预览按 UTF-8 边界截断。
- 只读模式隐藏不可用操作，并阻止写入。
- 工作区刷新始终只安排一个后续 tick，避免重叠轮询。
- 所有可破坏操作均经过显式确认。
