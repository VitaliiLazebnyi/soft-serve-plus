package git

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"charm.land/log/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
)

// WritePktline encodes and writes a pktline to the given writer.
func WritePktline(w io.Writer, v ...interface{}) error {
	msg := fmt.Sprintln(v...)
	pkt := pktline.NewEncoder(w)
	if err := pkt.EncodeString(msg); err != nil {
		return fmt.Errorf("git: error writing pkt-line message: %w", err)
	}
	if err := pkt.Flush(); err != nil {
		return fmt.Errorf("git: error flushing pkt-line message: %w", err)
	}

	return nil
}

// WritePktlineErr writes an error pktline to the given writer.
func WritePktlineErr(w io.Writer, err error) error {
	return WritePktline(w, "ERR", err.Error())
}

// EnsureWithin ensures the given repo is within the repos directory.
func EnsureWithin(reposDir string, repo string) error {
	repoDir := filepath.Join(reposDir, repo)
	absRepos, err := filepath.Abs(reposDir)
	if err != nil {
		log.Debugf("failed to get absolute path for repo: %s", err)
		return ErrSystemMalfunction
	}
	absRepo, err := filepath.Abs(repoDir)
	if err != nil {
		log.Debugf("failed to get absolute path for repos: %s", err)
		return ErrSystemMalfunction
	}

	// ensure the repo is within the repos directory
	if !strings.HasPrefix(absRepo, absRepos) {
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
	r, err := git.Open(repoPath)
	if err != nil {
		return err
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
	// Rename the default branch to the first branch available
	_, err = r.HEAD()
	if err == git.ErrReferenceNotExist {
		branch := brs[0]
		// Prefer "main" or "master" as the default branch
		for _, b := range brs {
			if b == "main" || b == "master" {
				branch = b
				break
			}
		}

		if _, err := r.SymbolicRef(git.HEAD, git.RefsHeads+branch, gitm.SymbolicRefOptions{
			CommandOptions: gitm.CommandOptions{
				Context: ctx,
			},
		}); err != nil {
			return err
		}
	}
	if err != nil && err != git.ErrReferenceNotExist {
		return err
	}
	return nil
}
