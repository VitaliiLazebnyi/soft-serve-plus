package git

import (
	"bytes"

	"github.com/aymanbagabas/git-module"
)

// Blame is the blame of a file: the commit that last changed each line.
type Blame struct {
	lines []*Commit
}

// Line returns the commit that last changed line i (1-based). It returns nil
// when there is no such line.
func (b *Blame) Line(i int) *Commit {
	if b == nil || i <= 0 || len(b.lines) < i {
		return nil
	}
	return b.lines[i-1]
}

// BlameFile returns the blame of a file at the given revision.
//
// It runs git blame itself rather than calling git-module's BlameFile, which
// puts the command options only on `git blame` and then reads the commit of
// every blamed line without a context. Here a cancelled context also stops
// the per-commit reads, so a client that disconnects while a large file is
// being blamed does not leave them running.
func (r *Repository) BlameFile(rev, file string, opts ...git.BlameOptions) (*Blame, error) {
	var opt git.BlameOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	cmd := git.NewCommand("blame").
		AddOptions(r.withCtx(opt.CommandOptions)).
		AddArgs("-l", "-s", rev, "--", file)
	// Honour the deprecated BlameOptions.Timeout the way git-module does.
	if timeout := opt.Timeout; timeout != 0 { //nolint:staticcheck
		cmd = cmd.WithTimeout(timeout)
	}
	stdout, err := cmd.RunInDir(r.Path)
	if err != nil {
		return nil, err
	}

	lines := bytes.Split(stdout, []byte{'\n'})
	blame := &Blame{lines: make([]*Commit, 0, len(lines))}
	seen := make(map[string]*Commit)
	for _, line := range lines {
		if len(line) < 40 {
			break
		}
		id := line[:40]
		// The earliest commit is marked with a leading "^".
		if id[0] == '^' {
			id = id[1:]
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		c, ok := seen[string(id)]
		if !ok {
			c, err = r.CatFileCommit(string(id), git.CatFileCommitOptions{Timeout: opt.Timeout}) //nolint:staticcheck
			if err != nil {
				return nil, err
			}
			seen[string(id)] = c
		}
		blame.lines = append(blame.lines, c)
	}
	return blame, nil
}
