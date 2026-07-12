package gen

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func repoAnalyzePrompt(prepared domain.RepoPrepared) string {
	return fmt.Sprintf(`你是 CodeEdge Harbor 任务工厂的仓库分析 Agent。你正在只读分析一个已经 checkout 到固定 commit 的本地源码目录。

仓库 URL: %s
固定 commit: %s
tree hash: %s

目标:
1. 阅读目录结构、构建系统、测试框架、关键模块和入口。
2. 找出适合作为 agentic coding benchmark 的真实工程场景。
3. 排除纯算法题、纯文档题、只改注释/变量名、小模板题、依赖环境故障的题。
4. 不要直接照搬 GitHub issue/PR/commit。

只返回一个 JSON 对象，不要输出 Markdown、解释或代码块。schema 必须是:
{
  "schema_version": "harbor.repo_analysis.v1",
  "repo_url": "...",
  "commit_sha": "...",
  "language": "...",
  "language_version": "...",
  "build_system": "...",
  "test_framework": "...",
  "key_modules": [{"name": "...", "purpose": "..."}],
  "entry_points": ["..."],
  "dependencies": ["..."],
  "potential_task_areas": [
    {
      "area": "...",
      "module": "...",
      "description": "...",
      "difficulty": "medium",
      "type_suggestion": "bug-fix",
      "affected_files_count": 3
    }
  ]
}
`, prepared.RepoURL, prepared.ResolvedCommit, prepared.TreeHash)
}

func taskDesignPrompt(repoAnalysisJSON string) string {
	return fmt.Sprintf(`你是 CodeEdge Harbor 任务设计 Agent。根据 repo_analysis 设计一个高价值的一阶段 Harbor 题。

repo_analysis:
%s

设计要求:
1. 任务必须来自真实工程场景，需要模型读代码、定位、修改、运行命令验证。
2. 不要把难度建立在 Docker 坏、依赖缺失、网络问题、隐藏测试或不可复现资源上。
3. 任务目标和 tests 后续可验证点必须能从 instruction + environment 推导。
4. Qwen pass@4 预期应不超过 1/4，平均轮数预期不少于 20。
5. 非 0-1 任务必须保留 GitHub URL 和固定 commit。
6. setup_commands 只包含仓库已位于 /app/repo 后所需的依赖准备命令。不得包含 git clone、git checkout/reset/switch、单独 cd、测试或 verifier 命令；仓库 clone 和 commit 固定由系统生成。

只返回一个 JSON 对象，不要输出 Markdown、解释或代码块。schema 必须是:
{
  "schema_version": "harbor.task_proposal.v1",
  "task_name": "codeedge/descriptive-task-name",
  "one_line_description": "...",
  "code_lang": "go",
  "task_type": "bug-fix",
  "application": "backend",
  "is_0_to_1": false,
  "github_link": "...",
  "commit_sha": "...",
  "estimated_aht_minutes": 45,
  "target_files": ["..."],
  "affected_modules": ["..."],
  "difficulty_rationale": "...",
  "boundary_conditions": ["..."],
  "suggested_verification": "...",
  "setup_commands": ["仅依赖安装或预取命令，例如 cargo fetch、go mod download、npm ci"]
}
`, repoAnalysisJSON)
}

func taskFilesPrompt(repoAnalysisJSON, proposalJSON string) string {
	return fmt.Sprintf(`你是 CodeEdge Harbor 任务内容生成 Agent。根据 repo_analysis 和 task_proposal 生成 Harbor 任务的语义文件内容。

repo_analysis:
%s

task_proposal:
%s

你只生成这些内容:
- instruction_md: 给模型看的任务说明。必须说明背景、问题、目标文件/模块、约束、保持行为、建议验证方式。不要泄露标准答案或测试内部断言。
- solve_sh: oracle 脚本主体。必须从 /app/repo 初始环境出发完成标准修复。不要修改 tests、solution、reward 或 verifier。
- test_sh: verifier 脚本主体。必须能区分好坏答案，覆盖核心需求和关键边界。不要只检查文件存在或 exit 0。
- tests_analysis_md: 必须严格包含 CodeEdge 三段标题，说明 verifier 检查点可由 instruction/environment 推导。

约束:
1. shell 脚本不用写 shebang 和 set -euo pipefail，系统会自动补。
2. 所有脚本默认工作目录是 /app/repo。
3. 不要让 test_sh 依赖外网、私有 token、当前时间或随机数。
4. 不要在 test_sh 中泄露 solve_sh 的完整实现细节。
5. test_sh 使用的精确公开类型名、错误类型名和配置 API 必须在 instruction_md 中明确声明，不能把它们作为隐藏契约。
6. instruction_md 若同时要求 Layer API 和直接 Service API，test_sh 必须分别覆盖两条公开配置路径。
7. test_sh 可以设置清理 trap；系统会将脚本主体放入子 shell，清理逻辑不得写入或覆盖 /logs/verifier/reward 的父级 EXIT trap。
8. test_sh 或 solve_sh 创建临时源码/测试文件时，必须先用 mkdir -p 创建其父目录；不得假设目标仓库已存在 tests、fixtures 或生成目录。
9. solve_sh 和 test_sh 只能使用 environment 中明确安装或基础镜像自带的命令。禁止使用 Codex 专用 apply_patch；补丁必须使用 git apply、patch、sed 等标准命令。
   使用 git apply 时，heredoc 必须是真实 unified diff，以 diff --git、---、+++ 和带行号的 @@ -a,b +c,d @@ 组成；严禁把 *** Begin Patch / *** Update File 格式传给 git apply。
10. Rust setup_commands 使用 cargo fetch 即可，不要添加 --locked、--frozen 或 --offline；系统会根据 Cargo.lock 是否存在自动选择锁定模式。

只返回一个 JSON 对象，不要输出 Markdown、解释或代码块。schema 必须是:
{
  "schema_version": "harbor.generated_task_files.v1",
  "instruction_md": "...",
  "solve_sh": "...",
  "test_sh": "...",
  "tests_analysis_md": "...",
  "extra_notes": "..."
}
`, repoAnalysisJSON, proposalJSON)
}

func runtimeSelfCheckPrompt() string {
	return `You are performing the first runtime self-check of the Harbor task you just designed.

You have explicit authorization to edit the standard task files in the current task directory, access the network, and run Docker build/run commands. Inspect instruction.md, task.toml, environment/Dockerfile, solution/solve.sh, tests/test.sh, and tests_analysis.md as one contract.

Required self-check:
1. Run shell syntax checks and ensure every command used by solve.sh/test.sh exists in the image.
2. Build environment/Dockerfile.
3. Run tests/test.sh against the untouched image and confirm it fails for the intended behavioral reason, not missing paths/tools or syntax errors.
4. Run solution/solve.sh followed by tests/test.sh and confirm the oracle passes.
5. If any step fails, repair the task files and repeat the focused failing step.
6. Do not weaken assertions, expose the solution in instruction.md, change the pinned repository/commit, or write credentials into task files.
7. Remove temporary containers/images you created when practical.

This is a repair-capable runtime validation turn. Finish only after the task is internally consistent, or clearly report the remaining concrete blocker so mandatory machine gates can reject it.`
}
