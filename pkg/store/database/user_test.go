package database

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// TestFindDefaultAdminUserID verifies that the default admin is the
// lowest-ID admin, skipping lower-ID non-admins, and that db.ErrRecordNotFound is
// returned when no user is an admin.
func TestFindDefaultAdminUserID(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	// The migration seeds the initial "admin" user; demote it so the test
	// controls every admin row.
	_, err := dbx.ExecContext(ctx, `UPDATE users SET admin = false;`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.FindDefaultAdminUserID(ctx, dbx)
	if !errors.Is(err, db.ErrRecordNotFound) {
		t.Fatal("not true: errors.Is(err, db.ErrRecordNotFound)")
	}

	_, err = dbx.ExecContext(ctx, `INSERT INTO users (id, username, admin, updated_at) VALUES
		(200, 'plain1', false, CURRENT_TIMESTAMP),
		(300, 'admin300', true, CURRENT_TIMESTAMP),
		(250, 'admin250', true, CURRENT_TIMESTAMP),
		(400, 'plain2', false, CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}

	id, err := s.FindDefaultAdminUserID(ctx, dbx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := id, int64(250); got != want {
		t.Fatalf("id = %v, want %v", got, want)
	}
}

// A stored key that does not parse is skipped and logged on the store's
// logger, not on one taken from the context.
func TestListPublicKeysLogsOnStoreLogger(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	var logs, ctxLogs bytes.Buffer
	s := New(log.New(&logs))
	ctx = log.WithContext(ctx, log.New(&ctxLogs))
	_, err := dbx.ExecContext(ctx, `INSERT INTO public_keys (user_id, public_key, updated_at) VALUES (1, 'not a key', CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
	pks, err := s.ListPublicKeysByUserID(ctx, dbx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(pks); got != 0 {
		t.Fatalf("len(pks) = %v, want %v", got, 0)
	}
	if !strings.Contains(logs.String(), "skipping unparsable stored public key") {
		t.Fatal("not true: strings.Contains(logs.String(), \"skipping unparsable stored public key\")")
	}
	if !strings.Contains(logs.String(), "store") {
		t.Fatal("not true: strings.Contains(logs.String(), \"store\")")
	}
	if got := ctxLogs.String(); got != "" {
		t.Fatalf("ctxLogs.String() = %v, want %v", got, "")
	}
}

func TestUserStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	k1, k2, k3 := newPublicKey(t), newPublicKey(t), newPublicKey(t)

	// Usernames are stored lower-cased and looked up case-insensitively.
	if err := s.CreateUser(ctx, dbx, "Alice", false, []ssh.PublicKey{k1, k2}); err != nil {
		t.Fatal(err)
	}
	u, err := s.FindUserByUsername(ctx, dbx, "ALICE")
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" || u.Admin {
		t.Errorf("FindUserByUsername() = %+v, want non-admin alice", u)
	}
	if got, err := s.GetUserByID(ctx, dbx, u.ID); err != nil || got.Username != "alice" {
		t.Errorf("GetUserByID(%d) = %+v, %v, want alice", u.ID, got, err)
	}
	if got, err := s.FindUserByPublicKey(ctx, dbx, k2); err != nil || got.ID != u.ID {
		t.Errorf("FindUserByPublicKey(k2) = %+v, %v, want alice", got, err)
	}
	if _, err := s.FindUserByPublicKey(ctx, dbx, k3); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("FindUserByPublicKey(unknown key) error = %v, want ErrRecordNotFound", err)
	}

	// Keys are listed in the order they were added.
	if err := s.AddPublicKeyByUserID(ctx, dbx, u.ID, k3); err != nil {
		t.Fatal(err)
	}
	pks, err := s.ListPublicKeysByUserID(ctx, dbx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameKeys(pks, []ssh.PublicKey{k1, k2, k3}) {
		t.Errorf("ListPublicKeysByUserID() = %d keys, want k1, k2, k3 in order", len(pks))
	}
	if err := s.RemovePublicKeyByUserID(ctx, dbx, u.ID, k2); err != nil {
		t.Fatal(err)
	}
	pks, err = s.ListPublicKeysByUserID(ctx, dbx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameKeys(pks, []ssh.PublicKey{k1, k3}) {
		t.Errorf("ListPublicKeysByUserID() after removing k2 = %d keys, want k1, k3", len(pks))
	}

	// A key belongs to one account only.
	if err := s.CreateUser(ctx, dbx, "bob", false, nil); err != nil {
		t.Fatal(err)
	}
	bob, err := s.FindUserByUsername(ctx, dbx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddPublicKeyByUserID(ctx, dbx, bob.ID, k1); !errors.Is(err, db.ErrDuplicateKey) {
		t.Errorf("AddPublicKeyByUserID(bob, alice's key) error = %v, want ErrDuplicateKey", err)
	}
	// By ID, the row is the account's whatever its name now is.
	if err := s.SetUsernameByUserID(ctx, dbx, bob.ID, "Robert"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetUserByIDForUpdate(ctx, dbx, bob.ID); err != nil || got.Username != "robert" {
		t.Errorf("GetUserByIDForUpdate(bob) = %+v, %v, want robert", got, err)
	}
	if err := s.SetUsernameByUserID(ctx, dbx, bob.ID, "alice"); !errors.Is(err, db.ErrDuplicateKey) {
		t.Errorf("SetUsernameByUserID(bob, alice) error = %v, want ErrDuplicateKey", err)
	}
	if err := s.SetUsernameByUserID(ctx, dbx, 999999, "nobody"); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("SetUsernameByUserID(missing) error = %v, want ErrRecordNotFound", err)
	}
	if err := s.RemovePublicKeyByUserID(ctx, dbx, bob.ID, k1); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("RemovePublicKeyByUserID(bob, alice's key) error = %v, want ErrRecordNotFound", err)
	}
	if err := s.SetUsernameByUserID(ctx, dbx, bob.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, dbx, "BOB", false, nil); !errors.Is(err, db.ErrDuplicateKey) {
		t.Errorf("CreateUser(BOB) with bob taken: error = %v, want ErrDuplicateKey", err)
	}
	if err := s.CreateUser(ctx, dbx, "-bad name", false, nil); err == nil {
		t.Error("CreateUser(invalid username) = nil error, want a validation error")
	}

	if err := s.SetAdminByUsername(ctx, dbx, "alice", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUsernameByUsername(ctx, dbx, "alice", "Carol"); err != nil {
		t.Fatal(err)
	}
	u, err = s.FindUserByUsername(ctx, dbx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Admin {
		t.Errorf("carol = %+v, want an admin", u)
	}
	if _, err := s.FindUserByUsername(ctx, dbx, "alice"); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("FindUserByUsername(alice) after rename: error = %v, want ErrRecordNotFound", err)
	}

	users, err := s.GetAllUsers(ctx, dbx)
	if err != nil {
		t.Fatal(err)
	}
	// admin (seeded), owner (newWebhookTestDB), carol, bob.
	if len(users) != 4 {
		t.Errorf("GetAllUsers() = %d users, want 4", len(users))
	}

	// Deleting a user deletes their keys with them.
	if err := s.DeleteUserByUsername(ctx, dbx, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindUserByPublicKey(ctx, dbx, k1); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("FindUserByPublicKey(k1) after deleting its user: error = %v, want ErrRecordNotFound", err)
	}
}

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
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	good1 := newTestPublicKey(t)
	good2 := newTestPublicKey(t)
	if err := s.AddPublicKeyByUserID(ctx, dbx, 100, good1); err != nil {
		t.Fatal(err)
	}
	_, err := dbx.ExecContext(ctx, dbx.Rebind(`INSERT INTO public_keys (user_id, public_key, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP);`), 100, "ssh-ed25519 not-base64!!")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddPublicKeyByUserID(ctx, dbx, 100, good2); err != nil {
		t.Fatal(err)
	}

	check := func(pks []ssh.PublicKey, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if got := len(pks); got != 2 {
			t.Fatalf("len(pks) = %v, want %v", got, 2)
		}
		if !proto.KeysEqual(pks[0], good1) {
			t.Fatal("not true: proto.KeysEqual(pks[0], good1)")
		}
		if !proto.KeysEqual(pks[1], good2) {
			t.Fatal("not true: proto.KeysEqual(pks[1], good2)")
		}
	}
	check(s.ListPublicKeysByUserID(ctx, dbx, 100))
}
