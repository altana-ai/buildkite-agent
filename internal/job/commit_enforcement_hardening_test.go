package job

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests that --enforce-git-commit-verification holds against an attacker who
// controls the build's branch and commit, a hook, or an earlier job's files on
// a reused host.

func gitForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s error = %v, output: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newCheckoutPathForTest returns a checkout directory registered with e, and
// cleans up after it.
func newCheckoutPathForTest(t *testing.T, e *Executor) string {
	t.Helper()
	checkoutPath, err := os.MkdirTemp("", "checkout-path-")
	if err != nil {
		t.Fatalf("os.MkdirTemp(checkout-path-) error = %v", err)
	}
	t.Cleanup(func() {
		if e.checkoutRoot != nil {
			_ = e.checkoutRoot.Close()
		}
		os.RemoveAll(checkoutPath) //nolint:errcheck // Best-effort cleanup.
	})
	e.shell.Env.Set("BUILDKITE_BUILD_CHECKOUT_PATH", checkoutPath)
	// Otherwise the checkout asks the agent API about commit meta-data, and there
	// is no API in a test.
	e.shell.Env.Set("BUILDKITE_COMMIT_RESOLVED", "true")
	return checkoutPath
}

func defaultCheckoutConfig(f *enforcementFixture, commit string) ExecutorConfig {
	return ExecutorConfig{
		Repository:       f.repository,
		Branch:           "main",
		Commit:           commit,
		GitCheckoutFlags: "-f",
		GitCleanFlags:    "-ffxdq",
		GitFetchFlags:    "-v --prune",
		PullRequest:      "false",
		CheckoutAttempts: 1,
	}
}

func TestEnforcedVerificationPinsTheCommit(t *testing.T) {
	f := newEnforcementFixture(t)

	for _, tt := range []struct {
		name, commit string
	}{
		{name: "HEAD is pinned to the verified branch tip", commit: "HEAD"},
		{name: "an abbreviated hash is pinned to the full hash", commit: f.mainCommit[:10]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnforcementExecutor(t, ExecutorConfig{Repository: f.repository, Branch: "main", Commit: tt.commit})
			if err := e.enforceCommitVerification(t.Context()); err != nil {
				t.Fatalf("e.enforceCommitVerification(ctx) = %v, want nil", err)
			}
			if e.Commit != f.mainCommit {
				t.Errorf("e.Commit = %q after verification, want the full hash %q", e.Commit, f.mainCommit)
			}
			if got, _ := e.shell.Env.Get("BUILDKITE_COMMIT"); got != f.mainCommit {
				t.Errorf("BUILDKITE_COMMIT = %q after verification, want %q", got, f.mainCommit)
			}
		})
	}
}

// F7: a tag sharing the branch's name must not stand in for the branch.
func TestEnforcedVerificationIgnoresTagShadowingTheBranch(t *testing.T) {
	f := newEnforcementFixture(t)
	f.createRef(t, "refs/tags/main", f.featureCommit)

	e := newEnforcementExecutor(t, ExecutorConfig{Repository: f.repository, Branch: "main", Commit: "HEAD"})
	if err := e.enforceCommitVerification(t.Context()); err != nil {
		t.Fatalf("e.enforceCommitVerification(ctx) = %v, want nil", err)
	}
	if e.Commit != f.mainCommit {
		t.Errorf("HEAD of branch main pinned to %q, want the branch tip %q, not the tag's commit", e.Commit, f.mainCommit)
	}

	e = newEnforcementExecutor(t, ExecutorConfig{Repository: f.repository, Branch: "main", Commit: f.featureCommit})
	if err := e.enforceCommitVerification(t.Context()); !errors.Is(err, ErrCommitNotVerified) {
		t.Errorf("e.enforceCommitVerification(ctx) for a commit only under tag main = %v, want ErrCommitNotVerified", err)
	}
}

func TestEnforcedVerificationRejectsARepositorySwappedByAHook(t *testing.T) {
	f := newEnforcementFixture(t)
	e := newEnforcementExecutor(t, ExecutorConfig{Repository: f.repository, Branch: "main", Commit: f.mainCommit})
	e.Repository = f.repository + "-elsewhere"

	err := e.enforceCommitVerification(t.Context())
	if !errors.Is(err, ErrCommitNotVerified) || !strings.Contains(err.Error(), "a hook changed BUILDKITE_REPO") {
		t.Errorf("e.enforceCommitVerification(ctx) after BUILDKITE_REPO changed = %v, want it to fail naming the change", err)
	}
}

