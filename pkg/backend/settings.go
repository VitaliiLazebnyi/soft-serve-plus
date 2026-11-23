package backend

import (
	"context"
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// AllowKeyless returns whether or not keyless access is allowed.
//
// If the server config sets AllowKeyless, that value always takes
// precedence over the database — it is a live override, not a one-time
// default, matching how InitialAdminKeys behaves. Do not cache this: a
// memoized/TTL'd copy would delay an admin's `settings allow-keyless false`
// from taking effect, which is a fail-open risk on an access-control check.
//
// A failed lookup denies keyless access. Callers that answer a client use
// AllowKeylessSetting instead, so an outage is not answered as a refusal.
func (d *Backend) AllowKeyless(ctx context.Context) bool {
	allow, err := d.AllowKeylessSetting(ctx)
	if err != nil {
		d.logLookupError(ctx, "error getting allow-keyless setting, denying keyless access", err)
		return false
	}
	return allow
}

// AllowKeylessSetting is AllowKeyless with the error of a failed lookup
// returned rather than read as "no". It does not log the error.
func (d *Backend) AllowKeylessSetting(ctx context.Context) (bool, error) {
	if d.cfg.AllowKeyless != nil {
		return *d.cfg.AllowKeyless, nil
	}

	allow, err := d.store.GetAllowKeylessAccess(ctx, d.db)
	if err != nil {
		return false, fmt.Errorf("read allow-keyless setting: %w", err)
	}
	return allow, nil
}

// logLookupError logs a failed lookup at Error, or at Debug when the
// client went away and canceled it.
func (d *Backend) logLookupError(ctx context.Context, msg string, err error) {
	if ctx.Err() != nil {
		d.logger.Debug(msg, "err", err)
		return
	}
	d.logger.Error(msg, "err", err)
}

// SetAllowKeyless sets whether or not keyless access is allowed.
func (d *Backend) SetAllowKeyless(ctx context.Context, allow bool) error {
	return d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return d.store.SetAllowKeylessAccess(ctx, tx, allow)
	})
}

// AnonAccess returns the level of anonymous access.
//
// If the server config sets AnonAccess, that value always takes precedence
// over the database — it is a live override, not a one-time default,
// matching how InitialAdminKeys behaves. Do not cache this: a memoized/TTL'd
// copy would delay an admin's `settings anon-access` change from taking
// effect, which is a fail-open risk on an access-control check.
//
// A failed lookup denies anonymous access. The access checks use anonAccess
// instead, so an outage is returned as an error, not answered as a refusal.
func (d *Backend) AnonAccess(ctx context.Context) access.Level {
	level, err := d.anonAccess(ctx)
	if err != nil {
		d.logLookupError(ctx, "error getting anon-access setting, denying anonymous access", err)
		return access.NoAccess
	}
	return level
}

// AnonAccessSetting is AnonAccess with the error of a failed lookup
// returned rather than read as "no access", for callers that answer a
// client. It does not log the error.
func (d *Backend) AnonAccessSetting(ctx context.Context) (access.Level, error) {
	return d.anonAccess(ctx)
}

// anonAccess is AnonAccessSetting.
func (d *Backend) anonAccess(ctx context.Context) (access.Level, error) {
	if d.cfg.AnonAccess != nil {
		return *d.cfg.AnonAccess, nil
	}

	level, err := d.store.GetAnonAccess(ctx, d.db)
	if err != nil {
		return access.NoAccess, fmt.Errorf("read anon-access setting: %w", err)
	}
	return level, nil
}

// SetAnonAccess sets the level of anonymous access.
func (d *Backend) SetAnonAccess(ctx context.Context, level access.Level) error {
	return d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return d.store.SetAnonAccess(ctx, tx, level)
	})
}
