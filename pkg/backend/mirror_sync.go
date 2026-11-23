package backend

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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

// RecordMirrorSync records the outcome of a sync of the mirror r: syncErr
// is its error, or nil for a sync that succeeded. It records it on r's row,
// by ID, wherever r is now; a mirror deleted meanwhile is
// proto.ErrRepoNotFound. After a sync that succeeded it also records when
// the mirrored history last changed (see UpdatedAt): git fetch runs no
// hooks.
func (d *Backend) RecordMirrorSync(ctx context.Context, r proto.Repository, syncErr error) error {
	if syncErr == nil {
		if err := d.populateLastModified(ctx, r.Name()); err != nil {
			d.logger.Warn("failed to record the last change of a mirror", "repo", r.Name(), "err", err)
		}
	}
	msg := ""
	if syncErr != nil {
		// Valid UTF-8 first, so the cut below lands between characters;
		// the store makes it so too, and drops NUL bytes.
		msg = db.StorableText(syncErr.Error())
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
	return wrapRepoError(d.store.SetRepoMirrorSyncByID(ctx, d.db, r.ID(), msg))
}

// MirrorSyncStatus returns the outcome of the syncs of the mirror name.
func (d *Backend) MirrorSyncStatus(ctx context.Context, name string) (MirrorSync, error) {
	m, err := d.store.GetRepoByName(ctx, d.db, proto.SanitizeRepo(name))
	if err != nil {
		return MirrorSync{}, wrapRepoError(err)
	}
	return MirrorSync{SyncedAt: m.MirrorSyncedAt.Time, Error: m.MirrorError}, nil
}
