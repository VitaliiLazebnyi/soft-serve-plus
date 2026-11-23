package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// IsAdminKey reports whether pk is one of the configured admin keys
// (initial_admin_keys), which the config parses once, when it is validated.
func (d *Backend) IsAdminKey(pk ssh.PublicKey) bool {
	if pk == nil {
		return false
	}
	for _, k := range d.cfg.AdminKeys() {
		if proto.KeysEqual(pk, k) {
			return true
		}
	}
	return false
}

// IsServerAdmin reports whether the caller is a server admin: user has the
// admin flag, or the public key the SSH session authenticated with, read
// from ctx, is one of the configured admin keys. It is the one statement of
// that rule; every check that honours an admin key on the session context
// asks it. A check on a key given explicitly uses IsAdminKey.
//
// Admin keys are attached to an admin user on every start, but a key that a
// non-admin account already owns stays on that account (and such sessions
// are refused, see AdminKeyNonAdminOwnerError), and a key with no unambiguous
// admin to go to is not attached, so the key itself is checked too.
func (d *Backend) IsServerAdmin(ctx context.Context, user proto.User) bool {
	if isAccountAdmin(user) {
		return true
	}
	return d.IsAdminKey(proto.PublicKeyFromContext(ctx))
}

// isAccountAdmin reports whether user's account has the admin flag. Readable
// and the other account-only checks use it; see IsServerAdmin for the rule
// that also honours an admin key.
func isAccountAdmin(user proto.User) bool {
	return user != nil && user.IsAdmin()
}

// BaseAccessLevelForUser returns the access level the caller holds before
// any repository is considered: the level it would get for a repository
// that does not exist. user is the caller's resolved user, such as the user
// an SSH session stored in its context. A server admin (see IsServerAdmin),
// including the holder of an admin key on ctx, gets admin access.
//
// The error is set when the anon-access setting could not be read; the
// level is then NoAccess.
func (d *Backend) BaseAccessLevelForUser(ctx context.Context, user proto.User) (access.Level, error) {
	admin := d.IsServerAdmin(ctx, user)
	if admin {
		return access.AdminAccess, nil
	}
	anon, err := d.anonAccess(ctx)
	if err != nil {
		return access.NoAccess, err
	}
	return d.baseAccessLevel(user, false, anon), nil
}

// Readable looks a repository up once and returns it together with the
// access level of user's account. The repository is nil when it does not
// exist or when the caller may not read it, so callers cannot tell the two
// apart. The level is returned either way so a writer can decide whether a
// missing name may be created.
//
// The error is set only when a lookup failed, as when the database is
// down; the repository is then nil and the level NoAccess. It is not
// logged here: the caller answers the client and logs it, so a lookup that
// failed is not reported as a missing repository.
//
// Only the account counts, not an admin key on ctx: callers acting for an
// SSH session use ReadableForUser.
func (d *Backend) Readable(ctx context.Context, name string, user proto.User) (proto.Repository, access.Level, error) {
	return d.readable(ctx, name, user, isAccountAdmin(user))
}

// ReadableForUser is Readable for the caller of an SSH session: user is the
// user the session stored in its context, and a server admin (see
// IsServerAdmin), including the holder of an admin key on ctx, gets admin
// access to every name.
//
// The session user is the one resolved when the session authenticated, as
// for the account commands and BaseAccessLevelForUser: a key removed from
// that account mid-session keeps the session's user until it reconnects.
// Collaborator rows, the repository's privacy and ownership are still read
// fresh on every call.
func (d *Backend) ReadableForUser(ctx context.Context, name string, user proto.User) (proto.Repository, access.Level, error) {
	return d.readable(ctx, name, user, d.IsServerAdmin(ctx, user))
}

// readable is Readable with the caller's admin status already decided.
func (d *Backend) readable(ctx context.Context, name string, user proto.User, admin bool) (proto.Repository, access.Level, error) {
	name = proto.SanitizeRepo(name)
	r, collab, err := d.repositoryWithCollab(ctx, name, user)
	if err != nil {
		return nil, access.NoAccess, fmt.Errorf("look up repository %q: %w", name, err)
	}

	level, err := d.accessLevel(ctx, r, collab, name, user, admin)
	if err != nil {
		return nil, access.NoAccess, err
	}
	if r == nil || level < access.ReadOnlyAccess {
		return nil, level, nil
	}

	return r, level, nil
}

