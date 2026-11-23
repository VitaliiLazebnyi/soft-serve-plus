package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io/fs"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/ssh/cmd"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	gossh "golang.org/x/crypto/ssh"
)

// sha1Algorithms are the SHA-1 based algorithms x/crypto still offers by
// default and that the server must not offer unless configured to.
var sha1Algorithms = []string{
	gossh.InsecureKeyExchangeDH14SHA1,
	gossh.InsecureKeyExchangeDH1SHA1,
	gossh.InsecureKeyExchangeDHGEXSHA1,
	gossh.HMACSHA1,
	gossh.InsecureHMACSHA196,
}

func TestServerConfigExcludesSHA1ByDefault(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sc := serverConfig(cfg, log.Default())
	if len(sc.KeyExchanges) == 0 || len(sc.Ciphers) == 0 || len(sc.MACs) == 0 {
		t.Fatalf("serverConfig leaves algorithms to x/crypto defaults: kex %v, ciphers %v, macs %v",
			sc.KeyExchanges, sc.Ciphers, sc.MACs)
	}
	for _, list := range [][]string{sc.KeyExchanges, sc.Ciphers, sc.MACs} {
		for _, a := range list {
			if slices.Contains(sha1Algorithms, a) {
				t.Errorf("serverConfig offers %q by default", a)
			}
		}
	}
	if !slices.Contains(sc.KeyExchanges, gossh.KeyExchangeCurve25519) {
		t.Errorf("default key exchanges %v lack %s", sc.KeyExchanges, gossh.KeyExchangeCurve25519)
	}
	if len(sc.PublicKeyAuthAlgorithms) == 0 {
		t.Fatal("serverConfig leaves public key auth algorithms to x/crypto defaults")
	}
	for _, a := range []string{gossh.KeyAlgoRSA, gossh.InsecureKeyAlgoDSA} { //nolint:staticcheck // checks the deprecated algorithm stays off
		if slices.Contains(sc.PublicKeyAuthAlgorithms, a) {
			t.Errorf("serverConfig accepts %q signatures by default", a)
		}
	}
}

// The key exchanges, ciphers and MACs are fixed (R68); the public key
// authentication algorithms follow the config.
func TestServerConfigHonoursConfiguredAlgorithms(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.PublicKeyAuthAlgorithms = []string{gossh.KeyAlgoED25519, gossh.KeyAlgoRSA}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sc := serverConfig(cfg, log.Default())
	if !slices.Equal(sc.KeyExchanges, config.DefaultSSHKexAlgorithms()) {
		t.Errorf("key exchanges = %v, want %v", sc.KeyExchanges, config.DefaultSSHKexAlgorithms())
	}
	if !slices.Equal(sc.Ciphers, config.DefaultSSHCiphers()) {
		t.Errorf("ciphers = %v, want %v", sc.Ciphers, config.DefaultSSHCiphers())
	}
	if !slices.Equal(sc.MACs, config.DefaultSSHMACs()) {
		t.Errorf("macs = %v, want %v", sc.MACs, config.DefaultSSHMACs())
	}
	if !slices.Equal(sc.PublicKeyAuthAlgorithms, cfg.SSH.PublicKeyAuthAlgorithms) {
		t.Errorf("public key auth algorithms = %v, want %v", sc.PublicKeyAuthAlgorithms, cfg.SSH.PublicKeyAuthAlgorithms)
	}
}

// limit changes one of the server's fixed limits for a test.
type limit func(*serverLimits)

// withMaxSessions sets the sessions one connection may have open at once.
func withMaxSessions(n int) limit {
	return func(l *serverLimits) { l.maxSessions = n }
}

// withHandshakeTimeout sets the time a client has to authenticate.
func withHandshakeTimeout(d time.Duration) limit {
	return func(l *serverLimits) { l.handshakeTimeout = d }
}

// newTestSSHServer is NewServer, with the dependencies ctx carries (see
// serverContext), and the fixed limits changed by lims.
func newTestSSHServer(ctx context.Context, lims ...limit) (*Server, error) {
	lim := serverLimits{handshakeTimeout: config.SSHHandshakeTimeout, maxSessions: config.SSHMaxSessions}
	for _, l := range lims {
		l(&lim)
	}
	d := depsOf(ctx)
	return newServer(d.cfg, d.dbx, d.st, d.be, d.hostKey, d.tokens, d.logger, lim)
}

// startTestServer starts a full SSH server built by NewServer with cfg,
// and the fixed limits changed by lims, and returns its address and the
// context it runs with, which holds its backend.
func startTestServer(t *testing.T, cfg *config.Config, lims ...limit) (string, context.Context) {
	t.Helper()
	return startSSHServer(t, cfg, log.Default(), lims...)
}

