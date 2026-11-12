package ssh

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/ssh/cmd"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spf13/cobra"
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

// The CLI command metric is labelled from the server's command tree, never
// with what the client typed, so clients cannot add series.
func TestCommandLabel(t *testing.T) {
	t.Parallel()
	newRoot := func() *cobra.Command {
		root := &cobra.Command{SilenceUsage: true, SilenceErrors: true}
		root.CompletionOptions.DisableDefaultCmd = true
		root.AddCommand(cmd.RepoCommand(), cmd.GitReceivePackCommand())
		return root
	}

	cases := []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"--help"}, ""},
		{[]string{"repo"}, "repo"},
		{[]string{"repo", "create", "x"}, "repo"},
		{[]string{"repo", "no-such-subcommand"}, "repo"},
		{[]string{"git-receive-pack", "any-name"}, "git-receive-pack"},
		{[]string{"help"}, "help"},
		{[]string{"no-such-command"}, unknownCommand},
		{[]string{strings.Repeat("x", 100<<10)}, unknownCommand},
	}
	for _, tc := range cases {
		if got := commandLabel(newRoot(), tc.args); got != tc.want {
			t.Errorf("commandLabel(%.40q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// Matching the help command early must not stop Execute from running it,
// nor add it twice.
func TestCommandLabelKeepsHelp(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(cmd.RepoCommand())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	args := []string{"help", "repo"}
	if got := commandLabel(root, args); got != "help" {
		t.Fatalf("commandLabel = %q, want help", got)
	}
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	n := 0
	for _, c := range root.Commands() {
		if c.Name() == "help" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("root has %d help commands, want 1", n)
	}
}

// mockSSHContext is a minimal ssh.Context for calling auth handlers
// directly.
type mockSSHContext struct {
	context.Context
	values      map[any]any
	permissions *ssh.Permissions
}

func (m *mockSSHContext) SetValue(key, value any) {
	m.values[key] = value
}

func (m *mockSSHContext) Value(key any) any {
	if v, ok := m.values[key]; ok {
		return v
	}
	return m.Context.Value(key)
}

func (m *mockSSHContext) Permissions() *ssh.Permissions {
	return m.permissions
}

func (m *mockSSHContext) User() string          { return "" }
func (m *mockSSHContext) RemoteAddr() net.Addr  { return &net.TCPAddr{} }
func (m *mockSSHContext) LocalAddr() net.Addr   { return &net.TCPAddr{} }
func (m *mockSSHContext) ServerVersion() string { return "" }
func (m *mockSSHContext) ClientVersion() string { return "" }
func (m *mockSSHContext) SessionID() string     { return "" }
func (m *mockSSHContext) Lock()                 {}
func (m *mockSSHContext) Unlock()               {}