// baseAccessLevel is the level user holds for a repository that does not
// exist, given whether the caller is an admin and the anonymous level.
//
// Admins get admin access whatever the flag.
//
// With AdminOnlyCreate set, only admins may create repositories: registered
// non-admins get read-only, which keeps their SSH session valid (session.go
// needs >= read-only) but fails every create path: repo create, repo import,
// push-to-create and rename-to-a-new-name all need read-write. The anonymous
// level is capped the same way.
//
// Without it, this is upstream behaviour: registered users get read-write on
// a missing name, and anonymous users get the anonymous level.
func (d *Backend) baseAccessLevel(user proto.User, admin bool, anon access.Level) access.Level {
	adminOnly := d.cfg.AdminOnlyCreate
	switch {
	case admin:
		return access.AdminAccess
	case adminOnly && (user != nil || anon > access.ReadOnlyAccess):
		// Never read-write or above on a missing name, so never a create.
		return access.ReadOnlyAccess
	case user != nil && anon > access.ReadWriteAccess:
		return anon
	case user != nil:
		return access.ReadWriteAccess
	}
	return anon
}

// collabRow is what the access check reads of a repository's row: whether
// there is one, and the caller's collaborator row on it, its level and
// whether there is one.
type collabRow struct {
	row   bool
	level access.Level
	ok    bool
}

// repositoryWithCollab is Repository with user's collaborator row on the
// repository read in the same query. The query runs for every name, missing
// ones and ones with no directory included, and is the only lookup of the
// repository: an existing private repository and a missing name then cost
// the same lookups that can fail (this one, and the anon-access setting),
// and fail alike, so an outage does not tell the two apart. A repository
// that is not found, for want of a row or of a directory, is nil with no
// error; collab.row says whether there is a row.
func (d *Backend) repositoryWithCollab(ctx context.Context, name string, user proto.User) (proto.Repository, collabRow, error) {
	var userID int64
	if user != nil {
		userID = user.ID()
	}
	m, level, ok, err := d.store.GetRepoWithCollabByName(ctx, d.db, name, userID)
	switch {
	case errors.Is(err, db.ErrRecordNotFound):
		return nil, collabRow{}, nil
	case err != nil:
		return nil, collabRow{}, err
	}
	collab := collabRow{row: true, level: level, ok: ok && userID != 0}
	rp := d.repoPath(name)
	if !d.repoDirFound(rp) {
		return nil, collab, nil
	}
	return &repo{name: name, path: rp, repo: m, logger: d.logger}, collab, nil
}

// accessLevel computes the access level of user for r, which is the
// repository named name or nil when it does not exist, and on which user's
// collaborator row is collab. admin is whether the caller is an admin, as
// its caller decided. The error is set when a lookup failed.
func (d *Backend) accessLevel(ctx context.Context, r proto.Repository, collab collabRow, name string, user proto.User, admin bool) (access.Level, error) {
	if admin {
		return access.AdminAccess, nil
	}

	// A failed lookup is returned on every path, missing names included, so
	// an outage answers a missing and an existing name alike.
	anon, err := d.anonAccess(ctx)
	if err != nil {
		return access.NoAccess, err
	}
	if r == nil {
		// A directory with no row is taken (case variant or orphan), not free.
		// So is a row with no directory (removed by hand, a partial restore
		// or an unmounted volume): its settings are unknown to the caller,
		// so it must not fall back to the level for a free name, which would
		// show a private repository in listings that walk the rows.
		if collab.row || d.repoPathExists(name) {
			return access.NoAccess, nil
		}
		return d.baseAccessLevel(user, false, anon), nil
	}

	return repoAccessLevel(r, user, anon, collab.level, collab.ok), nil
}

