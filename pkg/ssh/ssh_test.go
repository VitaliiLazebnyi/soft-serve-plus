package ssh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssh/cmd"
	"github.com/charmbracelet/soft-serve/pkg/store"
	gossh "golang.org/x/crypto/ssh"
)

// The server presents the host key it is given, and does not read or
// create the file at ssh.key_path for it.
func TestServerPresentsGivenHostKey(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := serverContext(t, cfg, log.Default())
	d := depsOf(ctx)
	d.hostKey = testHostKey(t, filepath.Join(t.TempDir(), "given"))
	if err := os.Remove(cfg.SSH.KeyPath); err != nil {
		t.Fatal(err)
	}
	want, err := gossh.NewPublicKey(d.hostKey.CryptoPublicKey())
	if err != nil {
		t.Fatal(err)
	}

	s, err := newTestSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, s)

	var got gossh.PublicKey
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User: "test",
		Auth: []gossh.AuthMethod{gossh.KeyboardInteractive(noAnswers)},
		HostKeyCallback: func(_ string, _ net.Addr, key gossh.PublicKey) error {
			got = key
			return nil
		},
		Timeout: sessionLoopsWait,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if got == nil || !bytes.Equal(got.Marshal(), want.Marshal()) {
		t.Errorf("server presented host key %v, want the given one", got)
	}
	if _, err := os.Stat(cfg.SSH.KeyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want no key file created", cfg.SSH.KeyPath, err)
	}
}

// NewServer refuses to build a server without a config or a host key.
func TestNewServerNeedsConfigAndHostKey(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	d := depsOf(serverContext(t, cfg, log.Default()))
	if _, err := NewServer(nil, d.dbx, d.st, d.be, d.hostKey, d.tokens, d.logger); !errors.Is(err, config.ErrNilConfig) {
		t.Errorf("NewServer without a config: %v, want %v", err, config.ErrNilConfig)
	}
	if _, err := NewServer(d.cfg, d.dbx, d.st, d.be, nil, d.tokens, d.logger); !errors.Is(err, errNilHostKey) {
		t.Errorf("NewServer without a host key: %v, want %v", err, errNilHostKey)
	}
}

// serve serves s on a local port until the test ends and returns its
// address.
func serve(t *testing.T, s *Server) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = s.Close() })
	return l.Addr().String()
}