// A global config is job-writable on a reused host, and could redirect the
// verification fetch to a repository the attacker controls.
func TestEnforcedVerificationIgnoresTheGlobalGitConfig(t *testing.T) {
	f := newEnforcementFixture(t)
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	content := "[url \"http://127.0.0.1:1/nowhere.git\"]\n\tinsteadOf = " + f.repository + "\n"
	if err := os.WriteFile(globalConfig, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", globalConfig, err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	e := newEnforcementExecutor(t, ExecutorConfig{Repository: f.repository, Branch: "main", Commit: f.mainCommit})
	if err := e.enforceCommitVerification(t.Context()); err != nil {
		t.Errorf("e.enforceCommitVerification(ctx) with a redirecting global config = %v, want nil (the config is ignored)", err)
	}
}

// F5: a replace ref left in a reused checkout makes `git checkout <commit>`
// write another commit's files while HEAD still names the verified commit.
func TestCheckoutPhaseCatchesAReplaceRefInAReusedCheckout(t *testing.T) {
	f := newEnforcementFixture(t)

	e := newEnforcementExecutor(t, defaultCheckoutConfig(f, f.mainCommit))
	checkoutPath := newCheckoutPathForTest(t, e)
	gitForTest(t, checkoutPath, "clone", "--quiet", "--", f.repository, ".")
	gitForTest(t, checkoutPath, "fetch", "--quiet", "origin", "feature")
	gitForTest(t, checkoutPath, "replace", f.mainCommit, f.featureCommit)

	err := e.CheckoutPhase(t.Context())
	if !errors.Is(err, ErrCommitNotVerified) || !strings.Contains(err.Error(), "not the verified commit's tree") {
		t.Fatalf("e.CheckoutPhase(ctx) over a replace ref = %v, want it to fail on the checked-out tree", err)
	}
	if _, statErr := os.Stat(checkoutPath); !os.IsNotExist(statErr) {
		t.Errorf("checkout directory still exists after the check failed (stat error = %v), want it removed", statErr)
	}
}

func TestCheckoutPhaseAcceptsAnHonestDefaultCheckout(t *testing.T) {
	f := newEnforcementFixture(t)
	e := newEnforcementExecutor(t, defaultCheckoutConfig(f, "HEAD"))
	checkoutPath := newCheckoutPathForTest(t, e)

	if err := e.CheckoutPhase(t.Context()); err != nil {
		t.Fatalf("e.CheckoutPhase(ctx) = %v, want nil", err)
	}
	if head := gitForTest(t, checkoutPath, "rev-parse", "HEAD"); head != f.mainCommit {
		t.Errorf("checked out %q, want the verified tip %q", head, f.mainCommit)
	}
}

// F8: neither a skipped checkout nor a failed check may run an earlier job's
// files, including its local hooks. The job moves to an empty directory of its
// own, and the shared checkout (a seeded tree, say) is left for later jobs.
func TestCheckoutPhaseLeavesNoEarlierJobFiles(t *testing.T) {
	f := newEnforcementFixture(t)

	plantStaleHook := func(t *testing.T, checkoutPath string) string {
		t.Helper()
		hooks := filepath.Join(checkoutPath, ".buildkite", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%q) error = %v", hooks, err)
		}
		hook := filepath.Join(hooks, "pre-exit")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\necho stale\n"), 0o755); err != nil {
			t.Fatalf("writing a stale pre-exit hook error = %v", err)
		}
		return hook
	}
	assertMovedToEmptyDir := func(t *testing.T, e *Executor, shared, staleHook string) {
		t.Helper()
		now, _ := e.shell.Env.Get("BUILDKITE_BUILD_CHECKOUT_PATH")
		if now == shared {
			t.Fatalf("BUILDKITE_BUILD_CHECKOUT_PATH is still the shared checkout %s, want an empty directory of the job's own", shared)
		}
		if entries, err := os.ReadDir(now); err != nil || len(entries) != 0 {
			t.Errorf("the job's checkout directory %s holds %d entries (error %v), want it empty", now, len(entries), err)
		}
		if e.hasLocalHook("pre-exit") {
			t.Error("an earlier job's pre-exit hook is still findable from the job's checkout directory")
		}
		if _, err := os.Stat(staleHook); err != nil {
			t.Errorf("the shared checkout lost %s (stat error = %v), want it left alone", staleHook, err)
		}
	}

	t.Run("skipped checkout of a verified commit", func(t *testing.T) {
		conf := defaultCheckoutConfig(f, f.mainCommit)
		conf.SkipCheckout = true
		e := newEnforcementExecutor(t, conf)
		shared := newCheckoutPathForTest(t, e)
		staleHook := plantStaleHook(t, shared)

		if err := e.CheckoutPhase(t.Context()); err != nil {
			t.Fatalf("e.CheckoutPhase(ctx) = %v, want nil", err)
		}
		assertMovedToEmptyDir(t, e, shared, staleHook)
	})

	t.Run("failed check", func(t *testing.T) {
		e := newEnforcementExecutor(t, defaultCheckoutConfig(f, f.featureCommit))
		shared := newCheckoutPathForTest(t, e)
		staleHook := plantStaleHook(t, shared)

		if err := e.CheckoutPhase(t.Context()); !errors.Is(err, ErrCommitNotVerified) {
			t.Fatalf("e.CheckoutPhase(ctx) = %v, want ErrCommitNotVerified", err)
		}
		assertMovedToEmptyDir(t, e, shared, staleHook)
	})
}

