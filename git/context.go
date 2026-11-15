package git

import (
	"context"
	"strings"

	"github.com/aymanbagabas/git-module"
)

// WithContext returns a copy of r whose git commands run under ctx: they are
// killed as soon as ctx is done, for example when the SSH session that asked
// for them disconnects. Without a context a command only stops at
// git-module's default timeout of one minute.
//
// Trees, tree entries and files obtained from the copy carry the context
// too.
func (r *Repository) WithContext(ctx context.Context) *Repository {
	rr := *r
	rr.ctx = ctx
	return &rr
}

// Context returns the context r's git commands run under. It is never nil.
func (r *Repository) Context() context.Context {
	if r == nil || r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// WithContext returns a copy of e whose contents are read under ctx, in
// place of the context of the repository it was listed from.
func (e *TreeEntry) WithContext(ctx context.Context) *TreeEntry {
	ee := *e
	if ee.repo != nil {
		ee.repo = ee.repo.WithContext(ctx)
	}
	return &ee
}

// cmdOpts returns git-module command options that run a command under r's
// context.
func (r *Repository) cmdOpts() git.CommandOptions {
	return git.CommandOptions{Context: r.Context()}
}

// withCtx sets o's context to r's when the caller has not set one.
func (r *Repository) withCtx(o git.CommandOptions) git.CommandOptions {
	if o.Context == nil {
		o.Context = r.Context()
	}
	return o
}

// Branches returns the names of the repository's branches.
func (r *Repository) Branches() ([]string, error) {
	heads, err := r.ShowRef(git.ShowRefOptions{Heads: true, CommandOptions: r.cmdOpts()})
	if err != nil {
		return nil, err
	}
	branches := make([]string, len(heads))
	for i := range heads {
		branches[i] = strings.TrimPrefix(heads[i].Refspec, RefsHeads)
	}
	return branches, nil
}

// StashList returns the repository's stashes. It must run in a work tree.
func (r *Repository) StashList(opts ...git.StashListOptions) ([]*git.Stash, error) {
	var opt git.StashListOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt.CommandOptions = r.withCtx(opt.CommandOptions)
	return r.Repository.StashList(opt)
}
