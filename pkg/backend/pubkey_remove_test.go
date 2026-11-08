package backend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

// RemoveOwnPublicKey refuses the account's only key, removes any other, and
// leaves the admin path (RemovePublicKey) free to remove the last key.
func TestRemoveOwnPublicKey(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	k1, k2 := newTestPublicKey(t), newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{k1, k2}})
	is.NoErr(err)

	is.NoErr(be.RemoveOwnPublicKey(ctx, "Alice", k2))
	is.True(errors.Is(be.RemoveOwnPublicKey(ctx, "alice", k1), ErrLastPublicKey))
	keys, err := be.ListPublicKeys(ctx, "alice")
	is.NoErr(err)
	is.Equal(len(keys), 1)

	// A key the account does not hold is not "the last key": nothing to
	// remove, nothing refused.
	is.NoErr(be.RemoveOwnPublicKey(ctx, "alice", k2))
	is.True(errors.Is(be.RemoveOwnPublicKey(ctx, "nobody", k1), proto.ErrUserNotFound))

	is.NoErr(be.RemovePublicKey(ctx, "alice", k1))
	keys, err = be.ListPublicKeys(ctx, "alice")
	is.NoErr(err)
	is.Equal(len(keys), 0)
}

// Two sessions of one user, on keys k1 and k2, each remove the other's key
// at the same moment. Both pass the SSH command's session-key check, so it
// is the backend's locked count that must refuse one of them: the account
// always keeps a key.
func TestRemoveOwnPublicKeyConcurrent(t *testing.T) {
	be, cfg := newTestBackend(t)
	testRemoveOwnPublicKeyConcurrent(t, be, backendTestContext(be, cfg))
}

// TestRemoveOwnPublicKeyConcurrentPostgres is TestRemoveOwnPublicKeyConcurrent
// on Postgres, whose READ COMMITTED transactions would both count two keys
// without the row lock.
func TestRemoveOwnPublicKeyConcurrentPostgres(t *testing.T) {
	e := newPostgresWebhookEnv(t)
	testRemoveOwnPublicKeyConcurrent(t, e.be, e.ctx)
}

func testRemoveOwnPublicKeyConcurrent(t *testing.T, be *Backend, ctx context.Context) {
	t.Helper()
	for i := range 20 {
		is := is.New(t)
		name := fmt.Sprintf("racer%d", i)
		k1, k2 := newTestPublicKey(t), newTestPublicKey(t)
		_, err := be.CreateUser(ctx, name, proto.UserOptions{PublicKeys: []ssh.PublicKey{k1, k2}})
		is.NoErr(err)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for j, k := range []ssh.PublicKey{k2, k1} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[j] = be.RemoveOwnPublicKey(ctx, name, k)
			}()
		}
		close(start)
		wg.Wait()

		refused := 0
		for _, err := range errs {
			switch {
			case err == nil:
			case errors.Is(err, ErrLastPublicKey):
				refused++
			default:
				t.Fatalf("round %d: unexpected error: %v", i, err)
			}
		}
		is.Equal(refused, 1)
		keys, err := be.ListPublicKeys(ctx, name)
		is.NoErr(err)
		is.Equal(len(keys), 1)
	}
}
