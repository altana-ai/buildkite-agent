package job

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/env"
	"github.com/buildkite/agent/v3/internal/job/githttptest"
	"github.com/buildkite/agent/v3/internal/shell"
)

// Tests for --enforce-git-commit-verification, which fails a job unless its
// commit is reachable from its branch (or tag), even without a checkout.

type enforcementFixture struct {
	repository    string
	mainCommit    string
	featureCommit string
}

func newEnforcementFixture(t *testing.T) *enforcementFixture {
	t.Helper()

	t.Setenv("GIT_AUTHOR_NAME", "Buildkite Agent")
	t.Setenv("GIT_AUTHOR_EMAIL", "agent@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Buildkite Agent")
	t.Setenv("GIT_COMMITTER_EMAIL", "agent@example.com")

	const repoName = "enforcement"
	s := githttptest.NewServer()
	t.Cleanup(s.Close)
	if err := s.CreateRepository(repoName); err != nil {
		t.Fatalf("s.CreateRepository(%q) error = %v", repoName, err)
	}
	if out, err := s.InitRepository(repoName); err != nil {
		t.Fatalf("s.InitRepository(%q) error = %v, output: %s", repoName, err, out)
	}
	featureCommit, out, err := s.PushBranch(repoName, "feature")
	if err != nil {
		t.Fatalf("s.PushBranch(%q, feature) error = %v, output: %s", repoName, err, out)
	}
	if out, err := s.CreateRef(repoName, "refs/tags/v1.0.0", featureCommit); err != nil {
		t.Fatalf("s.CreateRef(%q, refs/tags/v1.0.0) error = %v, output: %s", repoName, err, out)
	}

	repository := s.RepoURL(repoName)
	lsRemote, err := exec.Command("git", "ls-remote", repository, "refs/heads/main").Output()
	if err != nil {
		t.Fatalf("git ls-remote %q refs/heads/main error = %v", repository, err)
	}
	mainCommit, _, _ := strings.Cut(string(lsRemote), "\t")

	return &enforcementFixture{repository: repository, mainCommit: mainCommit, featureCommit: featureCommit}
}

func newEnforcementExecutor(t *testing.T, conf ExecutorConfig) *Executor {
	t.Helper()

	conf.EnforceGitCommitVerification = true
	conf.BuildPath = t.TempDir()
	e := New(conf)
	e.shell = shell.NewTestShell(t, shell.WithSignalGracePeriod(10*time.Millisecond))
	return e
}

