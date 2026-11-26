package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// The key the first migration seeds is the one most likely to be rotated:
// install with K1, later replace it with K2 in the config. K1 must then go,
// as a key the sync attached does, rather than stay on the admin account
// for good, with full admin rights and no log line.
func TestSeededAdminKeyRemovedWhenRotated(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	k1, k2 := newAuthorizedKey(t), newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{k1}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, k1); !ok || o.Username != "admin" {
		t.Fatalf("seeded key owner = %+v, %v; want admin", o, ok)
	}

	cfg.InitialAdminKeys = []string{k2}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, k1); ok {
		t.Errorf("rotated-out seeded key owner = %+v; want it removed", o)
	}
	if o, ok := ownerOf(t, dbx, k2); !ok || o.Username != "admin" {
		t.Errorf("new key owner = %+v, %v; want admin", o, ok)
	}
}

// A database whose first migration ran before seeded keys were recorded
// keeps such a key when it leaves the config: nothing tells it apart from a
// key added by hand. Each boot then warns about it, naming the account and
// the key, so the operator can remove it, or list it in initial_admin_keys
// to keep it and silence the warning.
func TestUnrecordedAdminKeyWarnsAtBoot(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	seeded, kept, other := newAuthorizedKey(t), newAuthorizedKey(t), newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{seeded, kept}
	boot(t, ctx, cfg, dbx)
	// As an older first migration left it: no record, and no marker that
	// seeded keys are recorded.
	mustExec(t, dbx, `UPDATE public_keys SET synced_user_id = NULL`)
	mustExec(t, dbx, `DELETE FROM settings WHERE "key" = ?`, adminKeysRecordedKey)
	// A key on another admin is that admin's business.
	mustExec(t, dbx, `INSERT INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, "erin", true)
	mustExec(t, dbx, `INSERT INTO public_keys (user_id, public_key, updated_at)
		VALUES ((SELECT id FROM users WHERE username = ?), ?, CURRENT_TIMESTAMP)`, "erin", other)

	cfg.InitialAdminKeys = []string{kept}
	logs := bootLogs(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, seeded); !ok || o.Username != "admin" {
		t.Errorf("unrecorded key owner = %+v, %v; want it kept on admin", o, ok)
	}
	fp := func(ak string) string {
		t.Helper()
		pk, _, err := proto.ParseAuthorizedKey(ak)
		if err != nil {
			t.Fatal(err)
		}
		return ssh.FingerprintSHA256(pk)
	}
	if !strings.Contains(logs, "WARN") || !strings.Contains(logs, fp(seeded)) {
		t.Errorf("logs = %q; want a warning naming %s", logs, fp(seeded))
	}
	for _, ak := range []string{kept, other} {
		if strings.Contains(logs, fp(ak)) {
			t.Errorf("logs = %q; want no warning about %s", logs, fp(ak))
		}
	}

	// Listed again, it is not warned about.
	cfg.InitialAdminKeys = []string{kept, seeded}
	if logs := bootLogs(t, ctx, cfg, dbx); strings.Contains(logs, fp(seeded)) {
		t.Errorf("logs with the key listed = %q; want no warning", logs)
	}
	cfg.InitialAdminKeys = []string{kept}

	// Removed by hand, it is not warned about again.
	mustExec(t, dbx, `DELETE FROM public_keys WHERE public_key = ?`, seeded)
	if logs := bootLogs(t, ctx, cfg, dbx); strings.Contains(logs, fp(seeded)) {
		t.Errorf("logs after removal = %q; want no warning", logs)
	}
}

// A database this version created records the keys it seeds, so a key on
// the admin user without a record was added by hand, and starting does not
// warn about it.
func TestHandAddedAdminKeyNoWarningOnNewInstall(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{newAuthorizedKey(t)}
	boot(t, ctx, cfg, dbx)
	byHand := newAuthorizedKey(t)
	mustExec(t, dbx, `INSERT INTO public_keys (user_id, public_key, updated_at)
		VALUES ((SELECT id FROM users WHERE username = ?), ?, CURRENT_TIMESTAMP)`, "admin", byHand)

	if logs := bootLogs(t, ctx, cfg, dbx); strings.Contains(logs, "WARN") {
		t.Errorf("logs = %q; want no warning", logs)
	}
	if o, ok := ownerOf(t, dbx, byHand); !ok || o.Username != "admin" {
		t.Errorf("hand-added key owner = %+v, %v; want admin", o, ok)
	}
}