// repoAccessLevel is accessLevel for an existing repository r once the
// anonymous level and user's collaborator row for r, if any, are known.
func repoAccessLevel(r proto.Repository, user proto.User, anon, collabAccess access.Level, isCollab bool) access.Level {
	// If the user is an admin, they have admin access.
	if isAccountAdmin(user) {
		return access.AdminAccess
	}

	// If the user is the owner, they have admin access.
	if user != nil && r.UserID() == user.ID() {
		return access.AdminAccess
	}

	// On a public repository the anonymous level is a floor for everyone:
	// a registered user, collaborator or not, never gets less than an
	// anonymous caller could. On a private repository it does not apply.
	//
	// If the user is a collaborator, they have their access level.
	if isCollab {
		if !r.IsPrivate() && anon > collabAccess {
			return anon
		}
		return collabAccess
	}

	// If the repository is private, the user has no access.
	if r.IsPrivate() {
		return access.NoAccess
	}

	// Otherwise, anonymous users have the anonymous level, and registered
	// users read-only or the anonymous level, whichever is higher.
	if user == nil || anon > access.ReadOnlyAccess {
		return anon
	}

	return access.ReadOnlyAccess
}

// ReadableRepositories returns every repository user may read. It gives
// each the level Readable would, but loads the anonymous level and the
// user's collaborations once for the whole list instead of once per
// repository. Like Readable, it leaves out a row whose directory is gone
// for everyone but admins, who are listed every row.
func (d *Backend) ReadableRepositories(ctx context.Context, user proto.User) ([]proto.Repository, error) {
	repos, err := d.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	if isAccountAdmin(user) {
		return repos, nil
	}

	anon, err := d.anonAccess(ctx)
	if err != nil {
		return nil, err
	}
	var collabs map[int64]access.Level
	if user != nil && user.ID() != 0 {
		ms, err := d.store.ListCollabsByUserID(ctx, d.db, user.ID())
		if err != nil {
			return nil, err
		}
		collabs = make(map[int64]access.Level, len(ms))
		for _, m := range ms {
			collabs[m.RepoID] = m.AccessLevel
		}
	}

	readable := make([]proto.Repository, 0, len(repos))
	for _, r := range repos {
		// A row with no directory is NoAccess for every non-admin, owner
		// and collaborators included (see accessLevel).
		if !d.repoPathExists(r.Name()) {
			continue
		}
		collabAccess, isCollab := collabs[r.ID()]
		if repoAccessLevel(r, user, anon, collabAccess, isCollab) >= access.ReadOnlyAccess {
			readable = append(readable, r)
		}
	}
	return readable, nil
}

// ReadableRepositoriesForUser is ReadableRepositories for the caller of an
// SSH session: user is the user the session stored in its context, and a
// server admin (see IsServerAdmin), including the holder of an admin key on
// ctx, is listed every repository. The session's key is not looked up again,
// as for ReadableForUser, so a failed lookup cannot turn the list into the
// anonymous one; an error listing the repositories is returned.
func (d *Backend) ReadableRepositoriesForUser(ctx context.Context, user proto.User) ([]proto.Repository, error) {
	if d.IsServerAdmin(ctx, user) {
		return d.Repositories(ctx)
	}
	return d.ReadableRepositories(ctx, user)
}

// User finds a user by username.
func (d *Backend) User(ctx context.Context, username string) (proto.User, error) {
	username = strings.ToLower(username)
	if err := proto.ValidateUsername(username); err != nil {
		return nil, err
	}

	var m models.User
	var pks []ssh.PublicKey
	if err := d.db.ReadTransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		m, err = d.store.FindUserByUsername(ctx, tx, username)
		if err != nil {
			return err
		}

		pks, err = d.store.ListPublicKeysByUserID(ctx, tx, m.ID)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return nil, proto.ErrUserNotFound
		}
		return nil, err
	}

	return &user{
		user:       m,
		publicKeys: pks,
	}, nil
}

// UserByID finds a user by ID.
func (d *Backend) UserByID(ctx context.Context, id int64) (proto.User, error) {
	var m models.User
	var pks []ssh.PublicKey
	if err := d.db.ReadTransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		m, err = d.store.GetUserByID(ctx, tx, id)
		if err != nil {
			return err
		}

		pks, err = d.store.ListPublicKeysByUserID(ctx, tx, m.ID)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return nil, proto.ErrUserNotFound
		}
		return nil, err
	}

	return &user{
		user:       m,
		publicKeys: pks,
	}, nil
}

