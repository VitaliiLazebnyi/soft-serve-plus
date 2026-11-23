package dbtest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/lib/pq"
)

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// TestDBNameUniqueInParallel checks that goroutines asking for names at the
// same moment, as parallel testscripts do, never get the same one.
func TestDBNameUniqueInParallel(t *testing.T) {
	const goroutines, each = 64, 100
	names := make(chan string, goroutines*each)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			<-start
			for range each {
				names <- DBName("softserve_migrate_test")
			}
		})
	}
	close(start)
	wg.Wait()
	close(names)

	seen := map[string]bool{}
	for name := range names {
		if seen[name] {
			t.Fatalf("name %q returned twice", name)
		}
		seen[name] = true
		if !identifier.MatchString(name) || len(name) > maxIdentifier {
			t.Fatalf("name %q is not an unquoted Postgres identifier of at most %d bytes", name, maxIdentifier)
		}
	}
}

// TestDBNameUniqueAcrossProcesses checks that two processes, such as two
// packages' test binaries sharing one Postgres server, do not get the same
// names even though each starts its counter at the same value.
func TestDBNameUniqueAcrossProcesses(t *testing.T) {
	const n = 1000
	if os.Getenv("SOFT_SERVE_TEST_DBNAME_CHILD") == "1" {
		for range n {
			os.Stdout.WriteString(DBName("softserve_test") + "\n") //nolint:errcheck
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDBNameUniqueAcrossProcesses$") //nolint:gosec,noctx
	cmd.Env = append(os.Environ(), "SOFT_SERVE_TEST_DBNAME_CHILD=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child process: %v\n%s%s", err, out, stderr.Bytes())
	}
	child := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "softserve_test_") {
			child[line] = true
		}
	}
	if len(child) != n {
		t.Fatalf("child process printed %d distinct names, want %d\nstdout:\n%s\nstderr:\n%s", len(child), n, out, stderr.Bytes())
	}
	for range n {
		if name := DBName("softserve_test"); child[name] {
			t.Fatalf("name %q returned in both processes", name)
		}
	}
}

// fakeAdmin fails CREATE DATABASE with the queued errors, then succeeds.
type fakeAdmin struct {
	errs    []error
	queries []string
}

func (f *fakeAdmin) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	f.queries = append(f.queries, query)
	if len(f.errs) == 0 {
		return nil, nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return nil, err
}

// TestCreateDBRetriesTakenName checks that CreateDB tries a fresh name when
// the name is taken, whether Postgres reports it as an existing database or,
// for two concurrent creates, as a duplicate key.
func TestCreateDBRetriesTakenName(t *testing.T) {
	f := &fakeAdmin{errs: []error{
		&pq.Error{Code: "23505"}, // another session creating the same name
		&pq.Error{Code: "42P04"}, // the name exists
	}}
	name, err := CreateDB(context.Background(), f, "softserve_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 3 {
		t.Fatalf("CreateDB ran %d statements, want 3: %q", len(f.queries), f.queries)
	}
	if f.queries[0] == f.queries[1] || f.queries[1] == f.queries[2] {
		t.Fatalf("CreateDB retried with the same name: %q", f.queries)
	}
	if f.queries[2] != "CREATE DATABASE "+name {
		t.Fatalf("CreateDB returned %q, but created with %q", name, f.queries[2])
	}
}

func TestCreateDBReturnsOtherErrors(t *testing.T) {
	other := &pq.Error{Code: "42501"} // insufficient_privilege
	f := &fakeAdmin{errs: []error{other}}
	if _, err := CreateDB(context.Background(), f, "softserve_test"); !errors.Is(err, other) {
		t.Fatalf("CreateDB = %v, want %v", err, other)
	}
	if len(f.queries) != 1 {
		t.Fatalf("CreateDB retried an error that is not about the name: %q", f.queries)
	}
}

func TestCreateDBGivesUp(t *testing.T) {
	f := &fakeAdmin{}
	for range createDBAttempts {
		f.errs = append(f.errs, &pq.Error{Code: "42P04"})
	}
	if _, err := CreateDB(context.Background(), f, "softserve_test"); err == nil {
		t.Fatal("CreateDB succeeded with every name taken")
	}
	if len(f.queries) != createDBAttempts {
		t.Fatalf("CreateDB ran %d statements, want %d", len(f.queries), createDBAttempts)
	}
}

// TestCreateDBPostgresParallel creates databases from many goroutines at once
// on a real server, as parallel testscripts do. It runs when
// SOFT_SERVE_DB_DRIVER is "postgres", as in the coverage job's Postgres step.
func TestCreateDBPostgresParallel(t *testing.T) {
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("set SOFT_SERVE_DB_DRIVER=postgres to run")
	}
	dsn := os.Getenv("SOFT_SERVE_DB_DATA_SOURCE")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close() //nolint:errcheck

	const n = 16
	ctx := context.Background()
	names := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			names[i], errs[i] = CreateDB(ctx, admin, "softserve_test")
		})
	}
	close(start)
	wg.Wait()
	for i, name := range names {
		if errs[i] != nil {
			t.Error(errs[i])
			continue
		}
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error("failed to drop database", name, err)
		}
	}
}
