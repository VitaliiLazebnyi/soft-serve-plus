package database

import (
	"context"
	"strconv"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// GetAllowKeylessAccess implements store.Store.
func (*Store) GetAllowKeylessAccess(ctx context.Context, tx db.Handler) (bool, error) {
	var allow bool
	query := tx.Rebind(`SELECT value FROM settings WHERE "key" = 'allow_keyless'`)
	if err := tx.GetContext(ctx, &allow, query); err != nil {
		return false, err
	}
	return allow, nil
}

// GetAnonAccess implements store.Store.
func (*Store) GetAnonAccess(ctx context.Context, tx db.Handler) (access.AccessLevel, error) {
	var level string
	query := tx.Rebind(`SELECT value FROM settings WHERE "key" = 'anon_access'`)
	if err := tx.GetContext(ctx, &level, query); err != nil {
		return access.NoAccess, err
	}
	return access.ParseAccessLevel(level), nil
}

// SetAllowKeylessAccess implements store.Store.
func (*Store) SetAllowKeylessAccess(ctx context.Context, tx db.Handler, allow bool) error {
	// Stored as text, the way migration 1 seeds it, so the value reads the
	// same on SQLite and Postgres.
	return setSetting(ctx, tx, "allow_keyless", strconv.FormatBool(allow))
}

// SetAnonAccess implements store.Store.
func (*Store) SetAnonAccess(ctx context.Context, tx db.Handler, level access.AccessLevel) error {
	return setSetting(ctx, tx, "anon_access", level.String())
}

// setSetting writes a setting, adding its row if it is missing: migration 1
// seeds every setting, so a missing row was deleted by hand.
func setSetting(ctx context.Context, h db.Handler, key, value string) error {
	query := h.Rebind(`INSERT INTO settings ("key", value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT ("key") DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`)
	_, err := h.ExecContext(ctx, query, key, value)
	return err
}
