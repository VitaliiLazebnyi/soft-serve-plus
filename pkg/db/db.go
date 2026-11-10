package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	_ "modernc.org/sqlite" // sqlite driver
)

// DB is a Soft Serve database connection pool.
type DB struct {
	*sqlx.DB
	logger *log.Logger
	// writeSlot, on SQLite, is held by the one write transaction of this
	// handle that may be open at a time. See transaction.
	writeSlot chan struct{}
}

// Open opens a database connection.
func Open(ctx context.Context, driverName string, dsn string) (*DB, error) {
	var (
		db  *sqlx.DB
		err error
	)
	switch driverName {
	case "sqlite":
		db, err = sqlx.ConnectContext(ctx, driverName, sqliteImmediateTx(dsn))
	case "postgres":
		db, err = connectPostgres(ctx, dsn)
	default:
		db, err = sqlx.ConnectContext(ctx, driverName, dsn)
	}
	if err != nil {
		return nil, err
	}

	d := &DB{
		DB: db,
	}
	if driverName == "sqlite" {
		d.writeSlot = make(chan struct{}, 1)
	}

	if config.IsVerbose() {
		logger := log.FromContext(ctx).WithPrefix("db")
		d.logger = logger
	}

	return d, nil
}

// defaultPostgresConnectTimeout bounds connecting to Postgres when the data
// source and PGCONNECT_TIMEOUT set no connect_timeout. Without it, lib/pq
// waits for as long as the operating system does, so an unreachable database
// can hang startup for minutes.
// It is a variable so tests can shorten it.
var defaultPostgresConnectTimeout = 10 * time.Second

// connectPostgres connects to Postgres and checks the connection, applying
// defaultPostgresConnectTimeout unless the data source or the environment
// already sets a connect_timeout (0 among them, which waits forever).
func connectPostgres(ctx context.Context, dsn string) (*sqlx.DB, error) {
	cfg, err := postgresConfig(dsn)
	if err != nil {
		return nil, err
	}
	connector, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		return nil, err
	}
	db := sqlx.NewDb(sql.OpenDB(connector), "postgres")
	if err := db.PingContext(ctx); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return db, nil
}

// postgresConfig parses dsn the way lib/pq does, environment included, with
// sessions in UTC (see postgresUTCConfig), and applies
// defaultPostgresConnectTimeout when no connect_timeout is set.
func postgresConfig(dsn string) (pq.Config, error) {
	cfg, err := postgresUTCConfig(dsn)
	if err != nil {
		return cfg, err
	}
	if !postgresConnectTimeoutSet(dsn) {
		cfg.ConnectTimeout = defaultPostgresConnectTimeout
	}
	return cfg, nil
}

// postgresConnectTimeoutSet reports whether the data source or the
// environment names a connect_timeout. A data source in URL form names it as
// a query parameter, a key/value one as a key; either way the word appears in
// it, and lib/pq rejects a parameter it does not know.
func postgresConnectTimeoutSet(dsn string) bool {
	if _, ok := os.LookupEnv("PGCONNECT_TIMEOUT"); ok {
		return true
	}
	return strings.Contains(dsn, "connect_timeout")
}

// sqliteImmediateTx makes write transactions on SQLite start with BEGIN
// IMMEDIATE unless the DSN already sets _txlock. A deferred transaction that
// reads before it writes cannot upgrade to a write lock while another
// connection holds one: SQLite returns SQLITE_BUSY at once instead of waiting
// for busy_timeout. Taking the write lock at BEGIN makes it wait instead.
// Transactions begun with TxOptions.ReadOnly, as ReadTransactionContext
// does, are still started as deferred by the driver, so reads do not queue
// behind writers.
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

// Close closes the database.
func (d *DB) Close() error {
	return d.DB.Close()
}

// Tx is a database transaction.
type Tx struct {
	*sqlx.Tx
	logger *log.Logger
}

// TransactionContext runs fn in a transaction. The transaction is committed
// if fn returns nil and rolled back otherwise. If fn does not return, because
// it panics or calls runtime.Goexit (such as t.FailNow in a test), the
// transaction is rolled back and the panic or exit continues.
func (d *DB) TransactionContext(ctx context.Context, fn func(tx *Tx) error) error {
	return d.transaction(ctx, nil, fn)
}

// ReadTransactionContext runs fn in a read-only transaction. On SQLite it
// starts with a deferred BEGIN, so it does not take the write lock and wait
// behind writers; on Postgres it is a READ ONLY transaction. fn must not
// write: Postgres rejects the write, and on SQLite a deferred transaction
// that reads and then writes fails at once with SQLITE_BUSY while another
// connection holds the write lock, instead of waiting for it. Use
// TransactionContext for anything that writes.
func (d *DB) ReadTransactionContext(ctx context.Context, fn func(tx *Tx) error) error {
	return d.transaction(ctx, &sql.TxOptions{ReadOnly: true}, fn)
}

func (d *DB) transaction(ctx context.Context, opts *sql.TxOptions, fn func(tx *Tx) error) error {
	// SQLite lets one transaction write at a time, and write transactions
	// begin IMMEDIATE (see sqliteImmediateTx), so each one waits at BEGIN
	// for the write lock. SQLite waits by polling with growing sleeps, for
	// busy_timeout in all, and then fails with "database is locked". The
	// lock goes to whichever waiter polls when it is free, not to the one
	// that has waited longest, so when a burst of writers between them
	// hold the lock longer than busy_timeout, the ones left behind fail,
	// however short each write is. On Windows, where file locking and
	// syncing are slow, a few dozen small writes are enough. The write
	// transactions of this process therefore take turns here first, in
	// the order they arrive and for as long as ctx allows, so BEGIN waits
	// only for other processes, such as git hooks, for which busy_timeout
	// still applies. Read-only transactions begin deferred and do not
	// take a turn. A write transaction must not start another one from fn:
	// it would wait for its own turn until ctx ends.
	if d.writeSlot != nil && (opts == nil || !opts.ReadOnly) {
		select {
		case d.writeSlot <- struct{}{}:
		case <-ctx.Done():
			return fmt.Errorf("failed to begin transaction: %w", ctx.Err())
		}
		defer func() { <-d.writeSlot }()
	}

	txx, err := d.DB.BeginTxx(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	tx := &Tx{txx, d.logger}
	returned := false
	defer func() {
		if !returned {
			// Release the connection, and SQLite's write lock, while the
			// goroutine unwinds.
			_ = tx.Rollback()
		}
	}()
	err = fn(tx)
	returned = true
	if err != nil {
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
		// Keep both errors reachable with errors.Is and errors.As.
		return fmt.Errorf("%w (failed to rollback: %w)", err, rerr)
	}

	return err
}
