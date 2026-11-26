package backend

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// A repository created by an HTTP push is removed only if nothing was done
// with it since: no ref, no changed setting, no collaborator, webhook, LFS
// object or LFS lock.
func TestRemovePushCreatedRepositoryKeepsUsed(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	inTx := func(fn func(tx *db.Tx) error) func(t *testing.T, r proto.Repository) {
		return func(t *testing.T, _ proto.Repository) {
			if err := be.db.TransactionContext(ctx, fn); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := map[string]func(t *testing.T, r proto.Repository){
		"untouched": nil,
		"ref": func(t *testing.T, r proto.Repository) {
			dir := be.repoPath(r.Name())
			tree, err := exec.CommandContext(t.Context(), "git", "-C", dir, "mktree").Output()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "commit-tree", string(tree[:len(tree)-1]), "-m", "x")
			cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
			commit, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if err := exec.CommandContext(t.Context(), "git", "-C", dir, "update-ref", "refs/heads/main", string(commit[:len(commit)-1])).Run(); err != nil {
				t.Fatal(err)
			}
		},
		// The settings are changed in the store, as the backend's setters
		// do, without the webhook event those also deliver.
		"description": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoDescriptionByName(ctx, tx, "repo-description", "about")
		}),
		"project-name": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoProjectNameByName(ctx, tx, "repo-project-name", "Project")
		}),
		"private": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoIsPrivateByName(ctx, tx, "repo-private", true)
		}),
		"hidden": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoIsHiddenByName(ctx, tx, "repo-hidden", true)
		}),
		"collaborator": func(t *testing.T, r proto.Repository) {
			if err := be.AddCollaborator(ctx, r.Name(), "alice", access.ReadWriteAccess); err != nil {
				t.Fatal(err)
			}
		},
		"webhook": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				_, err := be.store.CreateWebhook(ctx, tx, models.Webhook{RepoID: r.ID(), URL: "https://example.com/hook", ContentType: 1, Active: true})
				return err
			})(t, r)
		},
		"lfs-object": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				return be.store.CreateLFSObject(ctx, tx, r.ID(), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 1)
			})(t, r)
		},
		"lfs-lock": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				_, err := be.store.CreateLFSLockForUser(ctx, tx, r.ID(), owner.ID(), "foo.png", "refs/heads/main")
				return err
			})(t, r)
		},
	}

	for name, use := range tests {
		t.Run(name, func(t *testing.T) {
			repoName := "repo-" + name
			r, err := be.CreateRepository(ctx, repoName, owner, proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if use != nil {
				use(t, r)
			}

			removed, err := be.RemovePushCreatedRepository(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			_, err = be.Repository(ctx, repoName)
			if use == nil {
				if !removed {
					t.Fatal("not true: removed")
				}
				if !errors.Is(err, proto.ErrRepoNotFound) {
					t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
				}
			} else {
				if removed {
					t.Fatal("not true: !removed")
				}
				if err != nil {
					t.Fatal(err)
				} // the used repository is kept
			}
		})
	}
}

// pushWaitsForGate reports whether a BeginPush waits for its repository's
// push gate, which a removal holds.
func pushWaitsForGate() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, frames, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[sync.RWMutex.RLock") && strings.Contains(frames, "pkg/backend.(*Backend).BeginPush(") {
			return true
		}
	}
	return false
}

// A repository is not removed while a push to it runs, and a push that
// starts while it is being removed waits for that and then finds it gone,
// rather than writing refs into a directory about to be deleted.
func TestRemovePushCreatedRepositoryOrdersPushes(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// A push is running: the repository stays.
	done, err := be.BeginPush(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("not true: !removed")
	}
	done()

	// A push starts during the removal, after the last check for refs.
	pushErr := make(chan error, 1)
	be.beforePushCreatedDelete = func() {
		go func() {
			done, err := be.BeginPush(ctx, r)
			if err == nil {
				done()
			}
			pushErr <- err
		}()
		// The push waits for the removal to end before it starts.
		deadline := time.Now().Add(time.Minute)
		for !pushWaitsForGate() {
			select {
			case err := <-pushErr:
				t.Errorf("push started during the removal: %v", err)
				pushErr <- err
				return
			default:
			}
			if time.Now().After(deadline) {
				t.Error("the push neither started nor waited for the removal")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	removed, err = be.RemovePushCreatedRepository(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("not true: removed")
	}
	if !errors.Is(<-pushErr, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(<-pushErr, proto.ErrRepoNotFound)")
	} // the push finds it gone
}

// A change that lands after the removal's checks for refs, and before the
// repository's row is deleted, still keeps it: the checks run in the
// transaction that deletes the row. A repository renamed away, with
// another created under its name, in that gap is not the one deleted.
func TestRemovePushCreatedRepositoryChecksWhenDeleting(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("collaborator", func(t *testing.T) {
		r, err := be.CreateRepository(ctx, "collab", owner, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		be.beforePushCreatedDelete = func() {
			if err := be.AddCollaborator(ctx, "collab", "alice", access.ReadWriteAccess); err != nil {
				t.Fatal(err)
			}
		}

		removed, err := be.RemovePushCreatedRepository(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Fatal("not true: !removed")
		}
		_, err = be.Repository(ctx, "collab")
		if err != nil {
			t.Fatal(err)
		} // kept with its collaborator
	})

	t.Run("recreated", func(t *testing.T) {
		r, err := be.CreateRepository(ctx, "name", owner, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var other proto.Repository
		be.beforePushCreatedDelete = func() {
			if err := be.RenameRepository(ctx, "name", "moved"); err != nil {
				t.Fatal(err)
			}
			other, err = be.CreateRepository(ctx, "name", owner, proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
		}

		removed, err := be.RemovePushCreatedRepository(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Fatal("not true: !removed")
		}
		cur, err := be.Repository(ctx, "name")
		if err != nil {
			t.Fatal(err)
		} // the repository now under the name is kept
		if got, want := cur.ID(), other.ID(); got != want {
			t.Fatalf("cur.ID() = %v, want %v", got, want)
		}
		_, err = be.Repository(ctx, "moved")
		if err != nil {
			t.Fatal(err)
		}
	})
}
