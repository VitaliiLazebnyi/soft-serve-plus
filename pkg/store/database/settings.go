package database

import (
	"context"
	"fmt"
	"strconv"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// GetAllowKeylessAccess implements store.Store.
func (*Store) GetAllowKeylessAccess(ctx context.Context, h db.Handler) (bool, error) {
	var allow bool
	query := h.Rebind(`SELECT value FROM settings WHERE "key" = 'allow_keyless'`)
	if err := h.GetContext(ctx, &allow, query); err != nil {
		return false, err
	}
	return allow, nil
}

// GetAnonAccess implements store.Store.
func (*Store) GetAnonAccess(ctx context.Context, h db.Handler) (access.AccessLevel, error) {
	var level string
	query := h.Rebind(`SELECT value FROM settings WHERE "key" = 'anon_access'`)
	if err := h.GetContext(ctx, &level, query); err != nil {
		return access.NoAccess, err
	}
	l, err := access.ParseAccessLevel(level)
	if err != nil {
		return access.NoAccess, fmt.Errorf("anon_access setting %q: %w", level, err)
	}
	return l, nil
}

// SetAllowKeylessAccess implements store.Store.
func (*Store) SetAllowKeylessAccess(ctx context.Context, h db.Handler, allow bool) error {
	// Stored as text, the way migration 1 seeds it, so the value reads the
	// same on SQLite and Postgres.
	return setSetting(ctx, h, "allow_keyless", strconv.FormatBool(allow))
}

// SetAnonAccess implements store.Store.
func (*Store) SetAnonAccess(ctx context.Context, h db.Handler, level access.AccessLevel) error {
	return setSetting(ctx, h, "anon_access", level.String())
}

// setSetting writes a setting, adding its row if it is missing: migration 1
// seeds every setting, so a missing row was deleted by hand.
func setSetting(ctx context.Context, h db.Handler, key, value string) error {
	query := h.Rebind(`INSERT INTO settings ("key", value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT ("key") DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`)
	_, err := h.ExecContext(ctx, query, key, value)
	return err
}
