package job

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/buildkite/agent/v3/env"
	"github.com/buildkite/agent/v3/internal/shell"
	"github.com/buildkite/roko"
)

// ErrCommitNotVerified means --enforce-git-commit-verification could not show
// that the job's commit is on its branch, or its tag on a tag build. Unlike
// --git-commit-verification, an inconclusive check fails the job too.
var ErrCommitNotVerified = errors.New("commit verification enforced")

const enforcedVerificationTipRef = "refs/commit-verification/tip"

// Anything else could name a ref (even enforcedVerificationTipRef itself)
// rather than a commit.
var hexObjectName = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// enforceCommitVerification runs before the checkout, a checkout hook, or a
// skipped checkout, so no job that reaches its command escapes it. It pins
// Commit to the full hash it verified, so the checkout can't resolve a short
// hash or HEAD to something else.
func (e *Executor) enforceCommitVerification(ctx context.Context) error {
	if !e.EnforceGitCommitVerification {
		return nil
	}

	e.shell.Headerf("Verifying the commit is on its branch")
	commit, tree, ref, err := e.verifyEnforcedCommit(ctx)
	if err != nil {
		// A failed job still runs local pre-exit hooks, which must not come from
		// an earlier job's files.
		if dirErr := e.useEmptyCheckoutDir(); dirErr != nil {
			e.shell.Warningf("Couldn't switch to an empty checkout directory: %v", dirErr)
		}
		return fmt.Errorf("%w: %w", ErrCommitNotVerified, err)
	}
	e.verifiedCommit, e.verifiedTree = commit, tree
	if e.Commit != commit {
		e.shell.Commentf("Building %s, the commit %q resolves to on %s", commit, e.Commit, ref)
		e.Commit = commit
		e.shell.Env.Set("BUILDKITE_COMMIT", commit)
	}
	e.shell.Commentf("Commit %s is on %s", commit, ref)
	return nil
}

func (e *Executor) verifyEnforcedCommit(ctx context.Context) (commit, tree, ref string, err error) {
	switch {
	case e.Repository == "":
		return "", "", "", errors.New("the job has no repository to verify its commit against")
	case e.Repository != e.canonicalRepository:
		return "", "", "", fmt.Errorf("a hook changed BUILDKITE_REPO from %q to %q", e.canonicalRepository, e.Repository)
	case e.Commit != "HEAD" && !hexObjectName.MatchString(e.Commit):
		return "", "", "", fmt.Errorf("commit %q is not a commit hash", e.Commit)
	}
	switch branch := strings.TrimPrefix(e.Branch, "refs/heads/"); {
	case e.Tag != "":
		// A tag build's branch is its tag, so any other branch would let the tag
		// vouch for a commit on a branch it says nothing about.
		if branch != "" && branch != e.Tag {
			return "", "", "", fmt.Errorf("the job is a build of tag %q but claims branch %q", e.Tag, e.Branch)
		}
		ref = "refs/tags/" + e.Tag
	case branch != "":
		ref = "refs/heads/" + branch
	default:
		return "", "", "", errors.New("the job has no branch or tag to verify its commit against")
	}
	commit, tree, err = e.verifyCommitReachableFrom(ctx, ref)
	return commit, tree, ref, err
}

