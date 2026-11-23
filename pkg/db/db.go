// Package db opens the server's SQLite or Postgres database and runs
// queries and transactions on it, with tracing and its own error values.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	_ "modernc.org/sqlite" // sqlite driver
)

// DB is a Soft Serve database connection pool. It wraps the sqlx handle
// rather than embedding it, so every query goes through the methods of DB,
// which trace it and map its error.
type DB struct {
	x      *sqlx.DB
	logger *log.Logger // query trace, set only with Options.Trace
	errLog *log.Logger // constraint violation detail, see mapError
	// writeSlot, on SQLite, is held by the one write transaction of this
	// handle that may be open at a time. See transaction.
	writeSlot chan struct{}
}

// Options are the settings of a database handle besides its driver and data
// source.
type Options struct {
	// Logger receives constraint violation detail at debug level (see
	// mapError) and, with Trace, every query. A nil Logger is the default
	// logger.
	Logger *log.Logger
	// Trace logs every query, and the number of its arguments, at debug
	// level. The server sets it in verbose mode.
	Trace bool
	// MaxOpenConns limits the open connections (db.max_open_conns). Zero
	// leaves database/sql's default of no limit. The pool keeps
	// database/sql's default of two idle connections, or fewer when the
	// limit is lower.
	MaxOpenConns int
}

// Open opens a database connection.
func Open(ctx context.Context, driverName string, dsn string, opts Options) (*DB, error) {
	var (
		db  *sqlx.DB
		err error
	)
	switch driverName {
	case "sqlite":
		if dsn, err = sqliteDSN(dsn); err != nil {
			return nil, err
		}
		db, err = sqlx.ConnectContext(ctx, driverName, dsn)
	case "postgres":
		db, err = connectPostgres(ctx, dsn, defaultPostgresConnectTimeout)
	default:
		return nil, fmt.Errorf("unknown driver %q: the database driver is sqlite or postgres", driverName)
	}
	if err != nil {
		return nil, err
	}

	return Wrap(db, opts), nil
}

// defaultPostgresConnectTimeout bounds connecting to Postgres when the data
// source and PGCONNECT_TIMEOUT set no connect_timeout. Without it, lib/pq
// waits for as long as the operating system does, so an unreachable database
// can hang startup for minutes.
const defaultPostgresConnectTimeout = 10 * time.Second

