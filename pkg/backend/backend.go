package backend

import (
	"context"
	"fmt"
	"sync"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"golang.org/x/crypto/ssh"
)

// Backend is the Soft Serve backend that handles users, repositories, and
// server settings management and operations.
type Backend struct {
	ctx    context.Context
	cfg    *config.Config
	db     *db.DB
	store  store.Store
	logger *log.Logger

	// importGate reports why imports and mirror syncs are off, or "" when
	// they are allowed. It runs the git version check on first use, so
	// nothing has to happen in a particular order at startup for the gate
	// to be in place.
	importGate importGate

	// deliveries runs the webhook deliveries in the background, bounded, and
	// tracks them so shutdown can wait for them.
	deliveries *deliveryQueue

	// adminKeys caches the parsed initial_admin_keys; see adminKeyList.
	adminKeysMu  sync.Mutex
	adminKeysSrc []string
	adminKeys    []ssh.PublicKey
	adminKeysOK  bool

	// warnedOwnedAdminKeys maps the authorized-key string of a listed admin
	// key already logged as owned by a non-admin account to that account's
	// name, so the warning is logged once per key and owner rather than on
	// every lookup. An entry is dropped once the conflict is resolved (the
	// owner is promoted or the key leaves the account) and the whole map is
	// cleared when initial_admin_keys changes, so a conflict that comes back
	// is logged again and the map holds only currently listed keys. See
	// warnOwnedAdminKey and forgetOwnedAdminKey.
	warnedOwnedAdminKeysMu sync.Mutex
	warnedOwnedAdminKeys   map[string]string

	// pushGates orders pushes against the removal of repositories created
	// by an HTTP push; see BeginPush and RemovePushCreatedRepository.
	pushGates pushGates

	// repoDirs records the repository directories that a create, import,
	// rename or delete in progress owns without a database row, so a delete
	// does not take them for orphans.
	repoDirs repoDirHolds
}

// New returns a new Soft Serve backend.
func New(ctx context.Context, cfg *config.Config, db *db.DB, st store.Store) *Backend {
	logger := log.FromContext(ctx).WithPrefix("backend")
	b := &Backend{
		ctx:    ctx,
		cfg:    cfg,
		db:     db,
		store:  st,
		logger: logger,

		deliveries: newDeliveryQueue(maxBackgroundDeliveries, maxQueuedDeliveries, logger),
	}
	return b
}

// ImportsDisabledReason returns why repository imports and mirror syncs are
// off, or "" when they are allowed. The reason is for the server log; users
// get proto.ErrImportsDisabled.
func (d *Backend) ImportsDisabledReason() string {
	return d.importGate.reason()
}

// importGate caches the outcome of the git version check once git's version
// has been read. A failure to read it (git missing, or a transient exec
// error such as EAGAIN under load) closes the gate only for that call, so
// the check runs again next time rather than leaving imports and mirror
// syncs off until restart.
type importGate struct {
	mu     sync.Mutex
	done   bool
	cached string
}

func (g *importGate) reason() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done {
		return g.cached
	}
	reason, ok := checkGitVersion()
	if ok {
		g.done, g.cached = true, reason
	}
	return reason
}

// minImportGitVersion is the oldest git that honours the settings the import
// and mirror guards rely on: http.curloptResolve (2.37) for pinning the
// validated address and http.followRedirects via GIT_CONFIG_* (2.31). Older
// git silently ignores unknown keys, so on 2.31 to 2.36 the pin would be
// dropped and the guard open to DNS rebinding.
var minImportGitVersion = git.Version{Major: 2, Minor: 37}

// binVersion is how the gate learns the installed git version. Tests swap it
// out to exercise the disabled path without an old git on hand.
var binVersion = git.BinVersion

// checkGitVersion returns why imports must stay off, or "" if the installed
// git is new enough, and whether git's version was read. Running a clone
// under a git that ignores the guards would be worse than refusing it, so an
// unreadable version also closes the gate, but only until the next check.
func checkGitVersion() (reason string, ok bool) {
	v, err := binVersion()
	if err != nil {
		return fmt.Sprintf("cannot determine git version: %v", err), false
	}
	if v.Less(minImportGitVersion) {
		return fmt.Sprintf("git %s is older than %s, which imports and mirror syncs need to be safe", v, minImportGitVersion), true
	}
	return "", true
}
