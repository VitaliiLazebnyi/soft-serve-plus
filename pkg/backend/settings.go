package backend

import (
	"context"

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
func (d *Backend) AllowKeyless(ctx context.Context) bool {
	if d.cfg.AllowKeyless != nil {
		return *d.cfg.AllowKeyless
	}

	var allow bool
	if err := d.db.ReadTransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		allow, err = d.store.GetAllowKeylessAccess(ctx, tx)
		return err
	}); err != nil {
		d.logger.Error("error getting allow-keyless setting, denying keyless access", "err", err)
		return false
	}

	return allow
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
func (d *Backend) AnonAccess(ctx context.Context) access.AccessLevel {
	if d.cfg.AnonAccess != nil {
		return *d.cfg.AnonAccess
	}

	var level access.AccessLevel
	if err := d.db.ReadTransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		level, err = d.store.GetAnonAccess(ctx, tx)
		return err
	}); err != nil {
		d.logger.Error("error getting anon-access setting, denying anonymous access", "err", err)
		return access.NoAccess
	}

	return level
}

// SetAnonAccess sets the level of anonymous access.
func (d *Backend) SetAnonAccess(ctx context.Context, level access.AccessLevel) error {
	return d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return d.store.SetAnonAccess(ctx, tx, level)
	})
}
