package testscript

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/internal/testnet"
	"golang.org/x/crypto/ssh"
)

// fakeGit returns the environment of a script whose git is a shell script
// with body, for the commands runCommandScript runs.
func fakeGit(t *testing.T, body string) map[string]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake git is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { // an executable
		t.Fatal(err)
	}
	return map[string]string{
		"PATH":                  dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SSH_KNOWN_CONFIG_FILE": filepath.Join(t.TempDir(), "config"),
		"SSH_KNOWN_HOSTS_FILE":  filepath.Join(t.TempDir(), "known_hosts"),
	}
}

// stuckSSHServer starts an SSH server that accepts any client and every
// pty, shell and exec request, and then never answers, and returns its
// port.
func stuckSSHServer(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(hostKey)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(loopback, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, err := nc.Accept()
					if err != nil {
						continue
					}
					t.Cleanup(func() { _ = ch.Close() })
					go func() {
						for r := range creqs {
							_ = r.Reply(r.Type == "exec" || r.Type == "shell" || r.Type == "pty-req", nil)
						}
					}()
				}
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// A git, soft or ui command that does not finish within commandTimeout is
// stopped, and fails its script with stuckMarker, under "!" too.
func TestCommandTimeout(t *testing.T) {
	// Not parallel: it lowers commandTimeout, which every command reads.
	// Parallel tests start only once it is done.
	saved := commandTimeout
	commandTimeout = 3 * time.Second
	t.Cleanup(func() { commandTimeout = saved })
	// hang bounds each script, so that a bound that does not stop its
	// command fails the case that shows it rather than the whole test
	// binary. It is far past commandTimeout and only catches a hang. It
	// is also well past uiWaitTimeout, which ends a ui -wait on its own:
	// a ui command that the bound did not stop must fail as stuck here,
	// whichever of the two would otherwise fire first.
	hang := max(30*commandTimeout, uiWaitTimeout+time.Minute)

	var gitEnv map[string]string
	if runtime.GOOS != "windows" {
		gitEnv = fakeGit(t, "exec sleep 600")
	}
	sshEnv := map[string]string{"SSH_PORT": stuckSSHServer(t)}
	for _, tc := range []struct {
		name, script string
		env          map[string]string
	}{
		{"git", "git status\n", gitEnv},
		{"negated git", "! git status\n", gitEnv},
		{"soft", "soft repo list\n", sshEnv},
		{"negated soft", "! soft repo list\n", sshEnv},
		{"ui", "ui -wait never '\"q\"'\n", sshEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == nil {
				t.Skip("the fake git is a shell script")
			}
			type result struct {
				pass     bool
				log      string
				panicked any
			}
			run := prepareCommandScript(t, tc.script, tc.env)
			done := make(chan result, 1)
			go func() {
				pass, log, panicked := run()
				done <- result{pass, log, panicked}
			}()
			var r result
			select {
			case r = <-done:
			case <-time.After(hang):
				t.Fatalf("%s: the script still runs %v after it started; the bound did not stop its command", tc.name, hang)
			}
			if r.panicked != nil {
				t.Fatalf("script panicked: %v\n%s", r.panicked, r.log)
			}
			if r.pass || !strings.Contains(r.log, stuckMarker) {
				t.Fatalf("script passed = %v, want a failure with %q\n%s", r.pass, stuckMarker, r.log)
			}
			if tc.env["PATH"] != "" && !strings.Contains(r.log, boundedTimeoutMarker) {
				t.Fatalf("the processes of the stuck git are not listed\n%s", r.log)
			}
		})
	}
}

// A git command whose connect to the server failed is run again, up to
// connectAttempts times in all; any other failure is not.
func TestGitConnectRetry(t *testing.T) {
	t.Parallel()
	const refused = `echo "fatal: unable to access 'http://127.0.0.1:1/r/': Failed to connect to 127.0.0.1 port 1 after 7774 ms: Couldn't connect to server" >&2; exit 128`
	const daemonRefused = `printf 'fatal: unable to connect to 127.0.0.1:\n127.0.0.1[0: 127.0.0.1]: errno=Operation timed out\n' >&2; exit 128`
	for _, tc := range []struct {
		name, body, script string
		pass               bool
		runs               int
	}{
		{"http connect fails twice", `if [ "$(wc -l < "$RUNS")" -lt 3 ]; then ` + refused + `; fi; echo cloned`, "git clone x\nstdout cloned\n", true, connectAttempts},
		{"daemon connect fails twice", `if [ "$(wc -l < "$RUNS")" -lt 3 ]; then ` + daemonRefused + `; fi; echo cloned`, "git clone x\nstdout cloned\n", true, connectAttempts},
		{"connect always fails", refused, "! git clone x\n", true, connectAttempts},
		{"other failure", `echo "fatal: repository not found" >&2; exit 128`, "! git clone x\n", true, 1},
		// Only git's own fatal line counts.
		{"server's message", `echo "remote: fatal: unable to connect to 127.0.0.1:" >&2; exit 128`, "! git clone x\n", true, 1},
		{"server's message quoting curl", `echo "remote: fatal: unable to access 'http://127.0.0.1:1/r/': Failed to connect to 127.0.0.1 port 1" >&2; exit 128`, "! git clone x\n", true, 1},
		{"later error quoting curl", `echo "error: RPC failed; curl 7 Failed to connect to 127.0.0.1 port 1 after 0 ms: Couldn't connect to server" >&2; exit 128`, "! git clone x\n", true, 1},
		{"another daemon", `echo "fatal: unable to connect to cache daemon: Connection refused" >&2; exit 128`, "! git clone x\n", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := filepath.Join(t.TempDir(), "runs")
			if err := os.WriteFile(runs, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			// Each run first counts itself, then does what body says.
			env := fakeGit(t, `echo run >> "$RUNS"; `+tc.body)
			env["RUNS"] = runs
			pass, log := runCommandScript(t, tc.script, env)
			if pass != tc.pass {
				t.Fatalf("script passed = %v, want %v\n%s", pass, tc.pass, log)
			}
			data, err := os.ReadFile(runs)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(data), "run"); got != tc.runs {
				t.Fatalf("git ran %d times, want %d\n%s", got, tc.runs, log)
			}
		})
	}
}

