package backend

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// maxMirrorErrorLen caps the error RecordMirrorSync keeps: it can carry what
// the remote wrote to git's stderr.
const maxMirrorErrorLen = 1 << 10

// MirrorSync is the outcome of a mirror's syncs.
type MirrorSync struct {
	// SyncedAt is when a sync last succeeded; zero if none has.
	SyncedAt time.Time
	// Error is the error of the last sync if it failed, and empty if it
	// succeeded.
	Error string
}

// RecordMirrorSync records the outcome of a sync of the mirror name: syncErr
// is its error, or nil for a sync that succeeded.
func (d *Backend) RecordMirrorSync(ctx context.Context, name string, syncErr error) error {
	msg := ""
	if syncErr != nil {
		msg = syncErr.Error()
		if msg == "" {
			msg = "sync failed"
		}
		if len(msg) > maxMirrorErrorLen {
			cut := maxMirrorErrorLen
			for cut > 0 && !utf8.RuneStart(msg[cut]) {
				cut--
			}
			msg = msg[:cut] + "... (truncated)"
		}
	}
	return wrapRepoError(d.store.SetRepoMirrorSyncByName(ctx, d.db, utils.SanitizeRepo(name), msg))
}

// MirrorSyncStatus returns the outcome of the syncs of the mirror name.
func (d *Backend) MirrorSyncStatus(ctx context.Context, name string) (MirrorSync, error) {
	m, err := d.store.GetRepoByName(ctx, d.db, utils.SanitizeRepo(name))
	if err != nil {
		return MirrorSync{}, wrapRepoError(err)
	}
	return MirrorSync{SyncedAt: m.MirrorSyncedAt.Time, Error: m.MirrorError}, nil
}
