package agent

import (
	"slices"
	"strings"
	"testing"

	envutil "github.com/buildkite/agent/v3/env"
)

// Tests that a job's own env can't turn off --enforce-git-commit-verification,
// or point the git it relies on somewhere else.

func TestCreateEnvironmentEnforceGitCommitVerification(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		agent   bool
		jobEnv  map[string]string
		wantEnv string
	}{
		{name: "agent on, job tries to turn it off", agent: true, jobEnv: map[string]string{"BUILDKITE_ENFORCE_GIT_COMMIT_VERIFICATION": "false"}, wantEnv: "true"},
		{name: "agent off, job tries to turn it on", agent: false, jobEnv: map[string]string{"BUILDKITE_ENFORCE_GIT_COMMIT_VERIFICATION": "true"}, wantEnv: "false"},
		{name: "agent on, job silent", agent: true, wantEnv: "true"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := controlPlaneTestRunner(t, tt.jobEnv, AgentConfiguration{EnforceGitCommitVerification: tt.agent})
			got, err := r.createEnvironment(t.Context())
			if err != nil {
				t.Fatalf("r.createEnvironment(ctx) error = %v", err)
			}
			if v, _ := envutil.FromSlice(got).Get("BUILDKITE_ENFORCE_GIT_COMMIT_VERIFICATION"); v != tt.wantEnv {
				t.Errorf("bootstrap env BUILDKITE_ENFORCE_GIT_COMMIT_VERIFICATION = %q, want %q", v, tt.wantEnv)
			}
		})
	}
}

func TestCreateEnvironmentDropsGitRedirectingEnvWhenEnforcing(t *testing.T) {
	t.Parallel()

	jobEnv := map[string]string{
		"GIT_CONFIG_COUNT":      "1",
		"GIT_CONFIG_KEY_0":      "url.https://example.com/other.git.insteadOf",
		"GIT_CONFIG_VALUE_0":    "git@github.com:org/repo.git",
		"GIT_CONFIG_PARAMETERS": "'url.https://example.com/other.git.insteadof'='git@github.com:org/repo.git'",
		"GIT_SSH_COMMAND":       "ssh -o BatchMode=yes",
		"GIT_DIR":               "/tmp/elsewhere",
		"GIT_LFS_SKIP_SMUDGE":   "1",
	}
	redirecting := []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_PARAMETERS", "GIT_SSH_COMMAND", "GIT_DIR"}

	for _, enforcing := range []bool{true, false} {
		r := controlPlaneTestRunner(t, jobEnv, AgentConfiguration{EnforceGitCommitVerification: enforcing})
		got, err := r.createEnvironment(t.Context())
		if err != nil {
			t.Fatalf("r.createEnvironment(ctx) error = %v", err)
		}
		env := envutil.FromSlice(got)
		ignoredEnv, _ := env.Get("BUILDKITE_IGNORED_ENV")
		ignored := strings.Split(ignoredEnv, ",")

		for _, name := range redirecting {
			_, present := env.Get(name)
			if enforcing && (present || !slices.Contains(ignored, name)) {
				t.Errorf("enforcing: %s present = %t, in BUILDKITE_IGNORED_ENV = %t; want dropped and reported", name, present, slices.Contains(ignored, name))
			}
			if !enforcing && !present {
				t.Errorf("not enforcing: %s was dropped, want it passed through unchanged", name)
			}
		}
		if v, _ := env.Get("GIT_LFS_SKIP_SMUDGE"); v != "1" {
			t.Errorf("enforcing = %t: GIT_LFS_SKIP_SMUDGE = %q, want %q (it can't redirect git)", enforcing, v, "1")
		}
	}
}
