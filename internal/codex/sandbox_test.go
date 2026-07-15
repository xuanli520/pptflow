package codex

import (
	"runtime"
	"strings"
	"testing"
)

func TestSanitizeEnvironmentDropsAmbientProviderConfiguration(t *testing.T) {
	env := SanitizeEnvironment([]string{
		"PATH=/custom/bin",
		"LANG=C.UTF-8",
		"HOME=/ambient/home",
		"XDG_CONFIG_HOME=/ambient/config",
		"CODEX_HOME=/ambient/codex-home",
		"CODEX_API_KEY=ambient-codex-key",
		"OPENAI_API_KEY=ambient-openai-key",
		"OPENAI_BASE_URL=https://ambient.example/v1",
		"UNRELATED_SECRET=must-not-pass",
	}, map[string]string{
		"CODEX_HOME":     "/approved/codex-home",
		"OPENAI_API_KEY": "explicit-key",
		"CUSTOM_SECRET":  "explicit-secret",
	})
	values := environmentValues(env)
	if values["CODEX_HOME"] != "/approved/codex-home" || values["OPENAI_API_KEY"] != "explicit-key" || values["CUSTOM_SECRET"] != "explicit-secret" {
		t.Fatalf("explicit environment was not preserved: %#v", values)
	}
	for _, forbidden := range []string{"HOME", "XDG_CONFIG_HOME", "CODEX_API_KEY", "OPENAI_BASE_URL", "UNRELATED_SECRET"} {
		if _, ok := values[forbidden]; ok {
			t.Fatalf("ambient provider value %s leaked into configured environment: %#v", forbidden, values)
		}
	}
	if values["LANG"] != "C.UTF-8" {
		t.Fatalf("execution-neutral locale was unexpectedly removed: %#v", values)
	}
	if runtime.GOOS != "windows" {
		for _, path := range systemBinPaths {
			if !pathListContains(values["PATH"], path) {
				t.Fatalf("system binary directory %s missing from PATH %q", path, values["PATH"])
			}
		}
	}
}

func TestSanitizeEnvironmentIsDeterministic(t *testing.T) {
	base := []string{"LANG=C", "PATH=/base"}
	configured := map[string]string{"Z_VALUE": "z", "A_VALUE": "a"}
	first := strings.Join(SanitizeEnvironment(base, configured), "\n")
	second := strings.Join(SanitizeEnvironment(base, configured), "\n")
	if first != second {
		t.Fatalf("sanitized environment order is not stable:\nfirst=%q\nsecond=%q", first, second)
	}
}

func environmentValues(env []string) map[string]string {
	values := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}
