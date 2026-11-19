package database

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

func newTestPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

// A stored key that no longer parses is skipped, so the user's other keys
// still work instead of every lookup failing.
func TestListPublicKeysSkipsUnparsableKey(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	good1 := newTestPublicKey(t)
	good2 := newTestPublicKey(t)
	is.NoErr(s.AddPublicKeyByUserID(ctx, dbx, 100, good1))
	_, err := dbx.ExecContext(ctx, dbx.Rebind(`INSERT INTO public_keys (user_id, public_key, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP);`), 100, "ssh-ed25519 not-base64!!")
	is.NoErr(err)
	is.NoErr(s.AddPublicKeyByUserID(ctx, dbx, 100, good2))

	check := func(pks []ssh.PublicKey, err error) {
		t.Helper()
		is.NoErr(err)
		is.Equal(len(pks), 2)
		is.True(sshutils.KeysEqual(pks[0], good1))
		is.True(sshutils.KeysEqual(pks[1], good2))
	}
	check(s.ListPublicKeysByUserID(ctx, dbx, 100))
}

// A non-positive limit returns no locks on every driver. SQLite would
// otherwise treat a negative LIMIT as no limit at all.
func TestGetLFSLocksNonPositiveLimit(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "a.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "b.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}

	for _, limit := range []int{0, -1} {
		locks, err := s.GetLFSLocks(ctx, dbx, 7, 0, limit)
		is.NoErr(err)
		is.Equal(len(locks), 0)
	}

	locks, err := s.GetLFSLocks(ctx, dbx, 7, 0, 1)
	is.NoErr(err)
	is.Equal(len(locks), 1)
	locks, err = s.GetLFSLocks(ctx, dbx, 7, 0, 10)
	is.NoErr(err)
	is.Equal(len(locks), 2)
}
