package ssh

import (
	"context"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
	"github.com/prometheus/client_golang/prometheus/testutil"
	gossh "golang.org/x/crypto/ssh"
)

// publicKeyCounts returns the public key auth counter for allowed and
// refused attempts.
func publicKeyCounts() (allowed, refused float64) {
	return testutil.ToFloat64(publicKeyCounter.WithLabelValues("true")),
		testutil.ToFloat64(publicKeyCounter.WithLabelValues("false"))
}

// The public key auth counter counts public key auth attempts, as they are
// made, and nothing else.
func TestPublicKeyCounterCountsKeyAuth(t *testing.T) {
	is := is.New(t)
	pair, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	is.NoErr(err)
	pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(pair.AuthorizedKey()))
	is.NoErr(err)

	ctx := &mockSSHContext{
		Context:     context.Background(),
		values:      make(map[any]any),
		permissions: &ssh.Permissions{Permissions: &gossh.Permissions{Extensions: make(map[string]string)}},
	}
	s := &SSHServer{}

	allowed, refused := publicKeyCounts()
	is.True(s.PublicKeyHandler(ctx, pk))
	is.True(!s.PublicKeyHandler(ctx, nil))
	gotAllowed, gotRefused := publicKeyCounts()
	is.Equal(gotAllowed-allowed, 1.0)
	is.Equal(gotRefused-refused, 1.0)
}

// A session that signed in without a key is not a public key auth: it must
// not be counted as one, allowed or not.
func TestPublicKeyCounterSkipsKeylessSession(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	is.NoErr(migrate.Migrate(ctx, dbx))
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	is.True(be.AllowKeyless(ctx))

	s := &SSHServer{be: be}
	srv := &ssh.Server{
		Handler: ContextMiddleware(cfg, dbx, dbstore, be, log.Default())(AuthenticationMiddleware(func(s ssh.Session) {
			s.Exit(0) //nolint:errcheck
		})),
	}
	is.NoErr(srv.SetOption(ssh.KeyboardInteractiveAuth(s.KeyboardInteractiveHandler)))

	allowed, refused := publicKeyCounts()
	sess := testsession.New(t, srv, &gossh.ClientConfig{
		User: "anon",
		Auth: []gossh.AuthMethod{gossh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) {
			return nil, nil
		})},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec
	})
	is.NoErr(sess.Run(""))
	gotAllowed, gotRefused := publicKeyCounts()
	is.Equal(gotAllowed, allowed)
	is.Equal(gotRefused, refused)
}
