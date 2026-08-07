package tui

// Stage keys are stable workflow policy owned by the workflow adapter. They are
// translated only here, at the terminal boundary, so no display concern leaks
// into the frozen graph.
//
// The table is grouped by the template that declares each key. Two templates are
// compiled into this binary, and a durable record from either one can still be
// read back by the board: Standard Authoring 3.0 is what a new Run executes,
// while the Standard lifecycle keys remain reachable through historical Runs.
// Translating both keeps an old Run's stage readable instead of degrading it to
// a raw key the moment its template stops being instantiable.
//
// An unknown key is returned verbatim. That fallback is deliberate: a stage this
// build has never heard of must stay diagnosable, and a missing translation is a
// far smaller problem than a stage key silently rendered as something else.
var stageDisplayNames = map[string]string{
	// Standard Authoring 3.0 — the graph a new Run executes today.
	"repo_prepare":               "源码准备",
	"repo_structure_research":    "仓库结构调研",
	"test_runtime_research":      "测试运行时调研",
	"verifier_threat_research":   "验证绕过风险调研",
	"task_synthesis":             "题目综合设计",
	"task_review":                "题目审核",
	"authoring_loop":             "题目内容生成",
	"host_candidate_verify":      "候选宿主机验证",
	"test_quality_critic":        "测试质量评审",
	"solution_integrity_critic":  "解答完整性评审",
	"authoring_repair":           "题目修复",
	"content_review":             "内容审核",
	"solution_review":            "解答审核",
	"final_attestation":          "最终认证",
	"codeedge_package_admission": "任务包准入",
	"materialize_task":           "任务物化",

	// Standard lifecycle — retained for Runs recorded before the 3.0 cutover.
	"repo_analyze":              "仓库分析",
	"task_design":               "题目设计",
	"generate_task_files":       "任务文件规划",
	"instruction_generate":      "题目说明生成",
	"task_toml_generate":        "任务元数据生成",
	"dockerfile_generate":       "Dockerfile 生成",
	"dockerfile_build_validate": "Dockerfile 构建验证",
	"solve_generate":            "参考解答生成",
	"test_generate":             "测试生成",
	"tests_analysis":            "测试分析",
	"authoring_harness":         "Authoring harness 修复验证",
	"task_repair":               "任务修复",
	"runtime_self_check":        "运行时自检",
	"harbor_verify":             "Harbor 校验",
	"docker_build":              "Docker 镜像构建",
	"initial_verify":            "初始失败验证",
	"oracle_verify":             "参考解答验证",
	"codeedge_lint":             "CodeEdge 静态检查",
	"quality_check":             "质量检查",
	"similarity_check":          "相似度检查",
	"final_review":              "终审",
	"package":                   "打包交付",
}

// displayStageName translates one workflow stage key for display, returning the
// key itself when this build has no translation for it.
func displayStageName(stage string) string {
	if name, found := stageDisplayNames[stage]; found {
		return name
	}
	return stage
}
