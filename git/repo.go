package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	gopath "path"
	"path/filepath"
	"strings"

	"github.com/aymanbagabas/git-module"
)

// Repository is a git repository the server or "soft browse" reads.
//
// It is bound to the context it was opened with (see Open): every git
// command it runs, and those of the trees, entries and files read from it,
// is killed as soon as that context is done, for example when the SSH
// session that asked for it disconnects. The context is the one
// request-scoped value it keeps, because its many read methods take none.
type Repository struct {
	gm     *git.Repository
	Path   string
	IsBare bool

	// ctx is the context git commands run under, never nil.
	ctx context.Context
	// diffLimits caps Diff and StashDiff, and its total Patch:
	// DefaultDiffLimits, which tests lower on their own repository.
	diffLimits DiffLimits
	// attrSource reports whether git check-attr supports --source:
	// attrSourceSupported, which tests replace on their own repository to
	// exercise the fallback.
	attrSource func() bool
}

// newRepository returns the repository at path, bound to ctx, with the
// production limits and checks.
func newRepository(ctx context.Context, gm *git.Repository, path string, bare bool) *Repository {
	return &Repository{
		gm:         gm,
		Path:       path,
		IsBare:     bare,
		ctx:        ctx,
		diffLimits: DefaultDiffLimits(),
		attrSource: attrSourceSupported,
	}
}

// Init initializes and opens a new git repository. git init and the
// returned repository run under ctx.
func Init(ctx context.Context, path string, bare bool) (*Repository, error) {
	if bare {
		path = strings.TrimSuffix(path, ".git") + ".git"
	}

	err := git.Init(path, git.InitOptions{Bare: bare, CommandOptions: git.CommandOptions{Context: ctx}})
	if err != nil {
		return nil, err
	}
	return Open(ctx, path)
}

// gitDir runs "git rev-parse --git-dir" in path under ctx. It does not use
// git-module's RevParse, which turns every exit status 128 into
// ErrRevisionNotExist and drops git's stderr.
func gitDir(ctx context.Context, path string) (string, error) {
	out, err := git.NewCommandWithContext(ctx, "rev-parse", "--git-dir").RunInDir(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Open opens a git repository at the given path. The repository's git
// commands, including the one Open runs, run under ctx and stop when it is
// done; ctx must not be nil.
//
// When git does not accept the path as a repository the error wraps
// ErrNotAGitRepository and carries git's reason, for example "detected
// dubious ownership". That reason can include the server path.
func Open(ctx context.Context, path string) (*Repository, error) {
	repo, err := git.Open(path)
	if err != nil {
		return nil, err
	}
	gp, err := gitDir(ctx, repo.Path())
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotAGitRepository, strings.TrimSpace(err.Error()))
	}
	if gp != "." && gp != ".git" {
		return nil, ErrNotAGitRepository
	}
	return newRepository(ctx, repo, path, gp == "."), nil
}

// OpenLocal opens the repository that contains path. Unlike Open, path can be
// any directory inside a working tree, a linked worktree, a bare repository or
// a git directory, which is what a user running "soft browse" expects. It is
// not meant for repositories the server manages, which must use Open. Like
// Open, it binds the repository to ctx.
func OpenLocal(ctx context.Context, path string) (*Repository, error) {
	// git would fail with "fork/exec git: not a directory", which reads as
	// if git were missing.
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrNotAGitRepository, path)
	}
	out, err := git.NewCommandWithContext(ctx, "rev-parse", "--is-bare-repository", "--is-inside-git-dir", "--absolute-git-dir").RunInDir(path)
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
		out, err := git.NewCommandWithContext(ctx, "rev-parse", "--show-toplevel").RunInDir(path)
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
	return newRepository(ctx, repo, root, bare), nil
}

