package git

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aymanbagabas/git-module"
)

// Branches returns the names of the repository's branches, none for a
// repository without any.
func (r *Repository) Branches() ([]string, error) {
	heads, err := r.gm.ShowRef(git.ShowRefOptions{Heads: true, CommandOptions: git.CommandOptions{Context: r.ctx}})
	if err != nil {
		// As in References: git show-ref exits with status 1 and prints
		// nothing when nothing matches.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return []string{}, nil
		}
		return nil, err
	}
	branches := make([]string, len(heads))
	for i := range heads {
		branches[i] = strings.TrimPrefix(heads[i].Refspec, RefsHeads)
	}
	return branches, nil
}

// StashList returns the repository's stashes. It must run in a work tree.
func (r *Repository) StashList() ([]*git.Stash, error) {
	return r.gm.StashList(git.StashListOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
}

// Tags returns the names of the repository's tags, newest version first, as
// git-module sorts them.
func (r *Repository) Tags() ([]string, error) {
	return r.gm.Tags(git.TagsOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
}

// ShowRefVerify returns the object ID the full ref name refspec, such as
// "refs/heads/main", points to.
func (r *Repository) ShowRefVerify(refspec string) (string, error) {
	return r.gm.ShowRefVerify(refspec, git.ShowRefVerifyOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
}

// RevListCount returns how many commits git rev-list lists for revs, at most
// maxCount of them when maxCount is more than 0.
func (r *Repository) RevListCount(maxCount int, revs ...string) (int64, error) {
	opt := git.RevListCountOptions{CommandOptions: git.CommandOptions{Context: r.ctx}}
	if maxCount > 0 {
		opt.Args = []string{"--max-count=" + strconv.Itoa(maxCount)}
	}
	return r.gm.RevListCount(revs, opt)
}

// DeleteBranch deletes the branch, a short name such as "main", whether or
// not it is merged (git branch -D). It is not bound to r's context: once
// started, a deletion finishes even if the session that asked for it ends,
// so the caller can still report it and send its event.
func (r *Repository) DeleteBranch(branch string) error {
	return r.gm.DeleteBranch(branch, git.DeleteBranchOptions{
		Force:          true,
		CommandOptions: git.CommandOptions{Context: context.WithoutCancel(r.ctx)},
	})
}

// DeleteTag deletes the tag, a short name such as "v1.0.0". Like
// DeleteBranch, it finishes once started, whatever happens to r's context.
func (r *Repository) DeleteTag(tag string) error {
	return r.gm.DeleteTag(tag, git.DeleteTagOptions{
		CommandOptions: git.CommandOptions{Context: context.WithoutCancel(r.ctx)},
	})
}

// LatestCommitTime returns the committer date of the most recently committed
// ref tip, as git-module's LatestCommitTime does.
func (r *Repository) LatestCommitTime() (time.Time, error) {
	return r.gm.LatestCommitTime(git.LatestCommitTimeOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
}