// connectPostgres connects to Postgres and checks the connection, applying
// timeout unless the data source or the environment already sets a
// connect_timeout (0 among them, which waits forever).
func connectPostgres(ctx context.Context, dsn string, timeout time.Duration) (*sqlx.DB, error) {
	cfg, err := postgresConfig(dsn, timeout)
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
// sessions in UTC (see postgresUTCConfig), and applies timeout when no
// connect_timeout is set.
func postgresConfig(dsn string, timeout time.Duration) (pq.Config, error) {
	cfg, err := postgresUTCConfig(dsn)
	if err != nil {
		return cfg, err
	}
	if !postgresConnectTimeoutSet(dsn) {
		cfg.ConnectTimeout = timeout
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

// defaultSqliteBusyTimeout is the busy timeout, in milliseconds, a SQLite
// data source gets when it sets none.
const defaultSqliteBusyTimeout = 5000

// errSqliteForeignKeysOff is returned by Open for a SQLite data source that
// turns foreign keys off. The schema relies on them: deleting a user or a
// repository removes their rows in other tables by ON DELETE CASCADE.
var errSqliteForeignKeysOff = errors.New("the SQLite data source (db.data_source, or SOFT_SERVE_DB_DATA_SOURCE) turns foreign keys off; " +
	"Soft Serve Plus needs them on: remove the foreign_keys pragma or the _fk or _foreign_keys setting from it")

// sqliteDSN returns dsn with what the server relies on, on every
// connection the pool opens:
//
//   - foreign keys enforced, which the schema's ON DELETE CASCADE needs. A
//     data source that turns them off is refused rather than overridden:
//     the driver applies its _fk shorthand after the _pragma list and takes
//     the first of repeated keys, so no appended key could win;
//   - a busy timeout of five seconds, unless the data source sets one, so a
//     statement that meets another process's write, such as a git hook's,
//     waits for it instead of failing with "database is locked";
//   - write-ahead logging, unless it sets a journal mode or is a "file:"
//     URI that opens the database read-only (mode=ro or immutable), which
//     cannot switch it. The driver drops mode and immutable from a plain
//     path, which opens read-write, so there they change nothing.
//     In SQLite's default rollback journal a commit locks the database
//     file exclusively while it writes and syncs, and every read waits for it,
//     for busy_timeout at most and then with "database is locked"; on a
//     slow disk a few commits in a row are enough. With WAL, reads go on
//     from the last commit while a write commits. The mode is stored in
//     the database file, so a database that has been opened in WAL stays
//     in it until a data source names another mode;
//   - write transactions that begin IMMEDIATE, unless it sets _txlock. A
//     deferred transaction that reads before it writes cannot upgrade to a
//     write lock while another connection holds one: SQLite returns
//     SQLITE_BUSY at once instead of waiting for busy_timeout. Taking the
//     write lock at BEGIN makes it wait instead. Transactions begun with
//     TxOptions.ReadOnly, as ReadTransactionContext does, are still started
//     as deferred by the driver, so reads do not queue behind writers.
//
// A data source that starts with "?" is a file name to the driver and is
// left as it is.
func sqliteDSN(dsn string) (string, error) {
	i := strings.IndexByte(dsn, '?')
	if i == 0 {
		return dsn, nil
	}
	var q url.Values
	if i > 0 {
		var err error
		if q, err = url.ParseQuery(dsn[i+1:]); err != nil {
			// The driver rejects it too, with its own error.
			return dsn, nil //nolint:nilerr
		}
	}
	var add []string
	fk, busy, journal := sqlitePragmas(q)
	switch fk {
	case pragmaOff:
		return "", errSqliteForeignKeysOff
	case pragmaUnset:
		add = append(add, "_pragma=foreign_keys(1)")
	}
	if !busy {
		add = append(add, "_pragma=busy_timeout("+strconv.Itoa(defaultSqliteBusyTimeout)+")")
	}
	if !journal && !sqliteReadOnly(dsn, q) {
		add = append(add, "_pragma=journal_mode(WAL)")
	}
	if !q.Has("_txlock") {
		add = append(add, "_txlock=immediate")
	}
	if len(add) == 0 {
		return dsn, nil
	}
	sep := "?"
	if i > 0 {
		sep = "&"
	}
	return dsn + sep + strings.Join(add, "&"), nil
}

// pragmaState is what a data source sets a boolean pragma to.
type pragmaState int

const (
	pragmaUnset pragmaState = iota
	pragmaOn
	pragmaOff
)

// sqlitePragmas returns what the query of a SQLite data source sets foreign
// keys to, through the _fk and _foreign_keys keys or a _pragma, and whether
// it sets a busy timeout and a journal mode, each through a _pragma or the
// driver's own keys (_busy_timeout or _timeout, _journal_mode or _journal).
// The driver applies its own keys after the _pragma list, so a pragma added
// next to one of them would run first and then be undone on every new
// connection. A foreign_keys setting that is not one of the driver's true
// values counts as off.
func sqlitePragmas(q url.Values) (fk pragmaState, busy, journal bool) {
	set := func(v string) {
		switch strings.ToLower(strings.Trim(strings.TrimSpace(v), `'"`)) {
		case "1", "true", "on", "yes":
			if fk == pragmaUnset {
				fk = pragmaOn
			}
		default:
			fk = pragmaOff
		}
	}
	for _, k := range []string{"_fk", "_foreign_keys"} {
		for _, v := range q[k] {
			set(v)
		}
	}
	for _, p := range q["_pragma"] {
		name, value, hasValue := splitPragma(p)
		switch name {
		case "foreign_keys":
			if hasValue {
				set(value)
			}
		case "busy_timeout":
			busy = true
		case "journal_mode":
			journal = true
		}
	}
	busy = busy || q.Has("_busy_timeout") || q.Has("_timeout")
	journal = journal || q.Has("_journal_mode") || q.Has("_journal")
	return fk, busy, journal
}

// sqliteReadOnly reports whether a SQLite data source, dsn with its query q,
// opens the database read-only: a "file:" URI with the parameter mode=ro or
// immutable set to a true value. Such a connection cannot change the journal
// mode: SQLite refuses with "attempt to write a readonly database". The
// driver passes the parameters to SQLite only in a "file:" URI; it drops
// them from a plain path.
func sqliteReadOnly(dsn string, q url.Values) bool {
	if !strings.HasPrefix(dsn, "file:") {
		return false
	}
	if q.Get("mode") == "ro" {
		return true
	}
	switch strings.ToLower(q.Get("immutable")) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// splitPragma splits a _pragma value, such as "busy_timeout(5000)",
// "foreign_keys = on" or "main.journal_mode(WAL)", into its name, in lower
// case and without a schema, and its value. The schema is dropped so that a
// pragma naming one counts as the plain pragma: the driver runs the _pragma
// list sorted, so an added plain pragma would run before one that names
// main, and lose to it.
func splitPragma(p string) (name, value string, hasValue bool) {
	i := strings.IndexAny(p, "(=")
	if i < 0 {
		return pragmaName(p), "", false
	}
	value = strings.TrimSpace(p[i+1:])
	if p[i] == '(' {
		value = strings.TrimSpace(strings.TrimSuffix(value, ")"))
	}
	return pragmaName(p[:i]), value, true
}

// pragmaName is the name of a pragma written as name or schema.name, in
// lower case and without the schema.
func pragmaName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if j := strings.LastIndexByte(s, '.'); j >= 0 {
		s = strings.TrimSpace(s[j+1:])
	}
	return s
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

// Close closes the database.
func (d *DB) Close() error {
	return d.x.Close()
}

// DriverName returns the name of the database driver, "sqlite" or
// "postgres".
func (d *DB) DriverName() string {
	return d.x.DriverName()
}

// Rebind rewrites the ? placeholders of query for the driver.
func (d *DB) Rebind(query string) string {
	return d.x.Rebind(query)
}

// Wrap returns a DB over an open sqlx handle, for a driver or connector
// Open does not build, such as a test's. The handle must be a SQLite
// ("sqlite") or Postgres ("postgres") one.
func Wrap(x *sqlx.DB, opts Options) *DB {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	d := &DB{x: x, errLog: logger.WithPrefix("db")}
	if opts.Trace {
		d.logger = d.errLog
	}
	if x.DriverName() == "sqlite" {
		d.writeSlot = make(chan struct{}, 1)
	}
	if opts.MaxOpenConns > 0 {
		x.SetMaxOpenConns(opts.MaxOpenConns)
	}
	return d
}

// Conn returns one connection of the pool, for statements that must share a
// connection, such as a SQLite pragma, or run outside the transaction
// helpers, such as a test that holds a lock. Statements on it are neither
// traced nor mapped with WrapError. The caller closes it.
func (d *DB) Conn(ctx context.Context) (*sql.Conn, error) {
	return d.x.Conn(ctx)
}

// Stats returns the statistics of the connection pool.
func (d *DB) Stats() sql.DBStats {
	return d.x.Stats()
}

// SetMaxOpenConns limits the open connections; see Options.MaxOpenConns.
func (d *DB) SetMaxOpenConns(n int) {
	d.x.SetMaxOpenConns(n)
}

// SetMaxIdleConns sets the most idle connections the pool keeps. Zero
// closes the idle ones.
func (d *DB) SetMaxIdleConns(n int) {
	d.x.SetMaxIdleConns(n)
}

// PingContext checks that the database can be reached.
func (d *DB) PingContext(ctx context.Context) error {
	return d.x.PingContext(ctx)
}

// Tx is a database transaction. Like DB, it wraps the sqlx transaction
// rather than embedding it.
type Tx struct {
	x      *sqlx.Tx
	logger *log.Logger
	errLog *log.Logger
}

// DriverName returns the name of the database driver, "sqlite" or
// "postgres".
func (t *Tx) DriverName() string {
	return t.x.DriverName()
}

// Rebind rewrites the ? placeholders of query for the driver.
func (t *Tx) Rebind(query string) string {
	return t.x.Rebind(query)
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
	// begin IMMEDIATE (see sqliteDSN), so each one waits at BEGIN
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
			return fmt.Errorf("begin transaction: %w", ctx.Err())
		}
		defer func() { <-d.writeSlot }()
	}

	// On Postgres every transaction runs in READ COMMITTED, whatever the
	// server, database, role or data source make the default. The store's
	// row locks (LockAndCountWebhooks, LockUserRepos, the ForUpdate reads)
	// are followed by reads that must see what the lock's previous holder
	// committed. Only READ COMMITTED takes a snapshot per statement; under
	// REPEATABLE READ or SERIALIZABLE the snapshot predates the wait for the
	// lock, and two creates that took turns would both count the same
	// number of webhooks and both pass the cap.
	if d.DriverName() == "postgres" {
		o := sql.TxOptions{Isolation: sql.LevelReadCommitted}
		if opts != nil {
			o.ReadOnly = opts.ReadOnly
		}
		opts = &o
	}

	txx, err := d.x.BeginTxx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	tx := &Tx{txx, d.logger, d.errLog}
	returned := false
	defer func() {
		if !returned {
			// Release the connection, and SQLite's write lock, while the
			// goroutine unwinds.
			_ = tx.x.Rollback()
		}
	}()
	err = fn(tx)
	returned = true
	if err != nil {
		return rollback(tx, err)
	}

	if err := tx.x.Commit(); err != nil {
		// sql.ErrTxDone here means the transaction was already rolled back,
		// most often because ctx was cancelled after fn returned: nothing fn
		// wrote is kept, so it is a failure like any other. Report the
		// context's error along with it so callers can tell why.
		if errors.Is(err, sql.ErrTxDone) {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("commit transaction: %w: %w", err, cerr)
			}
		}
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func rollback(tx *Tx, err error) error {
	if rerr := tx.x.Rollback(); rerr != nil {
		if errors.Is(rerr, sql.ErrTxDone) {
			return err
		}
		// Keep both errors reachable with errors.Is and errors.As.
		return fmt.Errorf("%w (rollback: %w)", err, rerr)
	}

	return err
}
