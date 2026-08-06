package tui

// displayStageName translates stable workflow keys only at the TUI boundary.
// Unknown keys stay visible verbatim so operational diagnostics are never
// obscured by a stale display mapping.
func displayStageName(stage string) string {
	switch stage {
	case "repo_prepare":
		return "源码准备"
	case "repo_analyze":
		return "仓库分析"
	case "task_design":
		return "题目设计"
	case "task_review":
		return "题目审核"
	case "generate_task_files":
		return "任务文件规划"
	case "instruction_generate":
		return "题目说明生成"
	case "task_toml_generate":
		return "任务元数据生成"
	case "dockerfile_generate":
		return "Dockerfile 生成"
	case "dockerfile_build_validate":
		return "Dockerfile 构建验证"
	case "content_review":
		return "内容审核"
	case "solve_generate":
		return "参考解答生成"
	case "test_generate":
		return "测试生成"
	case "authoring_harness":
		return "Authoring harness 修复验证"
	case "tests_analysis":
		return "测试分析"
	case "codeedge_package_admission":
		return "任务包准入"
	case "solution_review":
		return "解答审核"
	case "materialize_task":
		return "任务物化"
	case "docker_build":
		return "Docker 镜像构建"
	case "initial_verify":
		return "初始失败验证"
	case "oracle_verify":
		return "参考解答验证"
	default:
		return stage
	}
}
