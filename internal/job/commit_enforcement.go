package job

import (
	"context"
	"errors"
	"fmt"
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
// skipped checkout, so no job that reaches its command escapes it.
func (e *Executor) enforceCommitVerification(ctx context.Context) error {
	if !e.EnforceGitCommitVerification {
		return nil
	}
	if e.Commit == "HEAD" {
		e.shell.Commentf("Commit verification: the commit is HEAD, so the job builds the tip of %q", e.Branch)
		return nil
	}

	e.shell.Headerf("Verifying the commit is on its branch")
	ref, err := e.enforcedVerificationRef()
	if err == nil {
		err = e.verifyCommitReachableFrom(ctx, ref)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCommitNotVerified, err)
	}
	e.shell.Commentf("Commit %s is on %s", e.Commit, ref)
	return nil
}

// enforcedVerificationRef uses the backend's branch and tag, which no hook can
// rewrite, so it verifies the build that was requested.
func (e *Executor) enforcedVerificationRef() (string, error) {
	switch {
	case e.Repository == "":
		return "", errors.New("the job has no repository to verify its commit against")
	case !hexObjectName.MatchString(e.Commit):
		return "", fmt.Errorf("commit %q is not a commit hash", e.Commit)
	case e.Tag != "":
		return "refs/tags/" + e.Tag, nil
	}
	branch := strings.TrimPrefix(e.Branch, "refs/heads/")
	if branch == "" {
		return "", errors.New("the job has no branch or tag to verify its commit against")
	}
	return "refs/heads/" + branch, nil
}

// verifyCommitReachableFrom answers in a throwaway bare repository so that it
// works without a checkout. It fetches only commits, borrowing objects from any
// mirror or reference repository the checkout would also use.
func (e *Executor) verifyCommitReachableFrom(ctx context.Context, ref string) error {
	if err := e.shell.Command("git", "check-ref-format", ref).Run(ctx); err != nil {
		return fmt.Errorf("%q is not a valid ref", ref)
	}

	gitDir, err := os.MkdirTemp(e.BuildPath, "commit-verification-")
	if err != nil {
		return fmt.Errorf("creating a repository to verify in: %w", err)
	}
	defer os.RemoveAll(gitDir) //nolint:errcheck // Best-effort cleanup of a temporary directory.

	git := func(args ...string) shell.Command {
		return e.shell.Command("git", append([]string{"--git-dir", gitDir}, args...)...)
	}
	if err := git("init", "--quiet", "--bare").Run(ctx); err != nil {
		return fmt.Errorf("initialising a repository to verify in: %w", err)
	}
	if err := e.borrowObjects(gitDir); err != nil {
		return err
	}
	if err := git("remote", "add", "origin", e.Repository).Run(ctx); err != nil {
		return fmt.Errorf("adding the repository as a remote: %w", err)
	}

	if e.SSHKeyscan && !e.skipRepositorySSHKeyscan {
		addRepositoryHostToSSHKnownHosts(ctx, e.shell, e.Repository)
	}
	sshKeyPath, cleanupSSHKey, err := e.prepareGitSSHKey()
	if err != nil {
		return fmt.Errorf("preparing git ssh key: %w", err)
	}
	if cleanupSSHKey != nil {
		defer func() {
			if err := cleanupSSHKey(); err != nil {
				e.shell.Warningf("Couldn't clean up git ssh key %q: %v", sshKeyPath, err)
			}
		}()
	}

	refspec := "+" + ref + ":" + enforcedVerificationTipRef
	if err := roko.NewRetrier(
		roko.WithMaxAttempts(3),
		roko.WithStrategy(roko.Exponential(2*time.Second, 0)),
		roko.WithJitter(),
	).DoWithContext(ctx, func(*roko.Retrier) error {
		return git("fetch", "--no-tags", "--no-write-fetch-head", "--filter=tree:0", "--", "origin", refspec).Run(ctx)
	}); err != nil {
		return fmt.Errorf("couldn't fetch %s from the repository: %w", ref, err)
	}

	// Without this, resolving a commit the fetch didn't bring asks the remote
	// for it, and a commit that exists only off the branch would then resolve.
	noLazyFetch := shell.WithExtraEnv(env.FromMap(map[string]string{"GIT_NO_LAZY_FETCH": "1"}))

	tip, err := git("rev-parse", "--verify", "--quiet", enforcedVerificationTipRef+"^{commit}").RunAndCaptureStdout(ctx, noLazyFetch)
	if err != nil {
		return fmt.Errorf("%s does not point at a commit", ref)
	}
	commit, err := git("rev-parse", "--verify", "--quiet", e.Commit+"^{commit}").RunAndCaptureStdout(ctx, noLazyFetch)
	if err != nil {
		return fmt.Errorf("commit %s is not in the history of %s", e.Commit, ref)
	}

	err = git("merge-base", "--is-ancestor", strings.TrimSpace(commit), strings.TrimSpace(tip)).Run(ctx, noLazyFetch)
	switch {
	case err == nil:
		return nil
	case shell.IsExitError(err) && shell.ExitCode(err) == 1:
		return fmt.Errorf("commit %s is not on %s", e.Commit, ref)
	default:
		return fmt.Errorf("couldn't check whether commit %s is on %s: %w", e.Commit, ref, err)
	}
}

// borrowObjects keeps the fetch small on a host that already holds most of the
// history, which is what makes verifying every job affordable.
func (e *Executor) borrowObjects(gitDir string) error {
	var sources []string
	if e.GitMirrorsPath != "" {
		sources = append(sources, filepath.Join(e.GitMirrorsPath, dirForRepository(e.Repository)))
	}
	sources = append(sources, referenceRepositories(e.GitCloneFlags)...)
	if checkoutPath, ok := e.shell.Env.Get("BUILDKITE_BUILD_CHECKOUT_PATH"); ok && checkoutPath != "" {
		sources = append(sources, checkoutPath)
	}

	var alternates []string
	for _, source := range sources {
		for _, objects := range []string{filepath.Join(source, "objects"), filepath.Join(source, ".git", "objects")} {
			if info, err := os.Stat(objects); err == nil && info.IsDir() {
				alternates = append(alternates, objects)
				break
			}
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