// HEAD returns the HEAD reference for a repository.
func (r *Repository) HEAD() (*Reference, error) {
	rn, err := r.gm.SymbolicRef(git.SymbolicRefOptions{Name: "HEAD", CommandOptions: git.CommandOptions{Context: r.ctx}})
	if err != nil {
		return nil, err
	}
	hash, err := r.gm.ShowRefVerify(rn, git.ShowRefVerifyOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
	if err != nil {
		return nil, err
	}
	return newReference(hash, rn), nil
}

// DetachedHEAD returns a reference named "HEAD" to the commit HEAD points
// at. It is for a detached HEAD, which holds a commit rather than naming a
// branch, so HEAD returns an error for it. It returns ErrRevisionNotExist
// when HEAD does not resolve to a commit.
func (r *Repository) DetachedHEAD() (*Reference, error) {
	hash, err := r.gm.RevParse(HEAD+"^{commit}", git.RevParseOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
	if err != nil {
		return nil, err
	}
	return newReference(hash, HEAD), nil
}

// References returns the references for a repository. It returns an empty
// list when the repository has no references.
func (r *Repository) References() ([]*Reference, error) {
	refs, err := r.gm.ShowRef(git.ShowRefOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
	if err != nil {
		// git show-ref exits with status 1 and prints nothing when there
		// are no references. Other failures print to stderr, which
		// git-module folds into an error that is no longer an
		// *exec.ExitError.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return []*Reference{}, nil
		}
		return nil, err
	}
	rrefs := make([]*Reference, 0, len(refs))
	for _, ref := range refs {
		rrefs = append(rrefs, newReference(ref.ID, ref.Refspec))
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
	var out, stderr bytes.Buffer
	cmd := NewCommand(r.ctx, "rev-parse", "--verify", "--end-of-options", ref+"^{tree}")
	if err := cmd.RunInDirPipeline(&out, &stderr, r.Path); err != nil {
		// git exits with 128 when it cannot resolve the revision, but
		// also when it cannot read the repository, such as one with
		// corrupt refs. Only a revision "rev-parse --verify --quiet"
		// also finds missing is a missing revision. The error is read
		// directly, not through RunInDir, which folds stderr into a new
		// error that is no longer an *exec.ExitError.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 && r.treeMissing(ref) {
			return "", ErrRevisionNotExist
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w - %s", err, msg)
		}
		return "", err
	}
	id, err := git.NewIDFromString(out.String())
	if err != nil {
		return "", ErrRevisionNotExist
	}
	return id.String(), nil
}

// treeMissing reports whether ref names no tree, as the client may ask for:
// "rev-parse --verify --quiet" exits with 1 for ref^{tree}, and ref is not
// a commit git can read either. It exits with 128 for a fatal error such as
// a repository it cannot read, and a commit whose tree cannot be read is a
// tree missing from the repository, not a revision the client got wrong.
func (r *Repository) treeMissing(ref string) bool {
	resolves := func(rev string) (bool, bool) {
		cmd := NewCommand(r.ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev)
		err := cmd.RunInDirPipeline(io.Discard, io.Discard, r.Path)
		var exitErr *exec.ExitError
		return err == nil, errors.As(err, &exitErr) && exitErr.ExitCode() == 1
	}
	if _, missing := resolves(ref + "^{tree}"); !missing {
		return false
	}
	isCommit, _ := resolves(ref + "^{commit}")
	return !isCommit && !r.refUnreadable(ref)
}

// refUnreadable reports whether rev is a ref that exists but whose object
// git cannot read, which is the repository's fault rather than a revision
// the client got wrong. That is so when rev names a ref (namesRef) and the
// ref does not peel ("rev-parse --verify --quiet REV^{}" fails): a ref to a
// blob, for example a tag on one, peels, and is the client asking for a
// tree or commit where there is none. A raw object ID that names nothing
// and an unknown name are no refs.
func (r *Repository) refUnreadable(rev string) bool {
	if !r.namesRef(rev) {
		return false
	}
	cmd := NewCommand(r.ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{}")
	return cmd.RunInDirPipeline(io.Discard, io.Discard, r.Path) != nil
}

// namesRef reports whether rev names a ref that exists, such as a branch
// or a detached HEAD, whatever its object: "rev-parse --symbolic-full-name"
// prints the ref's full name for it, or HEAD for a detached HEAD, and
// nothing for a raw object ID or an expression such as main~1.
func (r *Repository) namesRef(rev string) bool {
	var out bytes.Buffer
	cmd := NewCommand(r.ctx, "rev-parse", "--verify", "--quiet", "--symbolic-full-name", "--end-of-options", rev)
	if err := cmd.RunInDirPipeline(&out, io.Discard, r.Path); err != nil {
		return false
	}
	name := strings.TrimSpace(out.String())
	return strings.HasPrefix(name, "refs/") || name == "HEAD"
}

// LsTree returns the root tree of the given reference. It only resolves
// the reference; the tree is read when its entries are asked for.
func (r *Repository) LsTree(ref string) (*Tree, error) {
	id, err := r.treeID(ref)
	if err != nil {
		return nil, err
	}
	return &Tree{id: id, repo: r}, nil
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
	return r.LsTree(ref.ID())
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

// CountCommits returns the number of commits in the repository.
func (r *Repository) CountCommits(ref *Reference) (int64, error) {
	return r.gm.RevListCount([]string{ref.Name().String()}, git.RevListCountOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
}

// CommitsByPage returns the commits for a given page and size, newest
// first. It lists the page through Log, so commits git-module cannot parse
// are left out, and a cancelled context also stops the per-commit reads.
func (r *Repository) CommitsByPage(ref *Reference, page, size int) (Commits, error) {
	opt := LogOptions{MaxCount: size}
	if skip := (page - 1) * size; skip > 0 {
		opt.Skip = skip
	}
	return r.Log(ref.Name().String(), opt)
}

// SymbolicRef returns or updates the symbolic reference for the given name.
// Both name and ref can be empty.
func (r *Repository) SymbolicRef(name string, ref string) (string, error) {
	return r.gm.SymbolicRef(git.SymbolicRefOptions{
		Name:           name,
		Ref:            ref,
		CommandOptions: git.CommandOptions{Context: r.ctx},
	})
}

// WriteBlob streams the blob with the given ID to w without holding it in
// memory. It runs until ctx is done instead of git's default timeout, so a
// slow reader does not cut a large blob short.
func (r *Repository) WriteBlob(ctx context.Context, id *git.SHA1, w io.Writer) error {
	cmd := git.NewCommandWithContext(ctx, "cat-file", "blob", id.String())
	cmd.SetTimeout(-1)
	stderr := new(bytes.Buffer)
	if err := cmd.RunInDirPipeline(w, stderr, r.Path); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w - %s", err, msg)
		}
		return err
	}
	return nil
}
