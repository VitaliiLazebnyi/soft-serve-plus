package gitserve

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"charm.land/log/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
)

// EnsureWithin ensures the given repo is within the repos directory.
func EnsureWithin(reposDir string, repo string) error {
	repoDir := filepath.Join(reposDir, repo)
	absRepos, err := filepath.Abs(reposDir)
	if err != nil {
		log.Debugf("failed to get absolute path for repos: %s", err)
		return ErrSystemMalfunction
	}
	absRepo, err := filepath.Abs(repoDir)
	if err != nil {
		log.Debugf("failed to get absolute path for repo: %s", err)
		return ErrSystemMalfunction
	}

	// ensure the repo is within the repos directory. A plain prefix check
	// would let "/d/repos" accept a sibling such as "/d/repos2".
	rel, err := filepath.Rel(absRepos, absRepo)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		log.Debugf("repo path is outside of repos directory: %s", absRepo)
		return ErrInvalidRepo
	}

	return nil
}

// EnsureDefaultBranch ensures the repo has a default branch.
// It will prefer choosing "main" or "master" if available. A repo without
// branches, such as one that was only pushed tags or had its last branch
// deleted, is left as is: there is nothing to point HEAD at yet.
func EnsureDefaultBranch(ctx context.Context, repoPath string) error {
	r, err := git.Open(ctx, repoPath)
	if err != nil {
		return err
	}
	// HEAD resolves after almost every push, so look at it before listing
	// the branches, of which there can be many.
	_, headErr := r.HEAD()
	if headErr == nil {
		return nil
	}
	// Not r.Branches: git show-ref fails when nothing matches, which would
	// make a real failure indistinguishable from having no branches.
	out, err := gitm.NewCommandWithContext(ctx, "for-each-ref", "--format=%(refname)", git.RefsHeads).RunInDir(repoPath)
	if err != nil {
		return err
	}
	var brs []string
	for _, ref := range strings.Fields(string(out)) {
		brs = append(brs, strings.TrimPrefix(ref, git.RefsHeads))
	}
	if len(brs) == 0 {
		return nil
	}
	if !errors.Is(headErr, git.ErrReferenceNotExist) {
		return headErr
	}
	// Point HEAD at the first branch available, preferring "main" or
	// "master".
	branch := brs[0]
	for _, b := range brs {
		if b == "main" || b == "master" {
			branch = b
			break
		}
	}
	_, err = r.SymbolicRef(git.HEAD, git.RefsHeads+branch)
	return err
}
