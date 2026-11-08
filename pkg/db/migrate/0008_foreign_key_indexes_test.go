package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// Foreign-key columns that no unique constraint leads with get an index, and
// rolling the migration back drops it. The test runs on Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres".
func TestForeignKeyIndexes(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// Later migrations stay out of the test so that Rollback undoes this one.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:foreignKeyIndexesVersion]

	indexes := []string{
		"public_keys_user_id_idx",
		"repos_user_id_idx",
		"collabs_repo_id_idx",
		"lfs_objects_repo_id_idx",
		"lfs_locks_user_id_idx",
		"access_tokens_user_id_idx",
	}
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`
	if dbx.DriverName() == "postgres" {
		query = `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ?`
	}
	check := func(when string, want bool) {
		t.Helper()
		for _, name := range indexes {
			var n int
			if err := dbx.GetContext(ctx, &n, dbx.Rebind(query), name); err != nil {
				t.Fatalf("look up index %s: %v", name, err)
			}
			if got := n > 0; got != want {
				t.Errorf("%s: index %s exists = %v, want %v", when, name, got, want)
			}
		}
	}

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v", err)
	}
	check("after migrate", true)

	if err := Rollback(ctx, dbx); err != nil {
		t.Fatalf("Rollback() => %v", err)
	}
	check("after rollback", false)

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() again => %v", err)
	}
	check("after migrating again", true)

	// A user lookup reads the user's keys by user_id; it uses the index.
	if dbx.DriverName() != "postgres" {
		var plan []struct {
			ID      int    `db:"id"`
			Parent  int    `db:"parent"`
			NotUsed int    `db:"notused"`
			Detail  string `db:"detail"`
		}
		if err := dbx.SelectContext(ctx, &plan, `EXPLAIN QUERY PLAN SELECT * FROM public_keys WHERE user_id = 1`); err != nil {
			t.Fatal(err)
		}
		if len(plan) == 0 || !strings.Contains(plan[len(plan)-1].Detail, "public_keys_user_id_idx") {
			t.Errorf("query plan for public_keys by user_id = %+v, want it to use public_keys_user_id_idx", plan)
		}
	}
}