// F2: a checkout hook runs instead of the default checkout, so it starts from an
// empty directory and what it leaves is checked against the verified commit.
// Running a real hook needs a built agent binary, so these tests set up what a
// hook would see and leave, and call the two steps around it.
func TestEnforcedCheckoutHookStartsEmpty(t *testing.T) {
	f := newEnforcementFixture(t)
	hooksPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooksPath, "checkout"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing the checkout hook error = %v", err)
	}
	conf := defaultCheckoutConfig(f, f.mainCommit)
	conf.HooksPath = hooksPath
	e := newEnforcementExecutor(t, conf)
	shared := newCheckoutPathForTest(t, e)
	if err := os.WriteFile(filepath.Join(shared, "stale"), []byte("from an earlier job\n"), 0o644); err != nil {
		t.Fatalf("planting a stale file error = %v", err)
	}

	if err := e.prepareEnforcedCheckoutDir(); err != nil {
		t.Fatalf("e.prepareEnforcedCheckoutDir() = %v, want nil", err)
	}
	now, _ := e.shell.Env.Get("BUILDKITE_BUILD_CHECKOUT_PATH")
	if entries, err := os.ReadDir(now); now == shared || err != nil || len(entries) != 0 {
		t.Errorf("checkout hook would start in %s with %d entries (error %v), want an empty directory other than %s", now, len(entries), err, shared)
	}
}

func TestEnforcedCheckoutChecksWhatACheckoutHookLeft(t *testing.T) {
	f := newEnforcementFixture(t)

	for _, tt := range []struct {
		name    string
		leave   func(t *testing.T, checkoutPath string)
		wantErr string
	}{
		{name: "nothing", leave: func(*testing.T, string) {}},
		{
			name: "the verified commit",
			leave: func(t *testing.T, checkoutPath string) {
				gitForTest(t, checkoutPath, "clone", "--quiet", "--", f.repository, ".")
			},
		},
		{
			name: "files of its own",
			leave: func(t *testing.T, checkoutPath string) {
				if err := os.WriteFile(filepath.Join(checkoutPath, "from-the-hook"), nil, 0o644); err != nil {
					t.Fatalf("writing a file error = %v", err)
				}
			},
			wantErr: "aren't a git checkout",
		},
		{
			name: "another commit",
			leave: func(t *testing.T, checkoutPath string) {
				gitForTest(t, checkoutPath, "clone", "--quiet", "--branch", "feature", "--", f.repository, ".")
			},
			wantErr: "not the verified commit " + f.mainCommit,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnforcementExecutor(t, defaultCheckoutConfig(f, f.mainCommit))
			checkoutPath := newCheckoutPathForTest(t, e)
			if err := e.enforceCommitVerification(t.Context()); err != nil {
				t.Fatalf("e.enforceCommitVerification(ctx) = %v, want nil", err)
			}
			tt.leave(t, checkoutPath)

			err := e.assertCheckoutIsVerifiedCommit(t.Context())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("e.assertCheckoutIsVerifiedCommit(ctx) = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrCommitNotVerified) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("e.assertCheckoutIsVerifiedCommit(ctx) = %v, want ErrCommitNotVerified containing %q", err, tt.wantErr)
			}
		})
	}
}
