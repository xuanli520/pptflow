package codeedge

import "testing"

func TestGitEvidenceFromTokensSkipsGitConfigArguments(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		repos   int
		commits int
	}{
		{"plain clone", "git clone https://github.com/yewstack/yew.git /opt/x", 1, 0},
		{"clone with -c config", `git -c http.version=HTTP/1.1 clone --quiet https://github.com/yewstack/yew.git /opt/x`, 1, 0},
		{"clone with -C dir", "git -C /opt/x clone https://github.com/yewstack/yew.git", 1, 0},
		{"clone with -c and -C", `git -c http.version=HTTP/1.1 -C /opt/x clone https://github.com/yewstack/yew.git`, 1, 0},
		{"checkout after -c", `git -c http.version=HTTP/1.1 checkout --quiet 4c3bcdc692b067907fb0e9402a3c08b9f872bc0f`, 0, 1},
		{"checkout after --config", `git --config http.version=HTTP/1.1 checkout 4c3bcdc692b067907fb0e9402a3c08b9f872bc0f`, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := gitEvidenceFromTokens(dockerRunTokens(tc.line))
			if len(ev.repositories) != tc.repos || len(ev.commits) != tc.commits {
				t.Fatalf("repositories=%v commits=%v want repositories=%d commits=%d", ev.repositories, ev.commits, tc.repos, tc.commits)
			}
		})
	}
}
