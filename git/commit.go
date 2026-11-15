package git

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/aymanbagabas/git-module"
)

// ZeroID is the zero hash.
const ZeroID = git.EmptyID

var zeroHashPattern = regexp.MustCompile(`^0{40,}$`)

// IsZeroHash returns whether the hash is a zero hash.
func IsZeroHash(h string) bool {
	return zeroHashPattern.MatchString(h)
}

// Commit is a wrapper around git.Commit with helper methods.
type Commit = git.Commit

// Commits is a list of commits.
type Commits []*Commit

// git-module's commit parser (parseCommit, parseSignature) indexes the
// commit object without checking its shape: a header line without a space,
// an author or committer without "<email>" or without a date all panic, and
// a date it cannot read is a *time.ParseError or *strconv.NumError. git
// accepts such commits in a push (the server does not fsck), and old
// imported histories hold some. Every read of a commit therefore goes
// through CatFileCommit below, which turns all of these into
// ErrMalformedCommit, and the git-module methods that read commits with its
// own CatFileCommit (Log, CommitByRevision, BranchCommit, TagCommit) are
// replaced by methods that use this one.

// CatFileCommit returns the commit for the given revision. A commit
// git-module cannot parse, or one without an author or committer, is an
// ErrMalformedCommit. Its message names only the revision, since it reaches
// SSH users; what the parser reported is logged.
func (r *Repository) CatFileCommit(rev string, opts ...git.CatFileCommitOptions) (c *Commit, err error) {
	var opt git.CatFileCommitOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt.CommandOptions = r.withCtx(opt.CommandOptions)
	malformed := func(reason any) error {
		// Debug, not Warn: nothing caches a failed parse, so anyone who
		// can push such a commit could otherwise fill the log.
		log.FromContext(r.Context()).Debug("cannot parse commit", "path", r.Path, "rev", rev, "reason", reason)
		return fmt.Errorf("%w: %s", ErrMalformedCommit, rev)
	}
	defer func() {
		if p := recover(); p != nil {
			c, err = nil, malformed(p)
		}
	}()
	c, err = r.Repository.CatFileCommit(rev, opt)
	if err != nil {
		if isParseError(err) {
			return nil, malformed(err)
		}
		return nil, err
	}
	// parseCommit leaves a signature nil when its line is missing, and
	// callers read c.Author.Name without checking.
	if c.Author == nil || c.Committer == nil {
		return nil, malformed("no author or committer")
	}
	return c, nil
}

// BranchCommit returns the commit the branch (a short name such as "main")
// points to. See CatFileCommit.
func (r *Repository) BranchCommit(branch string, opts ...git.CatFileCommitOptions) (*Commit, error) {
	return r.CatFileCommit(RefsHeads+branch, opts...)
}

// TagCommit returns the commit the tag (a short name such as "v1.0.0")
// points to. See CatFileCommit.
func (r *Repository) TagCommit(tag string, opts ...git.CatFileCommitOptions) (*Commit, error) {
	return r.CatFileCommit(RefsTags+tag, opts...)
}

