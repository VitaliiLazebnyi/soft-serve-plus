package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	_ "modernc.org/sqlite" // sqlite driver
)

// DB is the interface for a Soft Serve database.
type DB struct {
	*sqlx.DB
	logger *log.Logger // query trace, set only in verbose mode
	errLog *log.Logger // constraint violation detail, see mapError
}

// Open opens a database connection.
func Open(ctx context.Context, driverName string, dsn string) (*DB, error) {
	var db *sqlx.DB
	switch driverName {
	case "postgres":
		cfg, err := postgresUTCConfig(dsn)
		if err != nil {
			return nil, err
		}
		connector, err := pq.NewConnectorConfig(cfg)
		if err != nil {
			return nil, err
		}
		db = sqlx.NewDb(sql.OpenDB(connector), driverName)
		if err := db.PingContext(ctx); err != nil {
			db.Close() //nolint:errcheck
			return nil, err
		}
	default:
		if driverName == "sqlite" {
			dsn = sqliteImmediateTx(dsn)
		}
		var err error
		db, err = sqlx.ConnectContext(ctx, driverName, dsn)
		if err != nil {
			return nil, err
		}
	}

	d := &DB{
		DB:     db,
		errLog: log.FromContext(ctx).WithPrefix("db"),
	}

	if config.IsVerbose() {
		logger := log.FromContext(ctx).WithPrefix("db")
		d.logger = logger
	}

	return d, nil
}

// sqliteImmediateTx makes write transactions on SQLite start with BEGIN
// IMMEDIATE unless the DSN already sets _txlock. A deferred transaction that
// reads before it writes cannot upgrade to a write lock while another
// connection holds one: SQLite returns SQLITE_BUSY at once instead of waiting
// for busy_timeout. Taking the write lock at BEGIN makes it wait instead.
// Read-only transactions are still started as deferred by the driver.
func sqliteImmediateTx(dsn string) string {
	i := strings.IndexByte(dsn, '?')
	if i == 0 {
		// The driver treats the whole string as a file name.
		return dsn
	}
	if i > 0 {
		if q, err := url.ParseQuery(dsn[i+1:]); err != nil || q.Has("_txlock") {
			return dsn
		}
		return dsn + "&_txlock=immediate"
	}
	return dsn + "?_txlock=immediate"
}

// postgresUTCConfig parses dsn into a lib/pq configuration whose sessions
// use UTC as their TimeZone, whatever the DSN or the server's default
// says. The tables store times in TIMESTAMP (without time zone) columns,
// filled by CURRENT_TIMESTAMP or from UTC time.Time values, and
// lib/pq reads them back as UTC. In any other session time zone
// CURRENT_TIMESTAMP stores that zone's wall clock, which is then read back
// off by the zone's UTC offset.
func postgresUTCConfig(dsn string) (pq.Config, error) {
	cfg, err := pq.NewConfig(dsn)
	if err != nil {
		return cfg, err
	}
	rt := make(map[string]string, len(cfg.Runtime)+1)
	for k, v := range cfg.Runtime {
		// Run-time parameter names are case-insensitive.
		if !strings.EqualFold(k, "timezone") {
			rt[k] = v
		}
	}
	rt["timezone"] = "UTC"
	cfg.Runtime = rt
	return cfg, nil
}

// SetPoolLimits applies the connection pool settings in c. A zero value
// leaves the database/sql default for that setting: no limit on open
// connections, two idle connections, and no limit on a connection's
// lifetime or idle time.
func (d *DB) SetPoolLimits(c config.DBConfig) {
	if c.MaxOpenConns > 0 {
		d.SetMaxOpenConns(c.MaxOpenConns)
	}
	if c.MaxIdleConns > 0 {
		d.SetMaxIdleConns(c.MaxIdleConns)
	}
	if c.ConnMaxLifetime > 0 {
		d.SetConnMaxLifetime(time.Duration(c.ConnMaxLifetime) * time.Second)
	}
	if c.ConnMaxIdleTime > 0 {
		d.SetConnMaxIdleTime(time.Duration(c.ConnMaxIdleTime) * time.Second)
	}
}

// Close implements db.DB.
func (d *DB) Close() error {
	return d.DB.Close()
}

// Tx is a database transaction.
type Tx struct {
	*sqlx.Tx
	logger *log.Logger
	errLog *log.Logger
}

// Transaction implements db.DB.
func (d *DB) Transaction(fn func(tx *Tx) error) error {
	return d.TransactionContext(context.Background(), fn)
}

// TransactionContext implements db.DB.
func (d *DB) TransactionContext(ctx context.Context, fn func(tx *Tx) error) error {
	txx, err := d.DB.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	tx := &Tx{txx, d.logger, d.errLog}
	if err := fn(tx); err != nil {
		return rollback(tx, err)
	}

	if err := tx.Commit(); err != nil {
		// sql.ErrTxDone here means the transaction was already rolled back,
		// most often because ctx was cancelled after fn returned: nothing fn
		// wrote is kept, so it is a failure like any other. Report the
		// context's error along with it so callers can tell why.
		if errors.Is(err, sql.ErrTxDone) {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("failed to commit transaction: %w: %w", err, cerr)
			}
		}
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

func rollback(tx *Tx, err error) error {
	if rerr := tx.Rollback(); rerr != nil {
		if errors.Is(rerr, sql.ErrTxDone) {
			return err
		}
		return fmt.Errorf("failed to rollback: %s: %w", err.Error(), rerr)
	}

	return err
}
