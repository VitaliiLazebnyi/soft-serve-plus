package git

import (
	"fmt"
	"os"
	gopath "path"
	"path/filepath"
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

// OpenLocal opens the repository that contains path. Unlike Open, path can be
// any directory inside a working tree, a linked worktree, a bare repository or
// a git directory, which is what a user running "soft browse" expects. It is
// not meant for repositories the server manages, which must use Open.
func OpenLocal(path string) (*Repository, error) {
	// git would fail with "fork/exec git: not a directory", which reads as
	// if git were missing.
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrNotAGitRepository, path)
	}
	out, err := git.NewCommand("rev-parse", "--is-bare-repository", "--is-inside-git-dir", "--absolute-git-dir").RunInDir(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotAGitRepository, strings.TrimSpace(err.Error()))
	}
	lines := strings.Split(strings.TrimRight(string(out), "\r\n"), "\n")
	if len(lines) != 3 {
		return nil, ErrNotAGitRepository
	}
	// Inside a git directory, also the .git directory of a working tree,
	// there is no work tree to show (git refuses --show-toplevel there), so
	// open the git directory as a bare repository, as Open does.
	bare := strings.TrimSpace(lines[0]) == "true" || strings.TrimSpace(lines[1]) == "true"
	root := strings.TrimRight(lines[2], "\r")
	if !bare {
		// --show-toplevel resolves both a subdirectory and a linked
		// worktree, whose git directory lives in the main repository.
		out, err := git.NewCommand("rev-parse", "--show-toplevel").RunInDir(path)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrNotAGitRepository, strings.TrimSpace(err.Error()))
		}
		root = strings.TrimRight(string(out), "\r\n")
	}
	root = filepath.FromSlash(root)
	repo, err := git.Open(root)
	if err != nil {
		return nil, err
	}
	return &Repository{
		Repository: repo,
		Path:       root,
		IsBare:     bare,
	}, nil
}

// HEAD returns the HEAD reference for a repository.
func (r *Repository) HEAD() (*Reference, error) {
	rn, err := r.Repository.SymbolicRef(git.SymbolicRefOptions{Name: "HEAD"})
	if err != nil {
		return nil, err
	}
	hash, err := r.ShowRefVerify(rn)
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

// DetachedHEAD returns a reference named "HEAD" to the commit HEAD points
// at. It is for a detached HEAD, which holds a commit rather than naming a
// branch, so HEAD returns an error for it. It returns ErrRevisionNotExist
// when HEAD does not resolve to a commit.
func (r *Repository) DetachedHEAD() (*Reference, error) {
	hash, err := r.RevParse(HEAD + "^{commit}")
	if err != nil {
		return nil, err
	}
	return &Reference{
		Reference: &git.Reference{
			ID:      hash,
			Refspec: HEAD,
		},
		path: r.Path,
	}, nil
}

// References returns the references for a repository.
func (r *Repository) References() ([]*Reference, error) {
	refs, err := r.ShowRef()
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

// treeID resolves ref to the ID of the tree it points to. The ref is passed
// after --end-of-options so it cannot be read as an option, and --verify
// makes git reject anything that is not a single revision. git still prints
// a negated revision such as "^HEAD" with its leading caret, so the output is
// parsed strictly: a ref git cannot resolve, or one that does not resolve to
// a single object ID, is an ErrRevisionNotExist.
func (r *Repository) treeID(ref string) (string, error) {
	out, err := git.NewCommand("rev-parse", "--verify", "--end-of-options", ref+"^{tree}").RunInDir(r.Path)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 128") {
			return "", ErrRevisionNotExist
		}
		return "", err
	}
	id, err := git.NewIDFromString(string(out))
	if err != nil {
		return "", ErrRevisionNotExist
	}
	return id.String(), nil
}

// LsTree returns the tree for the given reference.
func (r *Repository) LsTree(ref string) (*Tree, error) {
	id, err := r.treeID(ref)
	if err != nil {
		return nil, err
	}
	tree, err := r.Repository.LsTree(id)
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

// diffCommandOptions returns the options for git commands whose diff output
// is parsed. User and system config must not change the output: an external
// diff driver (diff.external) or forced color (color.ui=always) produce text
// the parser does not understand, so both are turned off on the command line
// too, which also covers the repository's own config.
func diffCommandOptions() git.CommandOptions {
	return git.CommandOptions{
		Args: []string{"--no-ext-diff", "--no-color"},
		Envs: []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"},
	}
}

// Diff returns the diff for the given commit.
func (r *Repository) Diff(commit *Commit) (*Diff, error) {
	diff, err := r.Repository.Diff(commit.ID.String(), DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars, git.DiffOptions{
		CommandOptions: diffCommandOptions(),
	})
	if err != nil {
		return nil, err
	}
	return toDiff(diff), nil
}

// Patch returns the patch for the given commit against its first parent, or
// against the empty tree for a root commit.
//
// Unlike [Diff.Patch], which rebuilds a patch from a parsed and size-limited
// [Diff], this returns git's own output unchanged, so nothing is truncated and
// markers such as "\ No newline at end of file" are kept. The result can be
// fed to "git apply".
func (r *Repository) Patch(commit *Commit) (string, error) {
	args := []string{"diff-tree", "-r", "-p", "--full-index", "-M", "--no-commit-id"}
	if commit.ParentsCount() == 0 {
		args = append(args, "--root", commit.ID.String())
	} else {
		parent, err := commit.ParentID(0)
		if err != nil {
			return "", err
		}
		args = append(args, parent.String(), commit.ID.String())
	}
	out, err := NewCommand(args...).
		AddEnvs("GIT_CONFIG_GLOBAL=/dev/null").
		RunInDir(r.Path)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// CountCommits returns the number of commits in the repository.
func (r *Repository) CountCommits(ref *Reference) (int64, error) {
	return r.RevListCount([]string{ref.Name().String()})
}

// CommitsByPage returns the commits for a given page and size.
func (r *Repository) CommitsByPage(ref *Reference, page, size int) (Commits, error) {
	cs, err := r.Repository.CommitsByPage(ref.Name().String(), page, size)
	if err != nil {
		return nil, err
	}
	commits := make(Commits, len(cs))
	copy(commits, cs)
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
	return r.Repository.SymbolicRef(opt)
}