// fixtureAdmin returns the admin account the first migration creates, the
// owner of fixture repositories: the backend refuses a create by a caller
// who may not create, as an anonymous caller may not by default.
func fixtureAdmin(t testing.TB, be *backend.Backend) proto.User {
	t.Helper()
	u, err := be.User(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

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

// The key exchanges, ciphers and MACs are fixed; the public key
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

// serverContext returns a context with what NewServer needs: cfg, on the
// test database and validated (see backendtest.UseConfig), the logger, the
// host key and token signer, and a migrated database, store and backend. The
// host key is the one at cfg.SSH.KeyPath, which it creates if there is none,
// as the server does at start.
//
// The caller sets cfg.DataPath to a t.TempDir before calling it, and starts
// the server after: closeTestDB, which closes the database, then runs after
// the server is closed and before the directory is removed.
func serverContext(t *testing.T, cfg *config.Config, logger *log.Logger) context.Context {
	t.Helper()
	e := backendtest.New(t, backendtest.UseConfig(cfg), backendtest.WithLogger(logger))
	closeTestDB(t, e.DB)
	hostKey := testHostKey(t, cfg.SSH.KeyPath)
	tokens, err := jwk.NewSigner(hostKey, cfg.HTTP.PublicURL)
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(e.Ctx, testDepsKey{}, &testDeps{
		cfg:     cfg,
		dbx:     e.DB,
		st:      e.Store,
		be:      e.Backend,
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

// prefixConn is a net.Conn whose first reads return prefix, read off the
// connection before it was handed over.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// stopServer is how a test stops an SSH server.
type stopServer func(*Server) error

// A connection still in its handshake when the server stops is cut off too.
// The SSH library closes on Close, and waits for on Shutdown, only the
// connections that have finished their handshake, so one that finished it
// just after either ran was authenticated after the stop and stayed open
// until its idle timeout: the drain cut-off did not cut it off.
// The test stops the server while a client is mid-handshake, and only then
// lets the client finish it.
func TestStopCutsOffHandshakeInProgress(t *testing.T) {
	t.Parallel()
	for name, stop := range map[string]stopServer{
		"close": func(s *Server) error { return s.Close() },
		"shutdown": func(s *Server) error {
			// Nothing has finished its handshake, so there is nothing
			// to drain.
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			return s.Shutdown(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := config.DefaultConfig()
			cfg.DataPath = t.TempDir()
			ctx := serverContext(t, cfg, log.Default())
			s, err := newTestSSHServer(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go s.Serve(l) //nolint: errcheck

			raw, err := (&net.Dialer{Timeout: time.Minute}).DialContext(ctx, "tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			if err := raw.SetDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			// The server's version line: it has accepted the connection
			// and is in its handshake.
			br := bufio.NewReader(raw)
			version, err := br.ReadString('\n')
			if err != nil || !strings.HasPrefix(version, "SSH-2.0-") {
				t.Fatalf("version line %q, %v", version, err)
			}

			if err := stop(s); err != nil {
				t.Fatalf("stop: %v", err)
			}

			// Finish the handshake. The server may refuse it, or let it
			// finish and then close the connection; either way the
			// connection must end.
			conn := &prefixConn{Conn: raw, r: io.MultiReader(strings.NewReader(version), br)}
			requireCutOff(t, conn, l.Addr().String(), "a connection that finished its handshake after the server stopped")
		})
	}
}

// requireCutOff finishes the client side of the SSH handshake on conn, which
// has a deadline a minute away, and fails the test unless the server cuts
// the connection off: during the handshake, or once it is done, within a
// minute. A handshake that only ran into the deadline was not cut off.
func requireCutOff(t *testing.T, conn net.Conn, addr, what string) {
	t.Helper()
	c, chans, reqs, err := gossh.NewClientConn(conn, addr, clientConfig(t))
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s: the handshake hung until its deadline instead of being cut off: %v", what, err)
		}
		return // cut off during the handshake
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	client := gossh.NewClient(c, chans, reqs)
	defer client.Close() //nolint: errcheck
	closed := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Minute):
		t.Fatalf("%s is still open a minute later", what)
	}
}

// A connection the listener accepted before the server stopped, whose
// connection callback runs only after the stop, is refused: Accept returns,
// the stop runs, and only then does the library's HandleConn reach the
// callback. Nothing else would close it, since the stop has already swept
// the connections it holds. A gate in the callback holds the connection
// there while the server stops.
func TestStopRefusesConnectionCalledBackAfter(t *testing.T) {
	t.Parallel()
	for name, stop := range map[string]stopServer{
		"close": func(s *Server) error { return s.Close() },
		"shutdown": func(s *Server) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			return s.Shutdown(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := config.DefaultConfig()
			cfg.DataPath = t.TempDir()
			ctx := serverContext(t, cfg, log.Default())
			s, err := newTestSSHServer(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			entered, gate := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(gate) }) })
			s.srv.ConnCallback = s.live.track(func(_ ssh.Context, c net.Conn) net.Conn {
				close(entered)
				<-gate
				return c
			})
			l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go s.Serve(l) //nolint: errcheck
			raw, err := (&net.Dialer{Timeout: time.Minute}).DialContext(ctx, "tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			if err := raw.SetDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Minute):
				t.Fatal("the connection callback was not reached within a minute")
			}

			if err := stop(s); err != nil {
				t.Fatalf("stop: %v", err)
			}
			once.Do(func() { close(gate) })

			requireCutOff(t, raw, l.Addr().String(), "a connection accepted before the stop and called back after it")
		})
	}
}

