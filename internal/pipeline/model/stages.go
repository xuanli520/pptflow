package model

func StageDisplayName(stage string) string {
	switch stage {
	case "A":
		return "structure and rules check"
	case "B":
		return "Docker runtime evidence"
	case "C":
		return "run_tests runtime evidence"
	case "D":
		return "tests effectiveness static review"
	case "E":
		return "static acceptance audit"
	case "F":
		return "annotator repair static review"
	default:
		return "unknown"
	}
}
