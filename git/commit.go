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
// SSH users; what the parser reported is logged on the default logger.
func (r *Repository) CatFileCommit(rev string) (c *Commit, err error) {
	malformed := func(reason any) error {
		// Debug, not Warn: nothing caches a failed parse, so anyone who
		// can push such a commit could otherwise fill the log.
		log.Debug("cannot parse commit", "path", r.Path, "rev", rev, "reason", reason)
		return fmt.Errorf("%w: %s", ErrMalformedCommit, rev)
	}
	defer func() {
		if p := recover(); p != nil {
			c, err = nil, malformed(p)
		}
	}()
	c, err = r.gm.CatFileCommit(rev, git.CatFileCommitOptions{CommandOptions: git.CommandOptions{Context: r.ctx}})
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
func (r *Repository) BranchCommit(branch string) (*Commit, error) {
	return r.CatFileCommit(RefsHeads + branch)
}

// TagCommit returns the commit the tag (a short name such as "v1.0.0")
// points to. See CatFileCommit.
func (r *Repository) TagCommit(tag string) (*Commit, error) {
	return r.CatFileCommit(RefsTags + tag)
}

// LogOptions are the options of Log.
type LogOptions struct {
	// MaxCount is the most commits listed; 0 means no limit.
	MaxCount int
	// Skip is how many commits to skip before listing.
	Skip int
	// Args are more git log arguments, placed before the revision, such
	// as "--not" and "--glob=refs/heads".
	Args []string
}

// Log returns the commits reachable from rev, newest first. It reads each
// commit through CatFileCommit, under r's context, and leaves out the
// commits it cannot parse, so one malformed commit does not hide the
// others.
func (r *Repository) Log(rev string, opt LogOptions) ([]*Commit, error) {
	ids, err := r.logIDs(rev, opt)
	if err != nil {
		return nil, err
	}
	commits := make([]*Commit, 0, len(ids))
	for _, id := range ids {
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		c, err := r.CatFileCommit(id)
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

// CommitByRevision returns the newest commit reachable from rev. A revision
// git cannot resolve is an ErrRevisionNotExist and a commit it cannot parse
// an ErrMalformedCommit.
func (r *Repository) CommitByRevision(rev string) (*Commit, error) {
	ids, err := r.logIDs(rev, LogOptions{MaxCount: 1})
	if err != nil {
		// git log exits with 128 for a revision it cannot resolve, but
		// also for a repository it cannot read. Only a revision that does
		// not resolve to a commit either is a missing revision.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 && r.revisionMissing(rev) {
			return nil, ErrRevisionNotExist
		}
		return nil, err
	}
	if len(ids) == 0 {
		return nil, ErrRevisionNotExist
	}
	return r.CatFileCommit(ids[0])
}

// revisionMissing reports whether git says rev names no commit. "rev-parse
// --verify --quiet" exits with 1 for that, and with 128 for a fatal error
// such as a repository it cannot read, which is not a missing revision; nor
// is an existing ref whose object is gone (see refUnreadable).
func (r *Repository) revisionMissing(rev string) bool {
	cmd := NewCommand(r.ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	err := cmd.RunInDirPipeline(io.Discard, io.Discard, r.Path)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}
	// A ref whose object is gone does not resolve either; that is the
	// repository's fault.
	return !r.refUnreadable(rev)
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
func (r *Repository) logIDs(rev string, opt LogOptions) ([]string, error) {
	cmd := NewCommand(r.ctx, "log").
		AddArgs(opt.Args...).
		AddArgs("--pretty=" + git.LogFormatHashOnly)
	if opt.MaxCount > 0 {
		cmd.AddArgs("--max-count=" + strconv.Itoa(opt.MaxCount))
	}
	if opt.Skip > 0 {
		cmd.AddArgs("--skip=" + strconv.Itoa(opt.Skip))
	}
	cmd.AddArgs("--end-of-options", rev, "--")
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