// AdminKeyNonAdminOwnerError is returned by UserByPublicKey for a key that is
// listed in initial_admin_keys but attached to a non-admin account. The SSH
// server refuses sessions with such a key and shows this error. It matches
// proto.ErrUserNotFound with errors.Is, since the key resolves to no
// account.
type AdminKeyNonAdminOwnerError struct {
	// Username is the non-admin account the key is attached to.
	Username string
}

// Error implements error.
func (e *AdminKeyNonAdminOwnerError) Error() string {
	return fmt.Sprintf("this key is listed in initial_admin_keys but is attached to non-admin account %q. "+
		"If that account is yours, ask an admin to run `user set-admin %s true`; "+
		"otherwise ask an admin to remove the key from it with `user remove-pubkey %s KEY`",
		e.Username, e.Username, e.Username)
}

// Unwrap makes the error match proto.ErrUserNotFound.
func (e *AdminKeyNonAdminOwnerError) Unwrap() error { return proto.ErrUserNotFound }

// UserByPublicKey finds a user by public key.
func (d *Backend) UserByPublicKey(ctx context.Context, pk ssh.PublicKey) (proto.User, error) {
	var m models.User
	var pks []ssh.PublicKey
	if err := d.db.ReadTransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		m, err = d.store.FindUserByPublicKey(ctx, tx, pk)
		if err != nil {
			return err
		}

		pks, err = d.store.ListPublicKeysByUserID(ctx, tx, m.ID)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return nil, proto.ErrUserNotFound
		}
		return nil, err
	}

	// A key listed in initial_admin_keys but owned by a non-admin account
	// resolves to no account and the SSH server refuses sessions with it
	// (see AdminKeyNonAdminOwnerError). `pubkey add` takes a public key
	// without proof of holding its private half, so the owner may have
	// claimed someone else's key before it was listed; acting as the owner
	// would hand them what the key holder does. Acting as nobody instead
	// strands a legitimate owner without their account, so the session is
	// refused with an error saying how to fix it.
	if !m.Admin && d.IsAdminKey(pk) {
		d.logger.Warn("admin key is owned by a non-admin account; refusing sessions with it until an admin runs `user set-admin` on that account or removes the key from it",
			"username", m.Username, "pk", proto.MarshalAuthorizedKey(pk))
		return nil, &AdminKeyNonAdminOwnerError{Username: m.Username}
	}

	return &user{
		user:       m,
		publicKeys: pks,
	}, nil
}

// UserByAccessToken finds a user by access token.
// This also validates the token for expiration and returns proto.ErrTokenExpired.
func (d *Backend) UserByAccessToken(ctx context.Context, token string) (proto.User, error) {
	var m models.User
	var pks []ssh.PublicKey
	token = HashToken(token)

	if err := d.db.ReadTransactionContext(ctx, func(tx *db.Tx) error {
		t, err := d.store.GetAccessTokenByToken(ctx, tx, token)
		if err != nil {
			return err
		}

		if t.ExpiresAt.Valid && t.ExpiresAt.Time.Before(time.Now()) {
			return proto.ErrTokenExpired
		}

		// The token row names its user; look the user up by that ID.
		m, err = d.store.GetUserByID(ctx, tx, t.UserID)
		if err != nil {
			return err
		}

		pks, err = d.store.ListPublicKeysByUserID(ctx, tx, m.ID)
		return err
	}); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return nil, proto.ErrUserNotFound
		}
		return nil, err
	}

	return &user{
		user:       m,
		publicKeys: pks,
	}, nil
}

// Users returns all users.
func (d *Backend) Users(ctx context.Context) ([]string, error) {
	ms, err := d.store.GetAllUsers(ctx, d.db)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, m := range ms {
		users = append(users, m.Username)
	}
	return users, nil
}

