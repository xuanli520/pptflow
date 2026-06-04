package pipeline

type BrowserPolicy struct {
	URLCandidates    []BrowserURLCandidate `json:"url_candidates"`
	AllowlistOrigins []string              `json:"allowlist_origins"`
}

func browserPolicy(runtime RuntimeState) BrowserPolicy {
	candidates := browserURLCandidates(runtime)
	return BrowserPolicy{
		URLCandidates:    candidates,
		AllowlistOrigins: browserAllowlistOrigins(candidates),
	}
}