// verifyCommitReachableFrom answers in a throwaway bare repository so that it
// works without a checkout and trusts nothing an earlier job could have left
// on the host. It returns the full commit hash and its tree.
func (e *Executor) verifyCommitReachableFrom(ctx context.Context, ref string) (commit, tree string, err error) {
	if err := e.shell.Command("git", "check-ref-format", ref).Run(ctx); err != nil {
		return "", "", fmt.Errorf("%q is not a valid ref", ref)
	}

	gitDir, err := os.MkdirTemp(e.BuildPath, "commit-verification-")
	if err != nil {
		return "", "", fmt.Errorf("creating a repository to verify in: %w", err)
	}
	defer os.RemoveAll(gitDir) //nolint:errcheck // Best-effort cleanup of a temporary directory.

	isolation := shell.WithExtraEnv(env.FromMap(map[string]string{
		// The global config is job-writable on a reused host, and a planted
		// url.insteadOf would point the fetch at another repository.
		"GIT_CONFIG_GLOBAL":      os.DevNull,
		"GIT_NO_REPLACE_OBJECTS": "1",
		// Otherwise resolving a commit the fetch didn't bring asks the remote
		// for it, and a commit that exists only off the branch would resolve.
		"GIT_NO_LAZY_FETCH": "1",
	}))
	gitArgs := append([]string{"--git-dir", gitDir, "-c", "core.commitGraph=false"}, e.enforcedCredentialConfig(ctx)...)
	git := func(args ...string) shell.Command {
		return e.shell.Command("git", append(append([]string{}, gitArgs...), args...)...)
	}

	if err := git("init", "--quiet", "--bare").Run(ctx, isolation); err != nil {
		return "", "", fmt.Errorf("initialising a repository to verify in: %w", err)
	}
	if err := git("remote", "add", "origin", e.Repository).Run(ctx, isolation); err != nil {
		return "", "", fmt.Errorf("adding the repository as a remote: %w", err)
	}

	if e.SSHKeyscan && !e.skipRepositorySSHKeyscan {
		addRepositoryHostToSSHKnownHosts(ctx, e.shell, e.Repository)
	}
	sshKeyPath, cleanupSSHKey, err := e.prepareGitSSHKey()
	if err != nil {
		return "", "", fmt.Errorf("preparing git ssh key: %w", err)
	}
	if cleanupSSHKey != nil {
		defer func() {
			if err := cleanupSSHKey(); err != nil {
				e.shell.Warningf("Couldn't clean up git ssh key %q: %v", sshKeyPath, err)
			}
		}()
	}

	refspec := "+" + ref + ":" + enforcedVerificationTipRef
	// Most builds are of a commit near the tip, so start shallow and fetch more
	// history only while the commit hasn't been found. Commits only: the tree
	// id comes from the commit object.
	for i, history := range []string{"--depth=64", "--deepen=1024", "--unshallow"} {
		fetch := func(smells map[string]bool) error {
			return git("fetch", "--no-tags", "--no-write-fetch-head", "--filter=tree:0", history, "--", "origin", refspec).
				Run(ctx, isolation, shell.WithStringSearch(smells))
		}
		if err := e.fetchWithBackoff(ctx, ref, fetch); err != nil {
			return "", "", fmt.Errorf("couldn't fetch %s from the repository: %w", ref, err)
		}

		commit, err = e.commitOnTip(ctx, git, isolation)
		if err != nil {
			return "", "", err
		}
		if commit != "" {
			break
		}

		shallow, err := git("rev-parse", "--is-shallow-repository").RunAndCaptureStdout(ctx, isolation)
		if err != nil {
			return "", "", fmt.Errorf("couldn't tell whether the history of %s is complete: %w", ref, err)
		}
		// Only complete history proves the commit isn't on the ref.
		if strings.TrimSpace(shallow) != "true" {
			return "", "", fmt.Errorf("commit %s is not on %s", e.Commit, ref)
		}
		if i == 2 {
			return "", "", fmt.Errorf("couldn't fetch the full history of %s to look for commit %s", ref, e.Commit)
		}
	}

	raw, err := git("cat-file", "commit", commit).RunAndCaptureStdout(ctx, isolation)
	if err != nil {
		return "", "", fmt.Errorf("couldn't read commit %s: %w", commit, err)
	}
	tree, found := strings.CutPrefix(strings.SplitN(raw, "\n", 2)[0], "tree ")
	if !found || !hexObjectName.MatchString(tree) {
		return "", "", fmt.Errorf("commit %s has no tree", commit)
	}
	return commit, tree, nil
}

// commitOnTip returns the full hash of the job's commit if it is reachable from
// the fetched tip, or "" if the history fetched so far doesn't show it.
func (e *Executor) commitOnTip(ctx context.Context, git func(...string) shell.Command, isolation shell.RunCommandOpt) (string, error) {
	tip, err := git("rev-parse", "--verify", "--quiet", enforcedVerificationTipRef+"^{commit}").RunAndCaptureStdout(ctx, isolation)
	if err != nil {
		return "", errors.New("the fetched ref does not point at a commit")
	}
	tip = strings.TrimSpace(tip)
	if e.Commit == "HEAD" {
		return tip, nil
	}

	commit, err := git("rev-parse", "--verify", "--quiet", e.Commit+"^{commit}").RunAndCaptureStdout(ctx, isolation)
	if err != nil {
		return "", nil
	}
	commit = strings.TrimSpace(commit)
	err = git("merge-base", "--is-ancestor", commit, tip).Run(ctx, isolation)
	switch {
	case err == nil:
		return commit, nil
	case shell.IsExitError(err) && shell.ExitCode(err) == 1:
		return "", nil
	default:
		return "", fmt.Errorf("couldn't check whether commit %s is on the fetched ref: %w", e.Commit, err)
	}
}

// enforcedCredentialConfig recreates the agent's own credential setup, which
// it writes to the global config that the verification ignores.
func (e *Executor) enforcedCredentialConfig(ctx context.Context) []string {
	if !e.useRepositoryProviderGitCredentials() {
		return nil
	}
	args := []string{"-c", "credential.useHttpPath=true", "-c", "credential.helper=" + gitCredentialHelperCommand(ctx)}
	if e.skipRepositorySSHKeyscan || e.shell.Env.GetString("BUILDKITE_USE_GITHUB_APP_GIT_CREDENTIALS", "") == "true" {
		args = append(args, "-c", "url.https://github.com/.insteadOf=git@github.com:")
	}
	return args
}

// enforcedFetchBackoff spreads retries over about ten minutes, so a job rides
// out a GitHub incident rather than failing, and jitter keeps a fleet of
// waiting agents from retrying in lockstep. Vars, not consts, so tests can
// shrink them.
var enforcedFetchBackoff = struct {
	base, max, budget  time.Duration
	missingRefAttempts int
}{base: 2 * time.Second, max: time.Minute, budget: 10 * time.Minute, missingRefAttempts: 3}

