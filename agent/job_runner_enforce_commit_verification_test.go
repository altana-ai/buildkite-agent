package agent

import (
	"testing"

	envutil "github.com/buildkite/agent/v3/env"
)

// Tests that a job's own env can't turn off --enforce-git-commit-verification.

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