// connecting matches the line "ssh -v" prints for each connect attempt,
// "debug1: Connecting to 127.0.0.1 [127.0.0.1] port N.", the same in
// OpenSSH on macOS, Linux and Windows.
var connecting = regexp.MustCompile(`(?m)^debug1: Connecting to 127\.0\.0\.1 `)

// refusedAtBanner matches the "ssh -v" output of a connect that ssh took
// for established although it was refused, so that only the banner
// exchange failed: OpenSSH for Windows reports a refused loopback connect
// that way.
var refusedAtBanner = regexp.MustCompile(`(?s)debug1: Connection established\..*kex_exchange_identification: .*Connection refused`)

// git's ssh dials localhost at 127.0.0.1, bounds the connect and the
// banner exchange, and tries a refused connect three times (see
// sshConfig). The hang this prevents is in macOS's ssh; the retry covers
// a loopback connect that fails under load.
//
// "ssh -G" checks the settings themselves, with any OpenSSH. A refused
// connect checks that ssh retries it, where it does: OpenSSH for Windows
// reports a refused loopback connect as established and fails at the
// banner exchange, which ConnectionAttempts does not retry, so the count
// is skipped there.
func TestSSHConfigRetriesConnect(t *testing.T) {
	t.Parallel()
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skipf("no ssh to run: %v", err)
	}
	config := filepath.Join(t.TempDir(), "config")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(config, []byte(fmt.Sprintf(sshConfig, knownHosts)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	settings, err := exec.CommandContext(ctx, sshPath, "-G", "-F", filepath.ToSlash(config), "localhost").Output()
	if err != nil {
		t.Fatalf("ssh -G: %v\n%s", err, settings)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(string(settings), "\r\n", "\n"), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	for _, want := range []string{"hostname 127.0.0.1", "connectionattempts 3", "connecttimeout 120"} {
		if !have[want] {
			t.Errorf("ssh -G does not report %q\n%s", want, settings)
		}
	}

	out, err := exec.CommandContext(ctx, sshPath, "-v", "-F", filepath.ToSlash(config),
		"-o", "BatchMode=yes", "-p", refusedPort(), "localhost", "true").CombinedOutput()
	if err == nil {
		t.Fatalf("ssh to a closed port succeeded\n%s", out)
	}
	if refusedAtBanner.Match(out) {
		t.Logf("not counting connect attempts: this ssh took the refused connect for established and failed at the banner exchange, which it does not retry\n%s", out)
		return
	}
	if got := len(connecting.FindAll(out, -1)); got != 3 {
		t.Fatalf("ssh made %d connect attempts to 127.0.0.1, want 3\n%s", got, out)
	}
}

// refusedPort returns a port on 127.0.0.1 where nothing listens, for the
// life of the test binary: a testnet.RandomPort, reserved for this process
// and below every OS's ephemeral range, so neither a ":0" listener nor
// another test's RandomPort is given it. A connect to it is refused at
// once. A socket that is bound but not listening would not do: macOS drops
// the SYNs, and the connect times out after about 7.8 s instead.
func refusedPort() string {
	return strconv.Itoa(testnet.RandomPort())
}

// refusedAtBanner tells OpenSSH for Windows' report of a refused connect
// from the one OpenSSH gives elsewhere, which ssh retries.
func TestRefusedAtBanner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, out string
		want      bool
	}{
		{"OpenSSH for Windows", "OpenSSH_for_Windows_9.5p2, LibreSSL 3.8.2\r\n" +
			"debug1: Connecting to 127.0.0.1 [127.0.0.1] port 23363.\r\n" +
			"debug1: fd 3 clearing O_NONBLOCK\r\n" +
			"debug1: Connection established.\r\n" +
			"debug1: getpeername failed: The socket is not connected\r\n" +
			"debug1: kex_exchange_identification: write: Connection refused\r\n" +
			"banner exchange: Connection to UNKNOWN port -1: Connection refused\r\n", true},
		{"OpenSSH on macOS", "debug1: Connecting to 127.0.0.1 [127.0.0.1] port 1.\n" +
			"debug1: connect to address 127.0.0.1 port 1: Connection refused\n" +
			"debug1: Connecting to 127.0.0.1 [127.0.0.1] port 1.\n" +
			"debug1: connect to address 127.0.0.1 port 1: Connection refused\n" +
			"ssh: connect to host 127.0.0.1 port 1: Connection refused\n", false},
	} {
		if got := refusedAtBanner.MatchString(tc.out); got != tc.want {
			t.Errorf("%s: refusedAtBanner = %v, want %v", tc.name, got, tc.want)
		}
	}
}
