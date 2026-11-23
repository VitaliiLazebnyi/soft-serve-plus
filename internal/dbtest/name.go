package dbtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/lib/pq"
)

// dbCounter numbers the names DBName returns in this process.
var dbCounter atomic.Uint64

// maxIdentifier is the longest identifier Postgres keeps; it silently
// truncates longer ones, which could make two distinct names the same.
const maxIdentifier = 63

// DBName returns a name for a disposable test database: prefix, the process
// ID, a counter, and 50 random bits from crypto/rand.
//
// Tests run in parallel within a process (testscript runs every script in
// parallel) and go test runs packages as parallel processes against the same
// server, so a name must differ across both. The process ID and counter make
// it unique among the processes running at once; the random part keeps it
// apart from a database a crashed earlier run left behind under a reused
// process ID. A name seeded from the clock is not enough: goroutines starting
// together read the same clock value, and so derive the same name.
//
// prefix must be a lowercase SQL identifier. DBName panics if the result would
// be longer than Postgres keeps.
func DBName(prefix string) string {
	name := fmt.Sprintf("%s_%d_%d_%s", prefix, os.Getpid(), dbCounter.Add(1), strings.ToLower(rand.Text()[:10]))
	if len(name) > maxIdentifier {
		panic(fmt.Sprintf("dbtest.DBName: %q is longer than %d bytes", name, maxIdentifier))
	}
	return name
}

// Execer runs a statement; *sql.DB and *db.DB both are one.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// createDBAttempts bounds how many names CreateDB tries.
const createDBAttempts = 5

// CreateDB creates a database named by DBName(prefix) through admin and
// returns its name. Should the name be taken all the same, CreateDB tries
// again with a fresh one: Postgres reports duplicate_database when the name
// exists, and unique_violation ("duplicate key") when another session is
// creating it at the same moment. The caller drops the database.
func CreateDB(ctx context.Context, admin Execer, prefix string) (string, error) {
	var err error
	for range createDBAttempts {
		name := DBName(prefix)
		if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+name); err == nil {
			return name, nil
		}
		if !nameTaken(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("dbtest.CreateDB: no free name after %d attempts: %w", createDBAttempts, err)
}

// nameTaken reports whether err from CREATE DATABASE means the name is in use.
func nameTaken(err error) bool {
	var pgErr *pq.Error
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "42P04" || pgErr.Code == "23505" // duplicate_database, unique_violation
}
