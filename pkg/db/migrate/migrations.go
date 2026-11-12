package migrate

import (
	"context"
	"embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

//go:embed *.sql
var sqls embed.FS

// Known differences between the SQLite and Postgres schemas, left on purpose
// because closing them would rewrite whole tables on upgrade for no change in
// behaviour:
//
//   - webhook_deliveries.id is TEXT UNIQUE on SQLite (since migration 4) but
//     TEXT PRIMARY KEY on Postgres, so SQLite would accept a NULL id. The only
//     writer, CreateWebhookDelivery, always sets a UUID. Adding NOT NULL on
//     SQLite needs another rebuild of the deliveries table, bodies included,
//     like the one migration 4 does.
//   - Ids from migrations 1 and 2 are SERIAL (32-bit) on Postgres but 64-bit
//     on SQLite. They number users, repos, keys, collaborators, webhooks and
//     LFS objects and locks, which stay far below 2^31. The one high-churn
//     counter, webhook_deliveries.seq, is BIGINT on both. Widening the ids
//     means altering every id and foreign key column, which rewrites each
//     table under an exclusive lock.
//
// Keep this in order of execution, oldest to newest.
var migrations = []Migration{
	createTables,
	webhooks,
	migrateLfsObjects,
	webhookDeliverySeq,
	webhookDeliveryRetention,
	repoAnonymous,
	lfsObjectSizeBigint,
	foreignKeyIndexes,
}

func execMigration(ctx context.Context, tx *db.Tx, version int, name string, down bool) error {
	direction := "up"
	if down {
		direction = "down"
	}

	fn := fmt.Sprintf("%04d_%s_%s.%s.sql", version, toSnakeCase(name), tx.DriverName(), direction)
	sqlstr, err := sqls.ReadFile(fn)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, string(sqlstr)); err != nil {
		return err
	}

	return nil
}

func migrateUp(ctx context.Context, tx *db.Tx, version int, name string) error {
	return execMigration(ctx, tx, version, name, false)
}

func migrateDown(ctx context.Context, tx *db.Tx, version int, name string) error {
	return execMigration(ctx, tx, version, name, true)
}

var (
	matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
	matchAllCap   = regexp.MustCompile("([a-z0-9])([A-Z])")
)

func toSnakeCase(str string) string {
	str = strings.ReplaceAll(str, "-", "_")
	str = strings.ReplaceAll(str, " ", "_")
	snake := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")
	return strings.ToLower(snake)
}
