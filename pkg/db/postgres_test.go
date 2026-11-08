package db

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresUTCConfigOverridesTimeZone(t *testing.T) {
	t.Parallel()
	for _, dsn := range []string{
		"postgres://u@localhost/db?sslmode=disable",
		"postgres://u@localhost/db?sslmode=disable&timezone=Asia/Tokyo",
		"postgres://u@localhost/db?sslmode=disable&TimeZone=Asia/Tokyo&application_name=x",
		"host=localhost user=u dbname=db TimeZone=Asia/Tokyo",
	} {
		cfg, err := postgresUTCConfig(dsn)
		if err != nil {
			t.Fatalf("postgresUTCConfig(%q): %v", dsn, err)
		}
		for k, v := range cfg.Runtime {
			if strings.EqualFold(k, "timezone") && (k != "timezone" || v != "UTC") {
				t.Errorf("postgresUTCConfig(%q): run-time parameter %s=%s, want only timezone=UTC", dsn, k, v)
			}
		}
		if got := cfg.Runtime["timezone"]; got != "UTC" {
			t.Errorf("postgresUTCConfig(%q): timezone = %q, want UTC", dsn, got)
		}
		if strings.Contains(dsn, "application_name") && cfg.ApplicationName != "x" {
			t.Errorf("postgresUTCConfig(%q): application_name = %q, want x", dsn, cfg.ApplicationName)
		}
	}
}

// TestPostgresTimestampsAreUTC checks that a CURRENT_TIMESTAMP default stored
// in a TIMESTAMP column reads back as the current UTC time, even when the DSN
// asks for a session time zone far from UTC. It needs a Postgres server and
// runs only when SOFT_SERVE_DB_DRIVER is "postgres".
func TestPostgresTimestampsAreUTC(t *testing.T) {
	t.Parallel()
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("SOFT_SERVE_DB_DRIVER is not postgres")
	}
	dsn := os.Getenv("SOFT_SERVE_DB_DATA_SOURCE")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	// Ask for a zone well away from UTC (and from the test machine's zone)
	// both in the DSN and through the options run-time parameter.
	const zone = "Pacific/Kiritimati" // UTC+14
	lower := strings.ToLower(dsn)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("TimeZone", zone)
		q.Set("options", "-c timezone="+zone)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn = strings.TrimSpace(dsn) + " TimeZone=" + zone + " options='-c timezone=" + zone + "'"
	}

	ctx := context.Background()
	d, err := Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	conn, err := d.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var tz string
	if err := conn.GetContext(ctx, &tz, "SHOW TimeZone"); err != nil {
		t.Fatal(err)
	}
	if tz != "UTC" {
		t.Errorf("session TimeZone = %q, want UTC", tz)
	}

	if _, err := conn.ExecContext(ctx, `CREATE TEMPORARY TABLE ts_test (
		id INTEGER,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if _, err := conn.ExecContext(ctx, "INSERT INTO ts_test (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	var created time.Time
	if err := conn.GetContext(ctx, &created, "SELECT created_at FROM ts_test WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	// Allow for clock skew between the test and a server on the same host
	// or a container; a wrong zone is off by hours.
	const slack = time.Minute
	if created.Before(before.Add(-slack)) || created.After(after.Add(slack)) {
		t.Errorf("created_at = %v, want about %v", created, before.UTC())
	}
}