// AddPublicKey adds a public key to a user.
func (d *Backend) AddPublicKey(ctx context.Context, username string, pk ssh.PublicKey) error {
	username = strings.ToLower(username)
	if err := proto.ValidateUsername(username); err != nil {
		return err
	}

	return d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// The row lock keeps the user from being deleted before the insert,
		// which would then fail on its foreign key.
		m, err := d.store.FindUserByUsernameForUpdate(ctx, tx, username)
		if err != nil {
			return userNotFound(err)
		}
		return d.store.AddPublicKeyByUserID(ctx, tx, m.ID, pk)
	})
}

// ErrSessionKeyRemoved is returned by AddOwnPublicKey when the key the
// session authenticated with no longer belongs to the session's account.
var ErrSessionKeyRemoved = errors.New("the key this session authenticated with is no longer on your account")

// AddOwnPublicKey adds a public key to user's own account, found by its ID:
// user is the session's user, resolved when the session authenticated with
// sessionKey. A lookup by its name could find another account that took the
// name after a rename, and give that account the key.
//
// The key is added only while sessionKey still belongs to the account, so a
// session whose key an admin removed, as when it was compromised, cannot add
// a key of its own choosing in its place; it gets ErrSessionKeyRemoved.
func (d *Backend) AddOwnPublicKey(ctx context.Context, user proto.User, sessionKey, pk ssh.PublicKey) error {
	return d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// The row lock keeps the user from being deleted before the insert,
		// which would then fail on its foreign key, and holds off the
		// removal of its keys (RemovePublicKey and RemoveOwnPublicKey take
		// the same lock), so the check below holds until the commit.
		if _, err := d.store.GetUserByIDForUpdate(ctx, tx, user.ID()); err != nil {
			return userNotFound(err)
		}
		if sessionKey == nil {
			return ErrSessionKeyRemoved
		}
		owner, err := d.store.FindUserByPublicKey(ctx, tx, sessionKey)
		switch {
		case errors.Is(err, db.ErrRecordNotFound):
			return ErrSessionKeyRemoved
		case err != nil:
			return err
		case owner.ID != user.ID():
			return ErrSessionKeyRemoved
		}
		return d.store.AddPublicKeyByUserID(ctx, tx, user.ID(), pk)
	})
}

// CreateUser creates a new user.
func (d *Backend) CreateUser(ctx context.Context, username string, opts proto.UserOptions) (proto.User, error) {
	username = termtext.Sanitize(username)
	username = strings.ToLower(username)
	if err := proto.ValidateNewUsername(username); err != nil {
		return nil, err
	}

	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return d.store.CreateUser(ctx, tx, username, opts.Admin, opts.PublicKeys)
	}); err != nil {
		return nil, err
	}

	return d.User(ctx, username)
}

// DeleteUser deletes a user.
func (d *Backend) DeleteUser(ctx context.Context, username string) error {
	username = strings.ToLower(username)
	if err := proto.ValidateUsername(username); err != nil {
		return err
	}

	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// Lock the user's row before looking for their repositories: on
		// Postgres a repository inserted meanwhile would otherwise be
		// missed here and then removed by the cascade below, its
		// directory left behind. The lock waits for such an insert to
		// commit, so the query below sees it, and keeps new ones out
		// until the user is gone.
		user, err := d.store.FindUserByUsernameForUpdate(ctx, tx, username)
		if err != nil {
			return err
		}

		// Deleting the user would cascade to their repositories' rows and
		// strand the repositories on disk, so their owner must deal with
		// them first.
		repos, err := d.store.GetUserRepos(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		if len(repos) > 0 {
			names := make([]string, len(repos))
			for i, r := range repos {
				names[i] = r.Name
			}
			return fmt.Errorf("%w: %s", proto.ErrUserOwnsRepos, strings.Join(names, ", "))
		}

		return d.store.DeleteUserByUsername(ctx, tx, username)
	})
	if errors.Is(err, db.ErrRecordNotFound) {
		return proto.ErrUserNotFound
	}
	return err
}

// RemovePublicKey removes a public key from a user.
func (d *Backend) RemovePublicKey(ctx context.Context, username string, pk ssh.PublicKey) error {
	username = strings.ToLower(username)
	if err := proto.ValidateUsername(username); err != nil {
		return err
	}

	return d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		// The row lock keeps the user from being deleted before the
		// removal, which would then report the key, not the user, as
		// missing.
		m, err := d.store.FindUserByUsernameForUpdate(ctx, tx, username)
		if err != nil {
			return userNotFound(err)
		}
		return publicKeyNotFound(d.store.RemovePublicKeyByUserID(ctx, tx, m.ID, pk))
	})
}