// fetchWithBackoff retries until the budget runs out, except that a ref the
// remote says it lacks gets only a few attempts: that answer is nearly always
// a deleted branch, which waiting won't bring back.
func (e *Executor) fetchWithBackoff(ctx context.Context, ref string, fetch func(smells map[string]bool) error) error {
	b := enforcedFetchBackoff
	deadline := time.Now().Add(b.budget)
	missingRef := 0
	return roko.NewRetrier(
		roko.TryForever(),
		roko.WithStrategy(roko.Constant(b.base)),
	).DoWithContext(ctx, func(r *roko.Retrier) error {
		smells := map[string]bool{gitErrStrBadReference: false, gitErrStrBadReferencePreGit221: false}
		err := fetch(smells)
		if err == nil || ctx.Err() != nil {
			if ctx.Err() != nil {
				r.Break()
			}
			return err
		}

		step := min(b.max, b.base<<min(r.AttemptCount(), 30))
		wait := step/2 + time.Duration(rand.Int64N(int64(step/2)+1))
		if smells[gitErrStrBadReference] || smells[gitErrStrBadReferencePreGit221] {
			missingRef++
			if missingRef >= b.missingRefAttempts {
				r.Break()
				return fmt.Errorf("the repository has no %s: %w", ref, err)
			}
		}
		if time.Now().Add(wait).After(deadline) {
			r.Break()
			return fmt.Errorf("still failing after retrying for %s: %w", b.budget, err)
		}
		r.SetNextInterval(wait)
		e.shell.Warningf("Couldn't fetch %s to verify the commit (attempt %d), retrying in %s: %v", ref, r.AttemptCount()+1, wait.Round(time.Second), err)
		return err
	})
}

// prepareEnforcedCheckoutDir gives a job that won't run the default checkout an
// empty directory of its own, so neither it nor local hooks can run an earlier
// job's files, while the shared checkout (a seeded tree, say) survives.
func (e *Executor) prepareEnforcedCheckoutDir() error {
	defaultCheckout := !e.SkipCheckout && !e.hasPluginHook("checkout") && !e.hasGlobalHook("checkout")
	if !e.EnforceGitCommitVerification || defaultCheckout {
		return nil
	}
	return e.useEmptyCheckoutDir()
}

func (e *Executor) useEmptyCheckoutDir() error {
	dir, err := os.MkdirTemp(e.BuildPath, "checkout-")
	if err != nil {
		return err
	}
	e.cleanupDirs = append(e.cleanupDirs, dir)
	e.shell.Commentf("Using an empty checkout directory, %s, so no earlier job's files can run", dir)
	e.shell.Env.Set("BUILDKITE_BUILD_CHECKOUT_PATH", dir)
	return e.createCheckoutDir()
}

// assertCheckoutIsVerifiedCommit catches a checkout hook that checked out
// something else, and a reused .git whose replace refs, grafts or planted
// commit and tree objects made the checkout produce different files.
func (e *Executor) assertCheckoutIsVerifiedCommit(ctx context.Context) error {
	if !e.EnforceGitCommitVerification || e.SkipCheckout {
		return nil
	}
	err := e.checkoutMatchesVerifiedCommit(ctx)
	if err != nil {
		// The checkout itself is untrustworthy now, and would fail every later
		// job the same way, so remove it rather than set it aside.
		e.discardCheckoutDir()
		return fmt.Errorf("%w: %w", ErrCommitNotVerified, err)
	}
	return nil
}

func (e *Executor) checkoutMatchesVerifiedCommit(ctx context.Context) error {
	checkoutPath, _ := e.shell.Env.Get("BUILDKITE_BUILD_CHECKOUT_PATH")
	if _, err := os.Stat(filepath.Join(checkoutPath, ".git")); err != nil {
		entries, readErr := os.ReadDir(checkoutPath)
		if readErr == nil && len(entries) == 0 {
			return nil
		}
		return fmt.Errorf("the checkout left files in %s that aren't a git checkout of %s", checkoutPath, e.verifiedCommit)
	}

	head, err := e.shell.Command("git", "-C", checkoutPath, "rev-parse", "--verify", "--quiet", "HEAD").RunAndCaptureStdout(ctx)
	if err != nil || strings.TrimSpace(head) != e.verifiedCommit {
		return fmt.Errorf("the checkout is at %q, not the verified commit %s", strings.TrimSpace(head), e.verifiedCommit)
	}
	// write-tree hashes the index from its entries, so it reflects what was
	// actually checked out, whatever the object store claims.
	tree, err := e.shell.Command("git", "-C", checkoutPath, "write-tree").RunAndCaptureStdout(ctx)
	if err != nil || strings.TrimSpace(tree) != e.verifiedTree {
		return fmt.Errorf("the checked-out files are tree %q, not the verified commit's tree %s", strings.TrimSpace(tree), e.verifiedTree)
	}
	return nil
}

func (e *Executor) discardCheckoutDir() {
	if err := e.removeCheckoutDir(); err != nil {
		e.shell.Warningf("Couldn't remove the checkout directory after commit verification failed: %v", err)
	}
}