// startSSHServer starts a full SSH server built by NewServer with cfg and
// logger and returns its address and the context it runs with, which holds its
// backend.
func startSSHServer(t *testing.T, cfg *config.Config, logger *log.Logger, lims ...limit) (string, context.Context) {
	t.Helper()
	addr, ctx, _ := startServerWithLogger(t, cfg, logger, lims...)
	return addr, ctx
}

// startServerWithLogger starts an SSH server with cfg and logger on a local
// port and returns its address, the context it runs with and the server.
func startServerWithLogger(t *testing.T, cfg *config.Config, logger *log.Logger, lims ...limit) (string, context.Context, *Server) {
	t.Helper()
	ctx := serverContext(t, cfg, logger)
	s, err := newTestSSHServer(ctx, lims...)
	if err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = s.Close() })
	return l.Addr().String(), ctx, s
}

// testDeps are what a test server is built from. The server takes them as
// parameters; the tests carry them on the context the server runs with,
// under testDepsKey, so that a test can reach the server's backend.
type testDeps struct {
	cfg     *config.Config
	dbx     *db.DB
	st      store.Store
	be      *backend.Backend
	hostKey *config.HostKey
	tokens  *jwk.Signer
	logger  *log.Logger
}

// testDepsKey is the context key of a test server's testDeps.
type testDepsKey struct{}

// depsOf returns the testDeps ctx carries; serverContext's contexts always
// carry them.
func depsOf(ctx context.Context) *testDeps {
	return ctx.Value(testDepsKey{}).(*testDeps)
}

// serverContext validates cfg and returns a context with what NewServer
// needs: the config, logger, host key and token signer, and a migrated
// database, store and backend. The host key is the one at cfg.SSH.KeyPath,
// which it creates if there is none, as the server does at start.
//
// The caller sets cfg.DataPath to a t.TempDir before calling it, and starts
// the server after: closeTestDB, which closes the database, then runs after
// the server is closed and before the directory is removed.
func serverContext(t *testing.T, cfg *config.Config, logger *log.Logger) context.Context {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	closeTestDB(t, dbx)
	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	dbstore := database.New(nil)
	be := backend.New(cfg, dbx, dbstore, logger)
	hostKey := testHostKey(t, cfg.SSH.KeyPath)
	tokens, err := jwk.NewSigner(hostKey, cfg.HTTP.PublicURL)
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(ctx, testDepsKey{}, &testDeps{
		cfg:     cfg,
		dbx:     dbx,
		st:      dbstore,
		be:      be,
		hostKey: hostKey,
		tokens:  tokens,
		logger:  logger,
	})
}

