package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssh/cmd"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"
)

// The TestAuthentication tests run AuthenticationMiddleware behind a real
// SSH handshake on a server built by NewServer. They read the user a session
// runs as from the access tokens `token create` makes, as that command acts
// as the user AuthenticationMiddleware puts in the session context.

func newSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// offeredKey offers pub, a key the client does not hold: the server
// approves it when asked about it, but the client cannot sign with it. The
// signature it sends instead names an algorithm the server does not accept,
// so the server refuses the key and lets the client try another way. (A
// signature that fails to verify would end the connection.)
type offeredKey struct {
	pub gossh.PublicKey
}

func (k offeredKey) PublicKey() gossh.PublicKey { return k.pub }

func (k offeredKey) Sign(io.Reader, []byte) (*gossh.Signature, error) {
	return &gossh.Signature{Format: "unsigned", Blob: []byte{0}}, nil
}

// noAnswers answers keyboard-interactive authentication, which asks no
// questions here.
func noAnswers(string, string, []string, []bool) ([]string, error) { return nil, nil }

// runCommand connects to addr with the given auth methods and runs command.
// It returns the handshake error, or the command's stderr and exit error.
func runCommand(t *testing.T, addr string, command string, auth ...gossh.AuthMethod) (stderr string, runErr, dialErr error) {
	t.Helper()
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		Auth:            auth,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         sessionLoopsWait,
	})
	if err != nil {
		return "", nil, err
	}
	defer c.Close() //nolint: errcheck
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint: errcheck
	var errBuf bytes.Buffer
	s.Stderr = &errBuf
	runErr = s.Run(command)
	return errBuf.String(), runErr, nil
}

// tokenCount returns how many access tokens user has.
func tokenCount(ctx context.Context, t *testing.T, user proto.User) int {
	t.Helper()
	tokens, err := depsOf(ctx).be.ListAccessTokens(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	return len(tokens)
}

func newAuthServer(t *testing.T) (string, context.Context, *config.Config) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, ctx := startTestServer(t, cfg)
	return addr, ctx, cfg
}