// ErrLastPublicKey is returned by RemoveOwnPublicKey when the key is the
// account's only public key.
var ErrLastPublicKey = errors.New("cannot remove the account's last public key")

// RemoveOwnPublicKey is RemovePublicKey for user, the session's user,
// removing a key from their own account, found by its ID as in
// AddOwnPublicKey: it refuses, with ErrLastPublicKey, to remove the
// account's only key, which would leave no way to sign in over SSH.
//
// The count and the removal happen in one transaction that holds the user's
// row (GetUserByIDForUpdate takes the row lock on Postgres; SQLite
// transactions begin IMMEDIATE and take turns), so two sessions of one user
// that each remove the other's key at once cannot both pass the check and
// leave the account with no keys: the second sees one key left and is
// refused. The lock also keeps the user from being deleted before the
// removal.
func (d *Backend) RemoveOwnPublicKey(ctx context.Context, user proto.User, pk ssh.PublicKey) error {
	return d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := d.store.GetUserByIDForUpdate(ctx, tx, user.ID()); err != nil {
			return userNotFound(err)
		}
		keys, err := d.store.ListPublicKeysByUserID(ctx, tx, user.ID())
		if err != nil {
			return err
		}
		if len(keys) <= 1 && slices.ContainsFunc(keys, func(k ssh.PublicKey) bool {
			return proto.KeysEqual(k, pk)
		}) {
			return ErrLastPublicKey
		}
		return publicKeyNotFound(d.store.RemovePublicKeyByUserID(ctx, tx, user.ID(), pk))
	})
}

// SetUsername sets the username of a user.
func (d *Backend) SetUsername(ctx context.Context, username string, newUsername string) error {
	username = strings.ToLower(username)
	if err := proto.ValidateUsername(username); err != nil {
		return err
	}

	return userNotFound(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			return d.store.SetUsernameByUsername(ctx, tx, username, newUsername)
		}),
	)
}

// SetOwnUsername renames user's own account, found by its ID as in
// AddOwnPublicKey, so a session whose account was renamed meanwhile renames
// its own account, not one that took the old name.
func (d *Backend) SetOwnUsername(ctx context.Context, user proto.User, newUsername string) error {
	return userNotFound(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			return d.store.SetUsernameByUserID(ctx, tx, user.ID(), newUsername)
		}),
	)
}

// SetAdmin sets the admin flag of a user.
func (d *Backend) SetAdmin(ctx context.Context, username string, admin bool) error {
	username = strings.ToLower(username)
	if err := proto.ValidateUsername(username); err != nil {
		return err
	}

	return userNotFound(
		d.db.TransactionContext(ctx, func(tx *db.Tx) error {
			return d.store.SetAdminByUsername(ctx, tx, username, admin)
		}),
	)
}

type user struct {
	user       models.User
	publicKeys []ssh.PublicKey
}

var _ proto.User = (*user)(nil)

// IsAdmin implements proto.User
func (u *user) IsAdmin() bool {
	return u.user.Admin
}

// PublicKeys implements proto.User
func (u *user) PublicKeys() []ssh.PublicKey {
	return u.publicKeys
}

// Username implements proto.User
func (u *user) Username() string {
	return u.user.Username
}

// ID implements proto.User.
func (u *user) ID() int64 {
	return u.user.ID
}

// userNotFound reports a missing row as proto.ErrUserNotFound.
// It is for changes made by username, where the user is the only row that
// can be missing.
func userNotFound(err error) error {
	if errors.Is(err, db.ErrRecordNotFound) {
		return proto.ErrUserNotFound
	}
	return err
}

// publicKeyNotFound maps the error of removing a key the user does not have
// to proto.ErrPublicKeyNotFound.
func publicKeyNotFound(err error) error {
	if errors.Is(err, db.ErrRecordNotFound) {
		return proto.ErrPublicKeyNotFound
	}
	return err
}