// testHostKey returns the host key at path, which it creates first if
// there is none.
func testHostKey(t *testing.T, path string) *config.HostKey {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if _, err := keygen.New(path, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite()); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.DefaultConfig()
	cfg.SSH.KeyPath = path
	key, err := config.KeyPair(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// handlerServer returns a Server with cfg, be and logger, and the commands
// built with them, dbx and st, but no SSH server of its own: for a test
// that serves its middleware on a server it sets up itself.
func handlerServer(t testing.TB, cfg *config.Config, dbx *db.DB, st store.Store, be *backend.Backend, logger *log.Logger) *Server {
	t.Helper()
	cmds, err := cmd.New(cmd.Deps{Config: cfg, Backend: be, DB: dbx, Store: st, Logger: logger.WithPrefix("ssh")})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: cfg, be: be, logger: logger.WithPrefix("ssh"), cmds: cmds}
}

// closeTestDB closes dbx when tb ends, and waits until its last connection
// has closed. Call it after tb.TempDir and before starting a server, so that
// it runs after the server is closed and before the temporary directory is
// removed.
//
// The server's session goroutines may outlive the test: an ssh.Server's
// Close does not wait for its handlers, and a TUI program leaves its
// commands running when it exits, as Bubble Tea does not wait for them. One
// of them, a command opening a repository say, can be in a query when the
// test ends. sql.DB.Close closes only the idle connections; one in use is
// closed when the query returns it. Until then the SQLite file stays open,
// and Windows does not delete an open file, so the TempDir cleanup fails
// with "The directory is not empty". A closed pool opens no new connection,
// so once the count is zero it stays zero.
func closeTestDB(tb testing.TB, dbx *db.DB) {
	tb.Helper()
	tb.Cleanup(func() {
		if err := dbx.Close(); err != nil {
			tb.Errorf("close database: %v", err)
		}
		deadline := time.Now().Add(sessionLoopsWait)
		for {
			n := dbx.Stats().OpenConnections
			if n == 0 {
				return
			}
			if time.Now().After(deadline) {
				tb.Errorf("%d database connections still open %s after the database was closed", n, sessionLoopsWait)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// dialWith connects to addr with a client limited to the given algorithms
// (nil leaves x/crypto's client defaults) and returns the handshake error.
func dialWith(t *testing.T, addr string, kex, ciphers, macs []string) error {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		Config: gossh.Config{
			KeyExchanges: kex,
			Ciphers:      ciphers,
			MACs:         macs,
		},
		User:            "test",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         sessionLoopsWait,
	})
	if err == nil {
		_ = c.Close()
	}
	return err
}

func wantNoCommonAlgorithm(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: client connected, want no common algorithm", what)
		return
	}
	if !strings.Contains(err.Error(), "no common algorithm") {
		t.Errorf("%s: err = %v, want no common algorithm", what, err)
	}
}

func TestSSHServerRefusesSHA1ByDefault(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, _ := startTestServer(t, cfg)

	// A non-AEAD cipher makes the MAC part of the negotiation.
	ctr := []string{gossh.CipherAES128CTR}

	if err := dialWith(t, addr, nil, nil, nil); err != nil {
		t.Fatalf("default client: %v", err)
	}
	if err := dialWith(t, addr, nil, ctr, []string{gossh.HMACSHA256}); err != nil {
		t.Fatalf("client with hmac-sha2-256: %v", err)
	}
	wantNoCommonAlgorithm(t, dialWith(t, addr, []string{gossh.InsecureKeyExchangeDH14SHA1}, nil, nil),
		"client limited to diffie-hellman-group14-sha1")
	wantNoCommonAlgorithm(t, dialWith(t, addr, []string{gossh.InsecureKeyExchangeDHGEXSHA1}, nil, nil),
		"client limited to diffie-hellman-group-exchange-sha1")
	wantNoCommonAlgorithm(t, dialWith(t, addr, nil, ctr, []string{gossh.HMACSHA1}),
		"client limited to hmac-sha1")
	wantNoCommonAlgorithm(t, dialWith(t, addr, nil, ctr, []string{gossh.InsecureHMACSHA196}),
		"client limited to hmac-sha1-96")
}

// dialWithSigner connects to addr authenticating with signer and returns
// the handshake error.
func dialWithSigner(addr string, signer gossh.Signer) error {
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         sessionLoopsWait,
	})
	if err == nil {
		_ = c.Close()
	}
	return err
}

// rsaSigners returns signers for one RSA key, each limited to one of algos.
func rsaSigners(t *testing.T, algos ...string) map[string]gossh.Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	as, ok := signer.(gossh.AlgorithmSigner)
	if !ok {
		t.Fatal("RSA signer is not an AlgorithmSigner")
	}
	signers := make(map[string]gossh.Signer, len(algos))
	for _, a := range algos {
		s, err := gossh.NewSignerWithAlgorithms(as, []string{a})
		if err != nil {
			t.Fatal(err)
		}
		signers[a] = s
	}
	return signers
}

// wantAuthFailure checks that public key authentication was refused. The
// x/crypto client reads the server-sig-algs list the server advertises and
// gives up before signing when its algorithm is not on it; a client that
// ignores the list is turned away by the server instead.
func wantAuthFailure(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: client authenticated, want it refused", what)
		return
	}
	if !strings.Contains(err.Error(), "no common public key signature algorithm") &&
		!strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("%s: err = %v, want an authentication failure", what, err)
	}
}

func TestSSHServerRefusesSHA1SignaturesByDefault(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, _ := startTestServer(t, cfg)

	signers := rsaSigners(t, gossh.KeyAlgoRSASHA256, gossh.KeyAlgoRSASHA512, gossh.KeyAlgoRSA)
	for _, a := range []string{gossh.KeyAlgoRSASHA256, gossh.KeyAlgoRSASHA512} {
		if err := dialWithSigner(addr, signers[a]); err != nil {
			t.Errorf("RSA key signing with %s: %v", a, err)
		}
	}
	wantAuthFailure(t, dialWithSigner(addr, signers[gossh.KeyAlgoRSA]), "RSA key signing with ssh-rsa")
}

func TestSSHServerHonoursConfiguredPublicKeyAuthAlgorithms(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.PublicKeyAuthAlgorithms = []string{gossh.KeyAlgoRSA}
	addr, _ := startTestServer(t, cfg)

	signers := rsaSigners(t, gossh.KeyAlgoRSASHA256, gossh.KeyAlgoRSA)
	if err := dialWithSigner(addr, signers[gossh.KeyAlgoRSA]); err != nil {
		t.Errorf("RSA key signing with ssh-rsa, which is configured: %v", err)
	}
	wantAuthFailure(t, dialWithSigner(addr, signers[gossh.KeyAlgoRSASHA256]), "RSA key signing with rsa-sha2-256")
}