func TestEnforceCommitVerification(t *testing.T) {
	f := newEnforcementFixture(t)

	tests := []struct {
		name   string
		branch string
		tag    string
		commit string
		repo   string
		// Empty means the job may proceed.
		wantErrContains string
	}{
		{name: "commit on its branch", branch: "main", commit: f.mainCommit},
		{name: "commit an ancestor of its branch tip", branch: "feature", commit: f.mainCommit},
		{name: "branch given as a full ref", branch: "refs/heads/main", commit: f.mainCommit},
		{name: "commit at its tag", branch: "v1.0.0", tag: "v1.0.0", commit: f.featureCommit},
		{name: "abbreviated commit", branch: "main", commit: f.mainCommit[:12]},
		{name: "HEAD builds the branch tip", branch: "main", commit: "HEAD"},
		{
			name:            "commit only on another branch",
			branch:          "main",
			commit:          f.featureCommit,
			wantErrContains: "is not in the history of refs/heads/main",
		},
		{
			name:            "commit not reachable from its tag",
			branch:          "main",
			tag:             "v1.0.0",
			commit:          strings.Repeat("ab", 20),
			wantErrContains: "is not in the history of refs/tags/v1.0.0",
		},
		{
			name:            "branch missing from the repository",
			branch:          "deleted-branch",
			commit:          f.mainCommit,
			wantErrContains: "couldn't fetch refs/heads/deleted-branch",
		},
		{
			name:            "commit given as a ref name",
			branch:          "main",
			commit:          enforcedVerificationTipRef,
			wantErrContains: "is not a commit hash",
		},
		{
			name:            "no branch or tag",
			commit:          f.mainCommit,
			wantErrContains: "no branch or tag",
		},
		{
			name:            "no repository",
			branch:          "main",
			commit:          f.mainCommit,
			repo:            "-",
			wantErrContains: "no repository",
		},
		{
			name:            "branch that would rewrite the refspec",
			branch:          "main:refs/heads/other",
			commit:          f.mainCommit,
			wantErrContains: "not a valid ref",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := f.repository
			if tt.repo == "-" {
				repo = ""
			}
			e := newEnforcementExecutor(t, ExecutorConfig{
				Repository: repo,
				Branch:     tt.branch,
				Tag:        tt.tag,
				Commit:     tt.commit,
			})

			err := e.enforceCommitVerification(t.Context())
			if tt.wantErrContains == "" {
				if err != nil {
					t.Fatalf("e.enforceCommitVerification(ctx) = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrCommitNotVerified) {
				t.Fatalf("e.enforceCommitVerification(ctx) = %v, want an error wrapping ErrCommitNotVerified", err)
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Errorf("e.enforceCommitVerification(ctx) = %q, want it to contain %q", err, tt.wantErrContains)
			}
		})
	}
}

func TestEnforceCommitVerificationOffDoesNothing(t *testing.T) {
	e := New(ExecutorConfig{Branch: "main", Commit: "not-a-commit"})
	e.shell = shell.NewTestShell(t)
	if err := e.enforceCommitVerification(t.Context()); err != nil {
		t.Errorf("e.enforceCommitVerification(ctx) with enforcement off = %v, want nil", err)
	}
}

// A commit present in a borrowed object store but off the branch must still
// fail: borrowing objects must not stand in for reachability.
func TestEnforceCommitVerificationBorrowedObjectsDoNotVerify(t *testing.T) {
	f := newEnforcementFixture(t)

	mirrors := t.TempDir()
	mirror := filepath.Join(mirrors, dirForRepository(f.repository))
	if out, err := exec.Command("git", "clone", "--mirror", "--", f.repository, mirror).CombinedOutput(); err != nil {
		t.Fatalf("git clone --mirror error = %v, output: %s", err, out)
	}

	for _, tt := range []struct {
		name    string
		commit  string
		wantErr bool
	}{
		{name: "on the branch", commit: f.mainCommit},
		{name: "off the branch but in the mirror", commit: f.featureCommit, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnforcementExecutor(t, ExecutorConfig{
				Repository:     f.repository,
				Branch:         "main",
				Commit:         tt.commit,
				GitMirrorsPath: mirrors,
			})
			err := e.enforceCommitVerification(t.Context())
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("e.enforceCommitVerification(ctx) = %v, want error: %t", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "is not on refs/heads/main") {
				t.Errorf("e.enforceCommitVerification(ctx) = %q, want it to say the commit is not on refs/heads/main", err)
			}
		})
	}
}

// A skipped checkout is one of the ways around --git-commit-verification, so the
// enforced check has to run first.
func TestCheckoutPhaseEnforcesBeforeSkippedCheckout(t *testing.T) {
	f := newEnforcementFixture(t)

	e := newEnforcementExecutor(t, ExecutorConfig{
		Repository:   f.repository,
		Branch:       "main",
		Commit:       f.featureCommit,
		SkipCheckout: true,
	})
	checkoutPath, err := os.MkdirTemp("", "checkout-path-")
	if err != nil {
		t.Fatalf("os.MkdirTemp(checkout-path-) error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(checkoutPath) }) //nolint:errcheck // Best-effort cleanup.
	e.shell.Env.Set("BUILDKITE_BUILD_CHECKOUT_PATH", checkoutPath)
	t.Cleanup(func() {
		if e.checkoutRoot != nil {
			_ = e.checkoutRoot.Close()
		}
	})

	if err := e.CheckoutPhase(t.Context()); !errors.Is(err, ErrCommitNotVerified) {
		t.Fatalf("e.CheckoutPhase(ctx) with a skipped checkout = %v, want an error wrapping ErrCommitNotVerified", err)
	}
}

func TestEnforceGitCommitVerificationIsLockedAgainstJobs(t *testing.T) {
	const name = "BUILDKITE_ENFORCE_GIT_COMMIT_VERIFICATION"
	if !env.IsProtected(name) {
		t.Errorf("env.IsProtected(%q) = false, want true so job env and secrets can't set it", name)
	}
	if !env.IsProtectedFromWithinJob(name) {
		t.Errorf("env.IsProtectedFromWithinJob(%q) = false, want true so hooks and plugins can't set it", name)
	}
	if env.IsCheckoutOverrideScoped(name) {
		t.Errorf("env.IsCheckoutOverrideScoped(%q) = true, want false so no checkout-override mode unlocks it", name)
	}
}

func TestReferenceRepositories(t *testing.T) {
	got := referenceRepositories(`-v --reference-if-able /opt/seed.git --dissociate --reference=/srv/other "--reference" "/path with space"`)
	want := []string{"/opt/seed.git", "/srv/other", "/path with space"}
	if !slices.Equal(got, want) {
		t.Errorf("referenceRepositories(...) = %q, want %q", got, want)
	}
}