// Log returns the commits reachable from rev, newest first, with the same
// options as git-module's Log. It reads each commit through CatFileCommit,
// under r's context, and leaves out the commits it cannot parse, so one
// malformed commit does not hide the others.
func (r *Repository) Log(rev string, opts ...git.LogOptions) ([]*Commit, error) {
	var opt git.LogOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	ids, err := r.logIDs(rev, opt)
	if err != nil {
		return nil, err
	}
	commits := make([]*Commit, 0, len(ids))
	for _, id := range ids {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		c, err := r.CatFileCommit(id, git.CatFileCommitOptions{Timeout: opt.Timeout}) //nolint:staticcheck
		if errors.Is(err, ErrMalformedCommit) {
			continue
		}
		if err != nil {
			return nil, err
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// CommitByRevision returns the newest commit reachable from rev, or, with
// opt.Path, the newest that changed that path. A revision git cannot
// resolve is an ErrRevisionNotExist and a commit it cannot parse an
// ErrMalformedCommit.
func (r *Repository) CommitByRevision(rev string, opts ...git.CommitByRevisionOptions) (*Commit, error) {
	var opt git.CommitByRevisionOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	ids, err := r.logIDs(rev, git.LogOptions{
		MaxCount:       1,
		Path:           opt.Path,
		Timeout:        opt.Timeout,
		CommandOptions: opt.CommandOptions,
	})
	if err != nil {
		// git log exits with 128 for a revision it cannot resolve, but
		// also for a repository it cannot read. Only a revision that does
		// not resolve to a commit either is a missing revision.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 && r.revisionMissing(rev, opt) {
			return nil, ErrRevisionNotExist
		}
		return nil, err
	}
	if len(ids) == 0 {
		return nil, ErrRevisionNotExist
	}
	return r.CatFileCommit(ids[0], git.CatFileCommitOptions{Timeout: opt.Timeout}) //nolint:staticcheck
}

// revisionMissing reports whether git says rev names no commit. "rev-parse
// --verify --quiet" exits with 1 for that, and with 128 for a fatal error
// such as a repository it cannot read, which is not a missing revision.
// It runs under the caller's context and timeout.
func (r *Repository) revisionMissing(rev string, opt git.CommitByRevisionOptions) bool {
	cmd := git.NewCommand("rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}").
		AddOptions(r.withCtx(git.CommandOptions{
			Context: opt.Context,
			Timeout: opt.CommandOptions.Timeout,
		}))
	if opt.Timeout != 0 {
		cmd = cmd.WithTimeout(opt.Timeout)
	}
	err := cmd.RunInDirPipeline(io.Discard, io.Discard, r.Path)
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// isParseError reports whether err is one of the errors git-module's commit
// parser returns for a header it cannot read, rather than a failure to run
// git: a date that is not a date (parseSignature), or a tree, object or
// parent ID that is not 40 hex digits (NewIDFromString). git itself ignores
// tree and parent headers after the first ones, so a push accepts those.
func isParseError(err error) bool {
	var timeErr *time.ParseError
	var numErr *strconv.NumError
	var hexErr hex.InvalidByteError
	return errors.As(err, &timeErr) || errors.As(err, &numErr) ||
		errors.As(err, &hexErr) || errors.Is(err, hex.ErrLength) ||
		// git-module has no sentinel for this one (sha1.go).
		err.Error() == "length must be 40"
}

// logIDs runs git log for rev with opt, as git-module's Log does, and
// returns the IDs it lists. Its error keeps git's *exec.ExitError.
func (r *Repository) logIDs(rev string, opt git.LogOptions) ([]string, error) {
	cmd := git.NewCommand("log").
		AddOptions(r.withCtx(opt.CommandOptions)).
		AddArgs("--pretty=" + git.LogFormatHashOnly)
	if opt.MaxCount > 0 {
		cmd.AddArgs("--max-count=" + strconv.Itoa(opt.MaxCount))
	}
	if opt.Skip > 0 {
		cmd.AddArgs("--skip=" + strconv.Itoa(opt.Skip))
	}
	if !opt.Since.IsZero() {
		cmd.AddArgs("--since=" + opt.Since.Format(time.RFC3339))
	}
	if opt.GrepPattern != "" {
		cmd.AddArgs("--grep=" + opt.GrepPattern)
	}
	if opt.RegexpIgnoreCase {
		cmd.AddArgs("--regexp-ignore-case")
	}
	cmd.AddArgs("--end-of-options", rev, "--")
	if opt.Path != "" {
		// A path that starts with ':' would be read as pathspec magic.
		path := opt.Path
		if path[0] == ':' {
			path = `\` + path
		}
		cmd.AddArgs(path)
	}
	if opt.Timeout != 0 {
		cmd = cmd.WithTimeout(opt.Timeout)
	}
	var stdout, stderr bytes.Buffer
	if err := cmd.RunInDirPipeline(&stdout, &stderr, r.Path); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w - %s", err, msg)
		}
		return nil, err
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return nil, nil
	}
	return strings.Split(string(out), "\n"), nil
}
