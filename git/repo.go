package git

import (
	"bytes"
	"context"
	"fmt"
	gopath "path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aymanbagabas/git-module"
)

var (
	// DiffMaxFile is the maximum number of files to show in a diff.
	DiffMaxFiles = 1000
	// DiffMaxFileLines is the maximum number of lines to show in a file diff.
	DiffMaxFileLines = 1000
	// DiffMaxLineChars is the maximum number of characters to show in a line diff.
	DiffMaxLineChars = 1000
)

// Repository is a wrapper around git.Repository with helper methods.
type Repository struct {
	*git.Repository
	Path   string
	IsBare bool

	// ctx is the context git commands run under; see WithContext.
	ctx context.Context
}

// Clone clones a repository.
func Clone(src, dst string, opts ...git.CloneOptions) error {
	return git.Clone(src, dst, opts...)
}

// Init initializes and opens a new git repository.
func Init(path string, bare bool) (*Repository, error) {
	if bare {
		path = strings.TrimSuffix(path, ".git") + ".git"
	}

	err := git.Init(path, git.InitOptions{Bare: bare})
	if err != nil {
		return nil, err
	}
	return Open(path)
}

// gitDir runs "git rev-parse --git-dir" in r. It does not use r.RevParse,
// which turns every exit status 128 into ErrRevisionNotExist and drops
// git's stderr.
func gitDir(r *git.Repository) (string, error) {
	out, err := git.NewCommand("rev-parse", "--git-dir").RunInDir(r.Path())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Open opens a git repository at the given path.
//
// When git does not accept the path as a repository the error wraps
// ErrNotAGitRepository and carries git's reason, for example "detected
// dubious ownership". That reason can include the server path.
func Open(path string) (*Repository, error) {
	repo, err := git.Open(path)
	if err != nil {
		return nil, err
	}
	gp, err := gitDir(repo)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotAGitRepository, strings.TrimSpace(err.Error()))
	}
	if gp != "." && gp != ".git" {
		return nil, ErrNotAGitRepository
	}
	return &Repository{
		Repository: repo,
		Path:       path,
		IsBare:     gp == ".",
	}, nil
}

// HEAD returns the HEAD reference for a repository.
func (r *Repository) HEAD() (*Reference, error) {
	rn, err := r.Repository.SymbolicRef(git.SymbolicRefOptions{Name: "HEAD", CommandOptions: r.cmdOpts()})
	if err != nil {
		return nil, err
	}
	hash, err := r.ShowRefVerify(rn, git.ShowRefVerifyOptions{CommandOptions: r.cmdOpts()})
	if err != nil {
		return nil, err
	}
	return &Reference{
		Reference: &git.Reference{
			ID:      hash,
			Refspec: rn,
		},
		path: r.Path,
	}, nil
}

// References returns the references for a repository.
func (r *Repository) References() ([]*Reference, error) {
	refs, err := r.ShowRef(git.ShowRefOptions{CommandOptions: r.cmdOpts()})
	if err != nil {
		return nil, err
	}
	rrefs := make([]*Reference, 0, len(refs))
	for _, ref := range refs {
		rrefs = append(rrefs, &Reference{
			Reference: ref,
			path:      r.Path,
		})
	}
	return rrefs, nil
}

// LsTree returns the tree for the given reference.
func (r *Repository) LsTree(ref string) (*Tree, error) {
	tree, err := r.Repository.LsTree(ref, git.LsTreeOptions{CommandOptions: r.cmdOpts()})
	if err != nil {
		return nil, err
	}
	return &Tree{
		Tree:       tree,
		Path:       "",
		Repository: r,
	}, nil
}

// Tree returns the tree for the given reference.
func (r *Repository) Tree(ref *Reference) (*Tree, error) {
	if ref == nil {
		rref, err := r.HEAD()
		if err != nil {
			return nil, err
		}
		ref = rref
	}
	return r.LsTree(ref.ID)
}

// TreePath returns the tree for the given path.
// Git paths always use forward slashes regardless of OS.
func (r *Repository) TreePath(ref *Reference, path string) (*Tree, error) {
	path = gopath.Clean(filepath.ToSlash(path))
	if path == "." {
		path = ""
	}
	if path == "" {
		return r.Tree(ref)
	}
	t, err := r.Tree(ref)
	if err != nil {
		return nil, err
	}
	return t.SubTree(path)
}

// Diff returns the diff for the given commit.
func (r *Repository) Diff(commit *Commit) (*Diff, error) {
	diff, err := r.Repository.Diff(commit.ID.String(), DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars, git.DiffOptions{
		CommandOptions: git.CommandOptions{
			Envs:    []string{"GIT_CONFIG_GLOBAL=/dev/null"},
			Context: r.Context(),
		},
	})
	if err != nil {
		return nil, err
	}
	return toDiff(diff), nil
}

// Patch returns the patch for the given reference.
func (r *Repository) Patch(commit *Commit) (string, error) {
	diff, err := r.Diff(commit)
	if err != nil {
		return "", err
	}
	return diff.Patch(), err
}

// CountCommits returns the number of commits in the repository.
func (r *Repository) CountCommits(ref *Reference) (int64, error) {
	return r.RevListCount([]string{ref.Name().String()}, git.RevListCountOptions{CommandOptions: r.cmdOpts()})
}

// CommitsByPage returns the commits for a given page and size.
//
// It lists the page itself rather than calling git-module's CommitsByPage,
// which reads each commit without a context, so that a cancelled context
// also stops the per-commit reads.
func (r *Repository) CommitsByPage(ref *Reference, page, size int) (Commits, error) {
	cmd := git.NewCommand("log", "--pretty="+git.LogFormatHashOnly).AddOptions(r.cmdOpts())
	if size > 0 {
		cmd.AddArgs("--max-count=" + strconv.Itoa(size))
	}
	if skip := (page - 1) * size; skip > 0 {
		cmd.AddArgs("--skip=" + strconv.Itoa(skip))
	}
	out, err := cmd.AddArgs("--end-of-options", ref.Name().String(), "--").RunInDir(r.Path)
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return Commits{}, nil
	}
	ids := bytes.Split(out, []byte{'\n'})
	commits := make(Commits, 0, len(ids))
	for _, id := range ids {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		c, err := r.CatFileCommit(string(id))
		if err != nil {
			return nil, err
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// SymbolicRef returns or updates the symbolic reference for the given name.
// Both name and ref can be empty.
func (r *Repository) SymbolicRef(name string, ref string, opts ...git.SymbolicRefOptions) (string, error) {
	var opt git.SymbolicRefOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	opt.Name = name
	opt.Ref = ref
	opt.CommandOptions = r.withCtx(opt.CommandOptions)
	return r.Repository.SymbolicRef(opt)
}
