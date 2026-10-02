package job

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/env"
	"github.com/buildkite/agent/v3/internal/shell"
)

// Tests for --enforce-git-commit-verification, which fails a job unless its
// commit is reachable from its branch (or tag), even without a checkout.

type enforcementFixture struct {
	origin        string
	repository    string
	mainCommit    string
	featureCommit string
	// deepCommit is on main beyond the first shallow fetch, so finding it
	// needs a deepen.
	deepCommit string
}

// mainHistoryLength is more than the 64 commits of the first shallow fetch.
const mainHistoryLength = 100

func (f *enforcementFixture) createRef(t *testing.T, ref, commit string) {
	t.Helper()
	gitForTest(t, f.origin, "update-ref", ref, commit)
}

// newEnforcementFixture serves a bare repository over file://. Like GitHub it
// honours the commits-only filter and deepening fetches, which githttptest's
// minimal upload-pack can't negotiate.
//
//	main:    100 commits; deepCommit is 90 back from the tip
//	feature: main plus one commit, also tagged v1.0.0
func newEnforcementFixture(t *testing.T) *enforcementFixture {
	t.Helper()

	t.Setenv("GIT_AUTHOR_NAME", "Buildkite Agent")
	t.Setenv("GIT_AUTHOR_EMAIL", "agent@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Buildkite Agent")
	t.Setenv("GIT_COMMITTER_EMAIL", "agent@example.com")

	origin := filepath.Join(t.TempDir(), "enforcement.git")
	gitForTest(t, filepath.Dir(origin), "init", "--quiet", "--bare", "--initial-branch=main", origin)
	gitForTest(t, origin, "config", "uploadpack.allowFilter", "true")
	repository := "file://" + filepath.ToSlash(origin)

	clone, err := os.MkdirTemp("", "enforcement-work-")
	if err != nil {
		t.Fatalf("os.MkdirTemp(enforcement-work-) error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(clone) }) //nolint:errcheck // Best-effort cleanup.
	gitForTest(t, clone, "init", "--quiet", "--initial-branch=main")
	// A burst of commits otherwise starts a background repack that races the reads below.
	gitForTest(t, clone, "config", "gc.auto", "0")
	gitForTest(t, clone, "config", "maintenance.auto", "false")
	for i := range mainHistoryLength {
		if err := os.WriteFile(filepath.Join(clone, "counter"), []byte(strconv.Itoa(i)), 0o644); err != nil {
			t.Fatalf("writing counter error = %v", err)
		}
		gitForTest(t, clone, "add", "counter")
		gitForTest(t, clone, "commit", "--quiet", "-m", "commit "+strconv.Itoa(i))
	}
	f := &enforcementFixture{
		origin:     origin,
		repository: repository,
		mainCommit: gitForTest(t, clone, "rev-parse", "HEAD"),
		deepCommit: gitForTest(t, clone, "rev-parse", "HEAD~90"),
	}
	gitForTest(t, clone, "push", "--quiet", repository, "HEAD:refs/heads/main")

	gitForTest(t, clone, "switch", "--quiet", "--create", "feature")
	if err := os.WriteFile(filepath.Join(clone, "feature"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("writing feature error = %v", err)
	}
	gitForTest(t, clone, "add", "feature")
	gitForTest(t, clone, "commit", "--quiet", "-m", "feature")
	f.featureCommit = gitForTest(t, clone, "rev-parse", "HEAD")
	gitForTest(t, clone, "push", "--quiet", repository, "HEAD:refs/heads/feature")
	f.createRef(t, "refs/tags/v1.0.0", f.featureCommit)
	return f
}

func shrinkEnforcedFetchBackoff(t *testing.T, budget time.Duration) {
	t.Helper()
	saved := enforcedFetchBackoff
	enforcedFetchBackoff.base = time.Millisecond
	enforcedFetchBackoff.max = 4 * time.Millisecond
	enforcedFetchBackoff.budget = budget
	t.Cleanup(func() { enforcedFetchBackoff = saved })
}

func newEnforcementExecutor(t *testing.T, conf ExecutorConfig) *Executor {
	t.Helper()
	shrinkEnforcedFetchBackoff(t, time.Second)

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
		{name: "commit beyond the first shallow fetch", branch: "main", commit: f.deepCommit},
		{
			name:            "commit only on another branch",
			branch:          "main",
			commit:          f.featureCommit,
			wantErrContains: "is not on refs/heads/main",
		},
		{
			name:            "commit not reachable from its tag",
			branch:          "v1.0.0",
			tag:             "v1.0.0",
			commit:          strings.Repeat("ab", 20),
			wantErrContains: "is not on refs/tags/v1.0.0",
		},
		{
			name:            "tag vouching for a commit claimed as another branch",
			branch:          "main",
			tag:             "v1.0.0",
			commit:          f.featureCommit,
			wantErrContains: `claims branch "main"`,
		},
		{
			name:            "branch missing from the repository",
			branch:          "deleted-branch",
			commit:          f.mainCommit,
			wantErrContains: "the repository has no refs/heads/deleted-branch",
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

func TestFetchWithBackoff(t *testing.T) {
	errFlaky := errors.New("remote end hung up unexpectedly")

	for _, tt := range []struct {
		name         string
		budget       time.Duration
		failures     int
		missingRef   bool
		wantErr      string
		wantAttempts int
	}{
		{name: "succeeds first time", budget: time.Second, wantAttempts: 1},
		{name: "rides out a flaky remote", budget: time.Second, failures: 5, wantAttempts: 6},
		{name: "gives up when the budget runs out", budget: 20 * time.Millisecond, failures: 1 << 30, wantErr: "still failing after retrying"},
		{name: "stops early on a ref the remote lacks", budget: time.Second, failures: 1 << 30, missingRef: true, wantErr: "the repository has no refs/heads/gone", wantAttempts: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shrinkEnforcedFetchBackoff(t, tt.budget)
			e := New(ExecutorConfig{})
			e.shell = shell.NewTestShell(t)

			attempts := 0
			err := e.fetchWithBackoff(t.Context(), "refs/heads/gone", func(smells map[string]bool) error {
				attempts++
				if attempts > tt.failures {
					return nil
				}
				if tt.missingRef {
					smells[gitErrStrBadReference] = true
				}
				return errFlaky
			})

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("e.fetchWithBackoff(...) = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, errFlaky) {
				t.Fatalf("e.fetchWithBackoff(...) = %v, want an error containing %q that wraps the fetch error", err, tt.wantErr)
			}
			if tt.wantAttempts != 0 && attempts != tt.wantAttempts {
				t.Errorf("fetch attempts = %d, want %d", attempts, tt.wantAttempts)
			}
		})
	}
}

func TestFetchWithBackoffStopsWhenCancelled(t *testing.T) {
	shrinkEnforcedFetchBackoff(t, time.Hour)
	e := New(ExecutorConfig{})
	e.shell = shell.NewTestShell(t)

	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	err := e.fetchWithBackoff(ctx, "refs/heads/main", func(map[string]bool) error {
		attempts++
		if attempts == 3 {
			cancel()
		}
		return errors.New("connection reset")
	})
	if err == nil {
		t.Fatal("e.fetchWithBackoff(cancelled ctx, ...) = nil, want an error")
	}
	if attempts != 3 {
		t.Errorf("fetch attempts = %d, want 3 (none after the cancel)", attempts)
	}
}

func TestEnforceCommitVerificationOffDoesNothing(t *testing.T) {
	e := New(ExecutorConfig{Branch: "main", Commit: "not-a-commit"})
	e.shell = shell.NewTestShell(t)
	if err := e.enforceCommitVerification(t.Context()); err != nil {
		t.Errorf("e.enforceCommitVerification(ctx) with enforcement off = %v, want nil", err)
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
