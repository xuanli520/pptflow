package git

type SyncProgress struct {
	Phase   string
	Percent int
	Message string
}

type SyncResult struct {
	Operation string
	Commit    string
	RepoPath  string
	ClonePath string
	Error     error
}

type SyncCallback func(SyncProgress)