// A client that offers the admin's public key without holding its private
// key, then signs with its own key, runs as its own user. The server
// approves the offered key before the signature check fails, so the
// session must take its user from the key that was signed with.
func TestAuthenticationUsesSignedKey(t *testing.T) {
	t.Parallel()
	addr, ctx, _ := newAuthServer(t)
	be := depsOf(ctx).be

	adminKey := newSigner(t)
	attackerKey := newSigner(t)
	admin, err := be.CreateUser(ctx, "testadmin", proto.UserOptions{
		Admin:      true,
		PublicKeys: []gossh.PublicKey{adminKey.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := be.CreateUser(ctx, "testattacker", proto.UserOptions{
		PublicKeys: []gossh.PublicKey{attackerKey.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}

	stderr, runErr, err := runCommand(t, addr, "token create probe",
		gossh.PublicKeys(offeredKey{pub: adminKey.PublicKey()}, attackerKey))
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if runErr != nil {
		t.Fatalf("token create: %v\n%s", runErr, stderr)
	}
	if n := tokenCount(ctx, t, attacker); n != 1 {
		t.Errorf("testattacker has %d tokens, want 1", n)
	}
	if n := tokenCount(ctx, t, admin); n != 0 {
		t.Errorf("testadmin has %d tokens, want 0: the session ran as the offered key's user", n)
	}
}

// Keyless access is refused when it is turned off, and when it is on, a
// keyless session runs as nobody, even after the client offered a user's
// key it could not sign with.
func TestAuthenticationKeyless(t *testing.T) {
	addr, ctx, _ := newAuthServer(t)
	be := depsOf(ctx).be

	adminKey := newSigner(t)
	admin, err := be.CreateUser(ctx, "testadmin", proto.UserOptions{
		Admin:      true,
		PublicKeys: []gossh.PublicKey{adminKey.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}
	offered := gossh.PublicKeys(offeredKey{pub: adminKey.PublicKey()})
	keyless := gossh.KeyboardInteractive(noAnswers)

	t.Run("allowed", func(t *testing.T) {
		if !be.AllowKeyless(ctx) {
			t.Fatal("keyless access is off by default")
		}
		stderr, runErr, err := runCommand(t, addr, "token create probe", offered, keyless)
		if err != nil {
			t.Fatalf("keyless handshake: %v", err)
		}
		if runErr == nil {
			t.Fatal("token create succeeded in a keyless session")
		}
		t.Logf("keyless token create: %v: %s", runErr, stderr)
		if n := tokenCount(ctx, t, admin); n != 0 {
			t.Errorf("testadmin has %d tokens, want 0: the keyless session ran as the offered key's user", n)
		}
	})

	t.Run("refused", func(t *testing.T) {
		if err := be.SetAllowKeyless(ctx, false); err != nil {
			t.Fatal(err)
		}
		for name, auth := range map[string][]gossh.AuthMethod{
			"keyboard-interactive":              {keyless},
			"offered key, keyboard-interactive": {offered, keyless},
		} {
			if _, _, err := runCommand(t, addr, "info", auth...); err == nil {
				t.Errorf("%s: client connected with keyless access off", name)
			}
		}
		// A key that authenticates still gets in.
		if _, runErr, err := runCommand(t, addr, "info", gossh.PublicKeys(adminKey)); err != nil || runErr != nil {
			t.Errorf("admin key with keyless access off: handshake %v, run %v", err, runErr)
		}
	})
}

// A key listed in initial_admin_keys that a non-admin account holds is
// refused a session, with an error that says how to fix it.
func TestAuthenticationRefusesClaimedAdminKey(t *testing.T) {
	t.Parallel()
	adminKey := newSigner(t)
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.InitialAdminKeys = []string{proto.MarshalAuthorizedKey(adminKey.PublicKey())}
	addr, ctx := startTestServer(t, cfg)
	be := depsOf(ctx).be

	// The listed key went to the admin account; hand it to a non-admin
	// one, as `pubkey add` before the key was listed would have.
	owner, err := be.UserByPublicKey(ctx, adminKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RemovePublicKey(ctx, owner.Username(), adminKey.PublicKey()); err != nil {
		t.Fatal(err)
	}
	mallory, err := be.CreateUser(ctx, "mallory", proto.UserOptions{
		PublicKeys: []gossh.PublicKey{adminKey.PublicKey()},
	})
	if err != nil {
		t.Fatal(err)
	}

	stderr, runErr, err := runCommand(t, addr, "token create probe", gossh.PublicKeys(adminKey))
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if runErr == nil {
		t.Fatal("session with a claimed admin key succeeded")
	}
	if !strings.Contains(stderr, "initial_admin_keys") || !strings.Contains(stderr, "user set-admin mallory true") {
		t.Errorf("stderr = %q, want the claimed admin key error", stderr)
	}
	if n := tokenCount(ctx, t, mallory); n != 0 {
		t.Errorf("mallory has %d tokens, want 0", n)
	}
}

// TestAuthenticationAfterOfferedKey checks that a session runs as the owner
// of the key that signed, not of a key the client offered before it. The
// client asks about each key before it signs, so PublicKeyHandler approves
// the offered key first. The server has no check of its own that the two
// agree: it relies on charm.land/ssh giving the session the key that signed,
// and this test guards that.
func TestAuthenticationAfterOfferedKey(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := serverContext(t, cfg, log.Default())
	be := depsOf(ctx).be

	alice, bob, stranger := newSigner(t), newSigner(t), newSigner(t)
	for name, key := range map[string]gossh.Signer{"alice": alice, "bob": bob} {
		if _, err := be.CreateUser(ctx, name, proto.UserOptions{
			PublicKeys: []gossh.PublicKey{key.PublicKey()},
		}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var approved []string
	sshSrv := &Server{be: be}
	hs := handlerServer(t, cfg, depsOf(ctx).dbx, depsOf(ctx).st, be, log.Default())
	srv := &ssh.Server{
		PublicKeyHandler: func(ctx ssh.Context, pk ssh.PublicKey) bool {
			ok := sshSrv.PublicKeyHandler(ctx, pk)
			if ok {
				mu.Lock()
				approved = append(approved, gossh.FingerprintSHA256(pk))
				mu.Unlock()
			}
			return ok
		},
		// The handler prints the session's user, or "anon" without one,
		// and the fingerprint of the session's key.
		Handler: hs.authenticationMiddleware(func(s ssh.Session) {
			name := "anon"
			if u := proto.UserFromContext(s.Context()); u != nil {
				name = u.Username()
			}
			_, _ = fmt.Fprintf(s, "%s %s", name, gossh.FingerprintSHA256(s.PublicKey()))
			_ = s.Exit(0)
		}),
	}
	addr := testsession.Listen(t, srv)

	for _, tc := range []struct {
		name     string
		offered  gossh.Signer
		signer   gossh.Signer
		wantUser string
	}{
		{"offer alice, sign as bob", alice, bob, "bob"},
		{"offer bob, sign as alice", bob, alice, "alice"},
		{"offer alice, sign with an unregistered key", alice, stranger, "anon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			approved = nil
			mu.Unlock()
			sess, err := testsession.NewClientSession(t, addr, &gossh.ClientConfig{
				User: "test",
				Auth: []gossh.AuthMethod{gossh.PublicKeys(offeredKey{pub: tc.offered.PublicKey()}, tc.signer)},
			})
			if err != nil {
				t.Fatalf("handshake: %v", err)
			}
			out, err := sess.Output("whoami")
			if err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			wasApproved := slices.Contains(approved, gossh.FingerprintSHA256(tc.offered.PublicKey()))
			mu.Unlock()
			if !wasApproved {
				t.Fatal("the server never approved the offered key, so the test checks nothing")
			}
			want := tc.wantUser + " " + gossh.FingerprintSHA256(tc.signer.PublicKey())
			if string(out) != want {
				t.Fatalf("session ran as %q, want %q", out, want)
			}
		})
	}
}

// While the settings cannot be read, a session whose access depends on them
// is told its access could not be checked, not that it is refused: a keyless
// session (the keyless setting) and a TUI session with an unregistered key
// (the anonymous level). The cause stays in the server log.
func TestSessionAccessCheckFailure(t *testing.T) {
	e := backendtest.New(t, backendtest.WithLogger(log.Default()))
	ctx, cfg, dbx, st, be := e.Ctx, e.Cfg, e.DB, e.Store, e.Backend
	logs := &syncBuffer{}

	// Authentication lets every client in, so the session handlers decide.
	hs := handlerServer(t, cfg, dbx, st, be, log.New(logs))
	srv := &ssh.Server{
		PublicKeyHandler: func(ssh.Context, ssh.PublicKey) bool { return true },
		KeyboardInteractiveHandler: func(ssh.Context, gossh.KeyboardInteractiveChallenge) bool {
			return true
		},
		Handler: hs.authenticationMiddleware(hs.bubbleTeaMiddleware(func(s ssh.Session) {
			_ = s.Exit(0)
		})),
	}
	addr := testsession.Listen(t, srv)

	kp, err := keygen.New(cfg.DataPath+"/stranger", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(kp.PrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	keyless := gossh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) { return nil, nil })

	if _, err := dbx.ExecContext(ctx, "ALTER TABLE settings RENAME TO settings_gone"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		auth gossh.AuthMethod
		pty  bool
	}{
		{"keyless", keyless, false},
		{"unregistered key, TUI", gossh.PublicKeys(signer), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := testsession.NewClientSession(t, addr, &gossh.ClientConfig{
				User: "test",
				Auth: []gossh.AuthMethod{tc.auth},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.pty {
				if err := sess.RequestPty("xterm", 40, 80, nil); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			sess.Stderr = &stderr
			sess.Stdout = &bytes.Buffer{}
			if err := sess.Run(""); err == nil {
				t.Fatal("session ran; want it ended")
			}
			if !strings.Contains(stderr.String(), ErrAccessCheckFailed.Error()) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), ErrAccessCheckFailed)
			}
			if strings.Contains(stderr.String(), "settings") {
				t.Fatalf("stderr = %q names the cause", stderr.String())
			}
		})
	}
	if !strings.Contains(logs.String(), "failed to check the session's access") {
		t.Errorf("logs = %q, want the failed check", logs.String())
	}
}

// An access check canceled because the client went away is logged at Debug.
func TestAccessCheckFailureCanceledLoggedAtDebug(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logAccessCheckFailure(ctx, logger, context.Canceled)
	if out := buf.String(); !strings.Contains(out, "access check canceled") || strings.Contains(out, "ERRO") {
		t.Errorf("logs = %q, want the canceled check at Debug only", out)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type logTestUser struct{ proto.User }

func (logTestUser) Username() string { return "alice" }

// TestLoggingMiddlewarePtyKeepsFields checks that a PTY session logs the
// terminal details in addition to the address, username and command, rather
// than in place of them.
func TestLoggingMiddlewarePtyKeepsFields(t *testing.T) {
	t.Parallel()
	var buf syncBuffer
	logger := log.NewWithOptions(&buf, log.Options{Level: log.DebugLevel})

	sess := testsession.New(t, &ssh.Server{
		Handler: func(s ssh.Session) {
			s.Context().SetValue(proto.ContextKeyUser, proto.User(logTestUser{}))
			(&Server{logger: logger}).loggingMiddleware(func(s ssh.Session) {
				s.Exit(0) //nolint: errcheck
			})(s)
		},
	}, nil)

	if err := sess.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Run("repo ls"); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := buf.String()
	var connected string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "connected") && !strings.Contains(line, "disconnected") {
			connected = line
		}
	}
	if connected == "" {
		t.Fatalf("no connected log line in output:\n%s", out)
	}
	for _, want := range []string{"username=alice", "addr=", `cmd="[repo ls]"`, "term=xterm", "width=80", "height=40"} {
		if !strings.Contains(connected, want) {
			t.Errorf("connected log line %q is missing %q", connected, want)
		}
	}
}

// A user lookup canceled because the client went away is logged at Debug,
// not as a server error.
func TestUserLookupFailureCanceledLoggedAtDebug(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logUserLookupFailure(ctx, logger, context.Canceled)
	if out := buf.String(); !strings.Contains(out, "user lookup canceled") || strings.Contains(out, "ERRO") {
		t.Errorf("logs = %q, want the canceled lookup at Debug only", out)
	}

	buf.Reset()
	logUserLookupFailure(context.Background(), logger, errors.New("db down"))
	if out := buf.String(); !strings.Contains(out, "ERRO") {
		t.Errorf("logs = %q, want the failed lookup at Error", out)
	}
}

// publicKeyCounts returns the public key auth counter for allowed and
// refused attempts.
func publicKeyCounts() (allowed, refused float64) {
	return testutil.ToFloat64(publicKeyCounter.WithLabelValues("true")),
		testutil.ToFloat64(publicKeyCounter.WithLabelValues("false"))
}

// The public key auth counter counts public key auth attempts, as they are
// made, and nothing else.
func TestPublicKeyCounterCountsKeyAuth(t *testing.T) {
	pair, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(pair.AuthorizedKey()))
	if err != nil {
		t.Fatal(err)
	}

	ctx := &mockSSHContext{
		Context:     context.Background(),
		values:      make(map[any]any),
		permissions: &ssh.Permissions{Permissions: &gossh.Permissions{Extensions: make(map[string]string)}},
	}
	s := &Server{}

	allowed, refused := publicKeyCounts()
	if !s.PublicKeyHandler(ctx, pk) {
		t.Fatal("not true: s.PublicKeyHandler(ctx, pk)")
	}
	if s.PublicKeyHandler(ctx, nil) {
		t.Fatal("not true: !s.PublicKeyHandler(ctx, nil)")
	}
	gotAllowed, gotRefused := publicKeyCounts()
	if got := gotAllowed - allowed; got != 1.0 {
		t.Fatalf("gotAllowed-allowed = %v, want %v", got, 1.0)
	}
	if got := gotRefused - refused; got != 1.0 {
		t.Fatalf("gotRefused-refused = %v, want %v", got, 1.0)
	}
}

// A session that signed in without a key is not a public key auth: it must
// not be counted as one, allowed or not.
func TestPublicKeyCounterSkipsKeylessSession(t *testing.T) {
	e := backendtest.New(t, backendtest.WithLogger(log.Default()))
	be := e.Backend
	if !be.AllowKeyless(e.Ctx) {
		t.Fatal("not true: be.AllowKeyless(e.Ctx)")
	}

	s := &Server{be: be}
	hs := handlerServer(t, e.Cfg, e.DB, e.Store, be, log.Default())
	srv := &ssh.Server{
		Handler: hs.authenticationMiddleware(func(s ssh.Session) {
			s.Exit(0) //nolint:errcheck
		}),
	}
	if err := srv.SetOption(ssh.KeyboardInteractiveAuth(s.KeyboardInteractiveHandler)); err != nil {
		t.Fatal(err)
	}

	allowed, refused := publicKeyCounts()
	sess := testsession.New(t, srv, &gossh.ClientConfig{
		User: "anon",
		Auth: []gossh.AuthMethod{gossh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) {
			return nil, nil
		})},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err := sess.Run(""); err != nil {
		t.Fatal(err)
	}
	gotAllowed, gotRefused := publicKeyCounts()
	if got := gotAllowed; got != allowed {
		t.Fatalf("gotAllowed = %v, want %v", got, allowed)
	}
	if got := gotRefused; got != refused {
		t.Fatalf("gotRefused = %v, want %v", got, refused)
	}
}

// The CLI command metric is labelled from the server's command tree, never
// with what the client typed, so clients cannot add series.
func TestCommandLabel(t *testing.T) {
	t.Parallel()
	newRoot := func() *cobra.Command {
		root := &cobra.Command{SilenceUsage: true, SilenceErrors: true}
		root.CompletionOptions.DisableDefaultCmd = true
		cmds := new(cmd.Commands) // builds the tree only; nothing runs
		root.AddCommand(cmds.RepoCommand(), cmds.GitReceivePackCommand())
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
	root.AddCommand(new(cmd.Commands).RepoCommand())
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

// TestSSHServerRefusesOverlongCommand checks that the server built by
// NewServer refuses an overlong exec command from an unregistered key with
// ErrCommandTooLong alone. A cobra "unknown command" error would mean the
// command handler parsed it, and a debug log line carrying the command would
// mean LoggingMiddleware parsed it, so the length check must be wired in
// ahead of both.
func TestSSHServerRefusesOverlongCommand(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	logger := log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	noAccess := access.NoAccess
	cfg.AnonAccess = &noAccess
	allowKeyless := false
	cfg.AllowKeyless = &allowKeyless
	addr, _ := startSSHServer(t, cfg, logger)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         sessionLoopsWait,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	run := func(cmd string) (string, error) {
		t.Helper()
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		defer sess.Close() //nolint: errcheck
		var stderr bytes.Buffer
		sess.Stderr = &stderr
		err = sess.Run(cmd)
		return stderr.String(), err
	}

	// An unknown command at the limit gets past the length check and is
	// refused further down the chain with some other error.
	stderr, err := run(strings.Repeat("a", MaxCommandLength))
	if err == nil {
		t.Fatal("command at the limit from an unregistered key succeeded")
	}
	if strings.Contains(stderr, ErrCommandTooLong.Error()) {
		t.Fatalf("command at the limit was refused as too long: %q", stderr)
	}

	// LoggingMiddleware logs the parsed command at debug level, so the log
	// shows whether it saw a command.
	if !strings.Contains(logs.String(), strings.Repeat("a", 64)) {
		t.Fatalf("debug log does not show the command at the limit, so it cannot show parsing: %q", logs.String())
	}

	// Suggestions stay on for short typos of a command name.
	stderr, _ = run("rpo")
	if !strings.Contains(stderr, "Did you mean this?") || !strings.Contains(stderr, "repo") {
		t.Fatalf("short typo: stderr %q does not suggest repo", stderr)
	}

	overlong := strings.Repeat("b", MaxCommandLength+1)
	stderr, err = run(overlong)
	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 1 {
		t.Fatalf("overlong command: got %v, want exit status 1 (stderr %q)", err, stderr)
	}
	if strings.TrimSpace(stderr) != ErrCommandTooLong.Error() {
		t.Fatalf("overlong command: stderr %q, want only %q", stderr, ErrCommandTooLong)
	}
	if strings.Contains(logs.String(), strings.Repeat("b", 64)) {
		t.Fatal("overlong command reached LoggingMiddleware")
	}
}

func TestCommandLengthMiddleware(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cmd     string
		refused bool
	}{
		{"empty", "", false},
		{"short", "repo info test", false},
		{"at limit", strings.Repeat("a", MaxCommandLength), false},
		{"over limit", strings.Repeat("a", MaxCommandLength+1), true},
		{"far over limit", "repo create " + strings.Repeat("a ", 50<<10), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var reached atomic.Bool
			sess := testsession.New(t, &ssh.Server{
				Handler: commandLengthMiddleware(func(s ssh.Session) {
					reached.Store(true)
					s.Exit(0) //nolint: errcheck
				}),
			}, nil)

			var stderr bytes.Buffer
			sess.Stderr = &stderr
			err := sess.Run(tc.cmd)

			if !tc.refused {
				if err != nil {
					t.Fatalf("run: %v (stderr %q)", err, stderr.String())
				}
				if !reached.Load() {
					t.Fatal("command within the limit did not reach the next handler")
				}
				return
			}

			var exitErr *gossh.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 1 {
				t.Fatalf("run: got %v, want exit status 1", err)
			}
			if reached.Load() {
				t.Fatal("overlong command reached the next handler")
			}
			if !strings.Contains(stderr.String(), ErrCommandTooLong.Error()) {
				t.Fatalf("stderr %q does not mention %q", stderr.String(), ErrCommandTooLong)
			}
		})
	}
}

// TestAuthenticationMiddlewareUserLookup checks which user a public-key
// session runs as: the key's owner, anonymous for an unregistered key, and
// no session at all when the lookup fails for another reason, so a database
// error cannot downgrade a registered user to anonymous.
func TestAuthenticationMiddlewareUserLookup(t *testing.T) {
	t.Parallel()

	e := backendtest.New(t, backendtest.WithLogger(log.Default()))
	ctx, cfg, dbx, dbstore, be := e.Ctx, e.Cfg, e.DB, e.Store, e.Backend
	dp := cfg.DataPath

	newKey := func(name string) gossh.Signer {
		t.Helper()
		kp, err := keygen.New(dp+"/"+name, keygen.WithKeyType(keygen.Ed25519))
		if err != nil {
			t.Fatal(err)
		}
		signer, err := gossh.NewSignerFromKey(kp.PrivateKey())
		if err != nil {
			t.Fatal(err)
		}
		return signer
	}
	alice := newKey("alice")
	stranger := newKey("stranger")
	if _, err := be.CreateUser(ctx, "alice", proto.UserOptions{
		PublicKeys: []gossh.PublicKey{alice.PublicKey()},
	}); err != nil {
		t.Fatal(err)
	}

	// The handler prints the session's user, or "anon" without one.
	logs := &syncBuffer{}
	hs := handlerServer(t, cfg, dbx, dbstore, be, log.New(logs))
	srv := &ssh.Server{
		PublicKeyHandler: (&Server{}).PublicKeyHandler,
		Handler: hs.authenticationMiddleware(func(s ssh.Session) {
			name := "anon"
			if u := proto.UserFromContext(s.Context()); u != nil {
				name = u.Username()
			}
			_, _ = s.Write([]byte(name))
			_ = s.Exit(0)
		}),
	}
	addr := testsession.Listen(t, srv)

	run := func(signer gossh.Signer) (string, string, error) {
		t.Helper()
		sess, err := testsession.NewClientSession(t, addr, &gossh.ClientConfig{
			User: "test",
			Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)},
		})
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		sess.Stdout = &stdout
		sess.Stderr = &stderr
		err = sess.Run("whoami")
		return stdout.String(), stderr.String(), err
	}

	if out, _, err := run(alice); err != nil || out != "alice" {
		t.Fatalf("registered key: got %q, %v; want alice", out, err)
	}
	if out, _, err := run(stranger); err != nil || out != "anon" {
		t.Fatalf("unregistered key: got %q, %v; want anon", out, err)
	}

	// A failing database makes the lookup fail with something other than
	// proto.ErrUserNotFound.
	if err := dbx.Close(); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := run(alice)
	var exitErr *gossh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() == 0 {
		t.Fatalf("lookup failure: got exit %v, want a non-zero exit status", err)
	}
	if out != "" {
		t.Fatalf("lookup failure: the session ran as %q", out)
	}
	if !strings.Contains(errOut, ErrUserLookupFailed.Error()) {
		t.Fatalf("lookup failure: stderr %q does not contain %q", errOut, ErrUserLookupFailed)
	}
	// The backend leaves logging the failure to the middleware.
	if !strings.Contains(logs.String(), "failed to look up the session's user") {
		t.Errorf("lookup failure: logs %q do not name the failed lookup", logs.String())
	}
}

func TestRedactCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			name: "git",
			in:   []string{"git-upload-pack", "repo"},
			want: []string{"git-upload-pack", "repo"},
		},
		{
			name: "webhook create secret",
			in:   []string{"repo", "webhook", "create", "r", "https://hooks.example.com/services/T0/B0/TOKEN", "--secret", "S3CRET", "-e", "push"},
			want: []string{"repo", "webhook", "create", "r", "https://hooks.example.com/[redacted]", "--secret", "[redacted]", "-e", "push"},
		},
		{
			name: "secret forms",
			in:   []string{"repo", "webhook", "update", "r", "1", "--secret=S3CRET", "-s", "S3CRET", "-sS3CRET", "-s=S3CRET"},
			want: []string{"repo", "webhook", "update", "r", "1", "--secret=[redacted]", "-s", "[redacted]", "-s[redacted]", "-s[redacted]"},
		},
		{
			name: "grouped secret forms",
			in:   []string{"repo", "webhook", "create", "r", "https://example.com", "-as", "S3CRET", "-asS3CRET", "-as=S3CRET", "-aes", "S3CRET"},
			want: []string{"repo", "webhook", "create", "r", "https://example.com", "-as", "[redacted]", "-as[redacted]", "-as[redacted]", "-aes", "[redacted]"},
		},
		{
			name: "webhook update url flag forms",
			in:   []string{"repo", "webhook", "update", "r", "1", "--url", "https://u:p@example.com/x?k=TOKEN", "--url=https://example.com/TOKEN", "-uhttps://example.com/TOKEN", "-u=https://example.com/TOKEN"},
			want: []string{"repo", "webhook", "update", "r", "1", "--url", "https://example.com/[redacted]", "--url=https://example.com/[redacted]", "-uhttps[redacted]", "-u=https://example.com/[redacted]"},
		},
		{
			name: "import token",
			in:   []string{"repo", "import", "r", "https://TOKEN@github.com/x/y.git", "--lfs-endpoint", "https://u:TOKEN@lfs.example.com", "-m"},
			want: []string{"repo", "import", "r", "https://github.com/[redacted]", "--lfs-endpoint", "https://lfs.example.com", "-m"},
		},
		{
			name: "unparsable url",
			in:   []string{"repo", "import", "r", "https://TOKEN@example.com/%zz"},
			want: []string{"repo", "import", "r", "[redacted]"},
		},
		{
			name: "scp remote and plain args",
			in:   []string{"repo", "import", "r", "git@github.com:x/y.git", "-d", "a description", "--name=Proj"},
			want: []string{"repo", "import", "r", "git@github.com:x/y.git", "-d", "a description", "--name=Proj"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := slices.Clone(tt.in)
			got := redactCommand(in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("redactCommand(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !slices.Equal(in, tt.in) {
				t.Errorf("redactCommand modified its input: %q", in)
			}
			for _, a := range got {
				if strings.Contains(a, "TOKEN") || strings.Contains(a, "S3CRET") {
					t.Errorf("redactCommand(%q) leaked a secret: %q", tt.in, got)
				}
			}
		})
	}
}

// TestSessionContextPerSession checks that concurrent sessions multiplexed over
// one connection (as OpenSSH's ControlMaster does) each see their own session,
// and so their own GIT_PROTOCOL, in the context the commands run with.
func TestSessionContextPerSession(t *testing.T) {
	t.Parallel()

	var arrived sync.WaitGroup
	arrived.Add(2)
	allArrived := make(chan struct{})
	go func() {
		arrived.Wait()
		close(allArrived)
	}()

	handler := func(s ssh.Session) {
		ctx := sessionContext(s)
		// Hold every session until both have gone through the
		// middleware, so a session stored per connection would be
		// overwritten by the other one.
		arrived.Done()
		select {
		case <-allArrived:
		case <-time.After(sessionLoopsWait):
			_, _ = io.WriteString(s.Stderr(), "timed out waiting for the other session")
			_ = s.Exit(1)
			return
		}
		var proto string
		if sess := cmd.SessionFromContext(ctx); sess != nil {
			for _, env := range sess.Environ() {
				if v, ok := strings.CutPrefix(env, "GIT_PROTOCOL="); ok {
					proto = v
				}
			}
		}
		_, _ = io.WriteString(s, proto)
	}

	srv := &ssh.Server{
		Handler: handler,
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = srv.Close() })

	c, err := gossh.Dial("tcp", l.Addr().String(), &gossh.ClientConfig{
		User:            "test",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         sessionLoopsWait,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	protos := []string{"version=1", "version=2"}
	got := make([]string, len(protos))
	errs := make([]error, len(protos))
	var wg sync.WaitGroup
	for i, p := range protos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := c.NewSession()
			if err != nil {
				errs[i] = err
				return
			}
			defer sess.Close() //nolint: errcheck
			if err := sess.Setenv("GIT_PROTOCOL", p); err != nil {
				errs[i] = err
				return
			}
			out, err := sess.Output("git-upload-pack 'repo'")
			got[i], errs[i] = string(out), err
		}()
	}
	wg.Wait()

	for i, p := range protos {
		if errs[i] != nil {
			t.Fatalf("session %d: %v", i, errs[i])
		}
		if got[i] != p {
			t.Errorf("session %d saw GIT_PROTOCOL %q, want %q", i, got[i], p)
		}
	}
}
