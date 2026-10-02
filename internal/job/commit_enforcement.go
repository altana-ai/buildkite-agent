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
	"github.com/buildkite/shellwords"
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
		// A failed job still runs the checkout's pre-exit hook, which must not
		// come from an earlier job's files.
		e.discardCheckoutDir()
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
		"GIT_GRAFT_FILE":         os.DevNull,
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
	if err := e.borrowObjects(gitDir); err != nil {
		return "", "", err
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
	if err := e.fetchWithBackoff(ctx, ref, func(smells map[string]bool) error {
		return git("fetch", "--no-tags", "--no-write-fetch-head", "--filter=tree:0", "--", "origin", refspec).
			Run(ctx, isolation, shell.WithStringSearch(smells))
	}); err != nil {
		return "", "", fmt.Errorf("couldn't fetch %s from the repository: %w", ref, err)
	}

	tip, err := git("rev-parse", "--verify", "--quiet", enforcedVerificationTipRef+"^{commit}").RunAndCaptureStdout(ctx, isolation)
	if err != nil {
		return "", "", fmt.Errorf("%s does not point at a commit", ref)
	}
	tip = strings.TrimSpace(tip)
	if e.Commit == "HEAD" {
		commit = tip
	} else {
		commit, err = git("rev-parse", "--verify", "--quiet", e.Commit+"^{commit}").RunAndCaptureStdout(ctx, isolation)
		if err != nil {
			return "", "", fmt.Errorf("commit %s is not in the history of %s", e.Commit, ref)
		}
		commit = strings.TrimSpace(commit)
	}

	err = git("merge-base", "--is-ancestor", commit, tip).Run(ctx, isolation)
	switch {
	case shell.IsExitError(err) && shell.ExitCode(err) == 1:
		return "", "", fmt.Errorf("commit %s is not on %s", e.Commit, ref)
	case err != nil:
		return "", "", fmt.Errorf("couldn't check whether commit %s is on %s: %w", e.Commit, ref, err)
	}

	tree, err = git("rev-parse", "--verify", "--quiet", commit+"^{tree}").RunAndCaptureStdout(ctx, isolation)
	if err != nil {
		return "", "", fmt.Errorf("couldn't read the tree of commit %s: %w", commit, err)
	}
	return commit, strings.TrimSpace(tree), nil
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

// borrowObjects keeps the fetch small on a host that already holds most of the
// history. It borrows only stores this job can't write: git doesn't re-hash an
// object it reads, so a store an earlier job could write could hold a forged
// commit or commit-graph that fakes ancestry.
func (e *Executor) borrowObjects(gitDir string) error {
	var sources []string
	if e.GitMirrorsPath != "" {
		sources = append(sources, filepath.Join(e.GitMirrorsPath, dirForRepository(e.Repository)))
	}
	sources = append(sources, referenceRepositories(e.GitCloneFlags)...)

	var alternates []string
	for _, source := range sources {
		for _, objects := range []string{filepath.Join(source, "objects"), filepath.Join(source, ".git", "objects")} {
			info, err := os.Stat(objects)
			if err != nil || !info.IsDir() {
				continue
			}
			if objectStoreIsWritable(objects) {
				e.shell.Commentf("Not borrowing objects from %s: this job could have written to it", objects)
			} else {
				alternates = append(alternates, objects)
			}
			break
		}
	}
	if len(alternates) == 0 {
		return nil
	}
	path := filepath.Join(gitDir, "objects", "info", "alternates")
	if err := os.WriteFile(path, []byte(strings.Join(alternates, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// objectStoreIsWritable reports whether this process can add a file anywhere
// git would read objects, packs, alternates or a commit-graph from. Probing by
// creating a file answers for every platform and every way a directory can be
// writable.
func objectStoreIsWritable(objects string) bool {
	dirs := []string{objects}
	entries, err := os.ReadDir(objects)
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(objects, entry.Name()))
		}
	}
	for _, dir := range dirs {
		probe, err := os.CreateTemp(dir, ".commit-verification-probe-")
		if err == nil {
			probe.Close()           //nolint:errcheck // Only the create mattered.
			os.Remove(probe.Name()) //nolint:errcheck // Best-effort cleanup.
			return true
		}
	}
	return false
}

// prepareEnforcedCheckoutDir empties the checkout directory when the job will
// not run the default checkout, so neither the job nor the checkout's local
// hooks can run an earlier job's files.
func (e *Executor) prepareEnforcedCheckoutDir() error {
	defaultCheckout := !e.SkipCheckout && !e.hasPluginHook("checkout") && !e.hasGlobalHook("checkout")
	if !e.EnforceGitCommitVerification || defaultCheckout {
		return nil
	}
	e.shell.Commentf("Emptying the checkout directory: this job doesn't run the default checkout")
	return e.removeCheckoutDir()
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

// referenceRepositories returns the --reference and --reference-if-able paths
// in the configured clone flags.
func referenceRepositories(cloneFlags string) []string {
	flags, err := shellwords.Split(cloneFlags)
	if err != nil {
		return nil
	}
	var paths []string
	for i := 0; i < len(flags); i++ {
		for _, name := range []string{"--reference", "--reference-if-able"} {
			if flags[i] == name && i+1 < len(flags) {
				paths = append(paths, flags[i+1])
				i++
				break
			}
			if value, ok := strings.CutPrefix(flags[i], name+"="); ok {
				paths = append(paths, value)
				break
			}
		}
	}
	return paths
}