// `ssh host repo commit REPO REV` on a commit that renames a file to a
// name git quotes ended the whole server from the diff parser's goroutine.
// Over a real SSH connection the command must print the rename, and the
// server must still answer afterwards.
func TestSSHRepoCommitRenameQuotedName(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, ctx := startTestServer(t, cfg)
	if _, err := depsOf(ctx).be.CreateRepository(ctx, "renames", fixtureAdmin(t, depsOf(ctx).be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(cfg.DataPath, "repos", "renames.git")
	work := t.TempDir()
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = work
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q", "-b", "main")
	body := strings.Repeat("same line\n", 20)
	put := func(name, content string) {
		blob := git(content, "hash-object", "-w", "--stdin")
		git("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	}
	put("plain.txt", body)
	git("", "commit", "-q", "-m", "add")
	git("", "update-index", "--force-remove", "--", "plain.txt")
	put("café.txt", body+"changed\n")
	git("", "commit", "-q", "-m", "to café")
	to := git("", "rev-parse", "HEAD")
	git("", "update-index", "--force-remove", "--", "café.txt")
	put("plain.txt", body+"changed\nagain\n")
	git("", "commit", "-q", "-m", "from café")
	from := git("", "rev-parse", "HEAD")
	git("", "push", "-q", bare, "main")

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	for _, rev := range []string{to, from, to} {
		s, err := c.NewSession()
		if err != nil {
			t.Fatalf("new session (server gone?): %v", err)
		}
		out, err := s.Output("repo commit renames " + rev)
		_ = s.Close()
		if err != nil {
			t.Fatalf("repo commit %s: %v\n%s", rev, err, out)
		}
		for _, want := range []string{"rename from ", "rename to ", "café.txt"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("repo commit %s: output lacks %q:\n%s", rev, want, out)
			}
		}
	}
}

// Tracker 266: a client that closes a fetch's channel while git waits for
// its wants, keeping the connection open as an OpenSSH ControlMaster
// does, has gone away; git then fails for want of input, and that is not
// a server failure to log at Error.
func TestSSHFetchChannelClosedIsNotAServerError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	var logs syncBuffer
	logger := log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})
	addr, ctx := startSSHServer(t, cfg, logger)

	be := depsOf(ctx).be
	if _, err := be.CreateRepository(ctx, "small", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	writeManyRefs(t, filepath.Join(cfg.DataPath, "repos", "small.git"), 1)

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// An input that stays open: without one the client sends the end of
	// its input at once, and git fails for that whether or not the channel
	// is closed.
	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	s.Stdin = stdin
	if err := s.Start("git-upload-pack small"); err != nil {
		t.Fatal(err)
	}
	// Read the advertisement to its flush packet: git now waits for the
	// client's wants.
	for {
		var size [4]byte
		if _, err := io.ReadFull(stdout, size[:]); err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseUint(string(size[:]), 16, 16)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if _, err := io.CopyN(io.Discard, stdout, int64(n)-4); err != nil {
			t.Fatal(err)
		}
	}
	// Close the channel, keeping the connection.
	_ = s.Close()

	waitCtx, cancel := context.WithTimeout(t.Context(), sessionLoopsWait)
	defer cancel()
	for !strings.Contains(logs.String(), "git service") {
		if waitCtx.Err() != nil {
			t.Fatalf("the fetch did not end: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := logs.String(); strings.Contains(out, "ERRO") {
		t.Fatalf("a client that closed its channel was logged as an error:\n%s", out)
	}
}

// writeManyRefs gives the bare repository dir n branches on one empty
// commit, written as packed-refs, so its ref advertisement is about 60
// bytes per branch.
func writeManyRefs(t *testing.T, dir string, n int) {
	t.Helper()
	run := func(stdin string, args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := run("", "commit-tree", run("", "mktree"), "-m", "refs")
	var b strings.Builder
	b.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for i := range n {
		fmt.Fprintf(&b, "%s refs/heads/b%07d\n", commit, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// copyBlockedOnWindow reports whether the git service's copy of git's output
// to an SSH session waits for the client to open the channel's window.
func copyBlockedOnWindow() bool {
	return goroutineBlocked("[sync.Cond.Wait",
		"golang.org/x/crypto/ssh.(*window).reserve(",
		"charm.land/ssh.(*session).Write(",
		"github.com/charmbracelet/soft-serve/pkg/gitserve.progressWriter.Write(",
	)
}

// A fetch whose channel the client closes while the connection
// stays open, as interrupting a clone over an OpenSSH ControlMaster
// connection does, must not keep git running and the session's place under
// max_sessions. Two things stop git: the session's own context, which ends
// when its channel closes (channelContext), and the failed write of the
// advertisement to the closed channel, whichever comes first.
func TestSSHFetchChannelClosedFreesSession(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, ctx := startTestServer(t, cfg, withMaxSessions(1))

	be := depsOf(ctx).be
	if _, err := be.CreateRepository(ctx, "big", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	// About 3 MB of advertisement: more than the client's channel window
	// and git's pipe together, so git blocks writing it while the client
	// reads nothing.
	writeManyRefs(t, filepath.Join(cfg.DataPath, "repos", "big.git"), 50_000)

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start("git-upload-pack big"); err != nil {
		t.Fatal(err)
	}
	// Wait for the advertisement to start and for the server's copy of it
	// to block on the client's full channel window, then close the
	// channel, keeping the connection.
	if _, err := stdout.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the advertisement to fill the channel window", func() bool {
		return copyBlockedOnWindow()
	})
	_ = s.Close()

	// The channel's place is freed when it closes, but the command's only
	// when its handler returned, which it does only once git is gone. Until
	// then another command on the connection is refused.
	ctxWait, cancel := context.WithTimeout(t.Context(), sessionLoopsWait)
	defer cancel()
	var last string
	for ctxWait.Err() == nil {
		s2, err := c.NewSession()
		if err != nil {
			last = err.Error()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var stderr bytes.Buffer
		s2.Stderr = &stderr
		err = s2.Run("repo list")
		_ = s2.Close()
		if err == nil {
			return
		}
		last = fmt.Sprintf("%v: %s", err, stderr.String())
		if !strings.Contains(stderr.String(), msgTooManySessions) {
			t.Fatalf("repo list: %s", last)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("command still refused after the fetch's channel closed: %s", last)
}

// NewServer bounds the handshake with config.SSHHandshakeTimeout and
// the sessions per connection with config.SSHMaxSessions: one connection
// opens that many sessions, and the next is refused.
func TestSSHServerFixedLimits(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := serverContext(t, cfg, log.Default())
	s, err := newTestSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.srv.HandshakeTimeout != config.SSHHandshakeTimeout || config.SSHHandshakeTimeout <= 0 {
		t.Errorf("handshake timeout = %s, want config.SSHHandshakeTimeout (%s)", s.srv.HandshakeTimeout, config.SSHHandshakeTimeout)
	}
	if config.SSHMaxSessions <= 0 {
		t.Fatalf("config.SSHMaxSessions = %d, want a limit", config.SSHMaxSessions)
	}

	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	c, err := dialClient(t, l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	for i := range config.SSHMaxSessions {
		sess, err := c.NewSession()
		if err != nil {
			t.Fatalf("session %d of %d: %v", i+1, config.SSHMaxSessions, err)
		}
		defer sess.Close() //nolint: errcheck
	}
	if sess, err := c.NewSession(); err == nil {
		_ = sess.Close()
		t.Fatalf("session %d on one connection accepted, want refused", config.SSHMaxSessions+1)
	} else if oce, ok := err.(*gossh.OpenChannelError); !ok || oce.Reason != gossh.ResourceShortage {
		t.Fatalf("session %d: err = %v, want resource shortage", config.SSHMaxSessions+1, err)
	}
}

// TestSSHServerClosesStalledHandshake checks that a client which connects but
// never completes the key exchange is disconnected after the handshake
// timeout rather than held until the much longer idle timeout.
func TestSSHServerClosesStalledHandshake(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	// A close within waitClosed's wait can only come from the handshake
	// timeout while the idle timeout is longer.
	if idle := time.Duration(cfg.SSH.IdleTimeout) * time.Second; idle <= sessionLoopsWait {
		t.Fatalf("idle timeout %s is within the %s wait", idle, sessionLoopsWait)
	}
	addr, _ := startUnixServer(t, cfg, withHandshakeTimeout(time.Second))

	// Send a version banner and then stall in the key exchange.
	waitClosed(t, dialUnix(t, addr, "SSH-2.0-stall\r\n"), "a stalled handshake")
}

// The flow the README gives for `pubkey add`, over a real SSH session: the
// signature ssh-keygen -Y sign makes with the new key is piped into the
// command, which reads it from the session's input. In a session with a pty
// the command refuses at once rather than wait for a terminal's input.
func TestPubkeyAddProofOverSSH(t *testing.T) {
	t.Parallel()
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found")
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, ctx := startTestServer(t, cfg)
	be := depsOf(ctx).be

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{signer.PublicKey()}}); err != nil {
		t.Fatal(err)
	}

	key := filepath.Join(t.TempDir(), "id_new")
	if out, err := exec.CommandContext(t.Context(), keygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	sign := exec.CommandContext(t.Context(), keygen, "-Y", "sign", "-n", "pubkey-add@soft-serve-plus", "-f", key)
	sign.Stdin = strings.NewReader("alice")
	proof, err := sign.Output()
	if err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v", err)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "alice",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         sessionLoopsWait,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	run := func(pty bool, stdin []byte) (string, error) {
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close() //nolint: errcheck
		if pty {
			if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
				t.Fatal(err)
			}
		}
		var stderr bytes.Buffer
		s.Stdin = bytes.NewReader(stdin)
		s.Stderr = &stderr
		err = s.Run(`pubkey add "` + strings.TrimSpace(string(pub)) + `"`)
		return stderr.String(), err
	}

	// In a pty the input is a terminal, so the command refuses at once,
	// even with a valid proof on that input, and says what to do.
	if out, err := run(true, proof); err == nil || !strings.Contains(out, "needs proof") || !strings.Contains(out, "terminal") {
		t.Fatalf("pubkey add in a pty: %v: %q", err, out)
	}
	if out, err := run(false, proof); err != nil {
		t.Fatalf("pubkey add with the proof: %v: %q", err, out)
	}
	u, err := be.User(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(u.PublicKeys()); n != 2 {
		t.Fatalf("alice has %d keys, want 2", n)
	}
}
