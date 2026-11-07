package testscript

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/crypto/ssh"
)

// scriptT is a testscript.T that records whether a script failed instead of
// failing the test that runs it, so a test can check that a command fails.
type scriptT struct {
	failed bool
	log    strings.Builder
}

type failNow struct{}

func (t *scriptT) Skip(args ...any)  { t.Fatal(args...) }
func (t *scriptT) Fatal(args ...any) { t.Log(args...); t.FailNow() }
func (t *scriptT) Parallel()         {}
func (t *scriptT) Log(args ...any)   { fmt.Fprintln(&t.log, args...) }
func (t *scriptT) FailNow()          { t.failed = true; panic(failNow{}) }
func (t *scriptT) Verbose() bool     { return false }

func (t *scriptT) Run(_ string, f func(testscript.T)) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(failNow); !ok {
				panic(r)
			}
		}
	}()
	f(t)
}

// runCommandScript runs script with the harness commands and env, and
// reports whether it passed along with its log. A command that panics fails
// the test.
func runCommandScript(t *testing.T, script string, env map[string]string) (bool, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cmd.txtar"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	st := &scriptT{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("script panicked: %v\n%s", r, st.log.String())
			}
		}()
		testscript.RunT(st, testscript.Params{
			Dir: dir,
			Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
				"readfile":               cmdReadfile,
				"ensureserverrunning":    cmdEnsureServerRunning,
				"ensureservernotrunning": cmdEnsureServerNotRunning,
				"stopserver":             cmdStopserver,
				"ui":                     cmdUI(signer),
				"writestderr": func(ts *testscript.TestScript, _ bool, args []string) {
					fmt.Fprint(ts.Stderr(), strings.Join(args, " "))
				},
			},
			Setup: func(e *testscript.Env) error {
				for k, v := range env {
					e.Setenv(k, v)
				}
				return nil
			},
		})
	}()
	return !st.failed, st.log.String()
}

// listeningPort returns the port of a listener that accepts and drops
// connections until the test ends.
func listeningPort(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(loopback, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// closedPort returns a port that nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(loopback, "0"))
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	return port
}

func TestHarnessCommands(t *testing.T) {
	t.Parallel()

	up, down := listeningPort(t), closedPort(t)
	env := map[string]string{"UP_PORT": up, "DOWN_PORT": down, "SSH_PORT": down}

	for _, tc := range []struct {
		name   string
		script string
		pass   bool
	}{
		{"readfile without a file", "readfile\n", false},
		{"readfile", "readfile f.txt\nstdout '^expected$'\n-- f.txt --\nexpected\n", true},
		{"readfile with extra arguments", "readfile f.txt 'expected'\n-- f.txt --\nexpected\n", false},
		{"negated ensureserverrunning", "! ensureserverrunning UP_PORT\n", false},
		{"negated ensureservernotrunning", "! ensureservernotrunning DOWN_PORT\n", false},
		{"ensureservernotrunning on a closed port", "ensureservernotrunning DOWN_PORT\n", true},
		{"ensureservernotrunning on a listening port", "ensureservernotrunning UP_PORT\n", false},
		{"negated ui without a server", "! ui '\"q\"'\n", true},
		{"ui without a server", "ui '\"q\"'\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pass, log := runCommandScript(t, tc.script, env)
			if pass != tc.pass {
				t.Fatalf("script passed = %v, want %v\n%s", pass, tc.pass, log)
			}
		})
	}
}

// stopserver starts with empty stdout and stderr, so a stderr check after it
// does not see the previous command's output.
func TestStopserverResetsOutput(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	env := map[string]string{
		"SOFT_SERVE_HTTP_PUBLIC_URL": srv.URL,
		"SSH_PORT":                   closedPort(t),
		"GIT_PORT":                   closedPort(t),
		"HTTP_PORT":                  closedPort(t),
		"STATS_PORT":                 closedPort(t),
	}
	pass, log := runCommandScript(t, "writestderr oops\nstderr oops\nstopserver\n! stderr .\n", env)
	if !pass {
		t.Fatalf("stderr after stopserver still held the previous command's output\n%s", log)
	}
}
