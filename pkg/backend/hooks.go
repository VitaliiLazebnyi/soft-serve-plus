package backend

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
)

var _ hooks.Hooks = (*Backend)(nil)

// PostReceive is called by the git post-receive hook.
//
// It sends the push and branch/tag webhooks. Git runs post-receive after it
// writes the refs and passes it only the refs it actually updated, so a ref
// that git or another hook rejected sends no events.
//
// It implements Hooks.
func (d *Backend) PostReceive(ctx context.Context, _ io.Writer, _ io.Writer, repo string, args []hooks.HookArg) {
	d.logger.Debug("post-receive hook called", "repo", repo, "args", args)
	if len(args) == 0 {
		return
	}

	user, ok := d.pushUser(ctx)
	if !ok {
		return
	}

	r, err := d.Repository(ctx, repo)
	if err != nil {
		d.logger.Error("error finding repository", "repo", repo, "err", err)
		return
	}

	// TODO: run this async
	// This would probably need something like an RPC server to communicate with the hook process.
	for _, arg := range args {
		if git.IsZeroHash(arg.OldSha) || git.IsZeroHash(arg.NewSha) {
			wh, err := webhook.NewBranchTagEvent(ctx, user, r, arg.RefName, arg.OldSha, arg.NewSha)
			if err != nil {
				d.logger.Error("error creating branch_tag webhook", "err", err)
			} else if err := webhook.SendEvent(ctx, wh); err != nil {
				d.logger.Error("error sending branch_tag webhook", "err", err)
			}
		}
		wh, err := webhook.NewPushEvent(ctx, user, r, arg.RefName, arg.OldSha, arg.NewSha)
		if err != nil {
			d.logger.Error("error creating push webhook", "err", err)
		} else if err := webhook.SendEvent(ctx, wh); err != nil {
			d.logger.Error("error sending push webhook", "err", err)
		}
	}
}

// pushUser returns the user who pushed, from the environment the server sets
// for the hooks. It returns false when the user cannot be looked up.
func (d *Backend) pushUser(ctx context.Context) (proto.User, bool) {
	if pubkey := os.Getenv("SOFT_SERVE_PUBLIC_KEY"); pubkey != "" {
		pk, _, err := sshutils.ParseAuthorizedKey(pubkey)
		if err != nil {
			d.logger.Error("error parsing public key", "err", err)
			return nil, false
		}

		user, err := d.UserByPublicKey(ctx, pk)
		switch {
		case errors.Is(err, proto.ErrUserNotFound):
			// The SSH layer already authorized this push. A key without a
			// user row, such as an initial_admin_keys entry with no account,
			// still sends its events, with an empty sender. This covers any
			// unregistered key, so with anon-access read-write an anonymous
			// SSH push sends them too.
			return nil, true
		case err != nil:
			d.logger.Error("error finding user from public key", "key", pubkey, "err", err)
			return nil, false
		}
		return user, true
	}

	if username := os.Getenv("SOFT_SERVE_USERNAME"); username != "" {
		user, err := d.User(ctx, username)
		if err != nil {
			d.logger.Error("error finding user from username", "username", username, "err", err)
			return nil, false
		}
		return user, true
	}

	// Keyless SSH and anonymous HTTP pushes set neither variable. The
	// server already authorized them, so they send their events with an
	// empty sender, like an unregistered key.
	return nil, true
}

// PreReceive is called by the git pre-receive hook.
//
// It implements Hooks.
func (d *Backend) PreReceive(_ context.Context, _ io.Writer, _ io.Writer, repo string, args []hooks.HookArg) {
	d.logger.Debug("pre-receive hook called", "repo", repo, "args", args)
}

// Update is called by the git update hook.
//
// It sends no webhooks: git runs it before it writes the ref, and the ref
// may still be rejected. PostReceive sends them.
//
// It implements Hooks.
func (d *Backend) Update(_ context.Context, _ io.Writer, _ io.Writer, repo string, arg hooks.HookArg) {
	d.logger.Debug("update hook called", "repo", repo, "arg", arg)
}

// PostUpdate is called by the git post-update hook.
//
// It implements Hooks.
func (d *Backend) PostUpdate(ctx context.Context, _ io.Writer, _ io.Writer, repo string, args ...string) {
	d.logger.Debug("post-update hook called", "repo", repo, "args", args)

	var wg sync.WaitGroup

	// Populate last-modified file.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := populateLastModified(ctx, d, repo); err != nil {
			d.logger.Error("error populating last-modified", "repo", repo, "err", err)
			return
		}
	}()

	wg.Wait()
}

func populateLastModified(ctx context.Context, d *Backend, name string) error {
	var rr *repo
	_rr, err := d.Repository(ctx, name)
	if err != nil {
		return err
	}

	if r, ok := _rr.(*repo); ok {
		rr = r
	} else {
		return proto.ErrRepoNotFound
	}

	r, err := rr.Open()
	if err != nil {
		return err
	}

	c, err := r.LatestCommitTime()
	if err != nil {
		return err
	}

	return rr.writeLastModified(c)
}
