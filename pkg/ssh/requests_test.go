package ssh

import (
	"bufio"
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

// allStacks returns the stacks of every goroutine.
func allStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// goroutineBlocked reports whether one goroutine waits in state, as its
// stack header names it (such as "[sync.Cond.Wait"), with every one of
// frames in its stack.
func goroutineBlocked(state string, frames ...string) bool {
	for _, g := range strings.Split(allStacks(), "\n\n") {
		header, stack, _ := strings.Cut(g, "\n")
		if !strings.Contains(header, state) {
			continue
		}
		all := true
		for _, f := range frames {
			if !strings.Contains(stack, f) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// goroutinesWith returns how many goroutines have frame in their stack.
func goroutinesWith(frame string) int {
	n := 0
	for _, g := range strings.Split(allStacks(), "\n\n") {
		if strings.Contains(g, frame) {
			n++
		}
	}
	return n
}

// limitHandlersFrame is the stack frame of a session handler that
// limitHandlers runs, from the count of handlers up to its decrement.
const limitHandlersFrame = "github.com/charmbracelet/soft-serve/pkg/ssh.limitHandlers.func1("

// sessionLoopFrame is the stack frame of a charm.land/ssh session request
// loop.
const sessionLoopFrame = "charm.land/ssh.(*session).handleRequests("

// sessionLoops returns how many charm.land/ssh session request loops are
// running.
func sessionLoops() int {
	return strings.Count(allStacks(), sessionLoopFrame)
}

// blockedSessionLoops returns how many of the session request loops in
// stacks are blocked on a channel send, as a loop sending a window size
// nobody reads is. A loop that waits for its next request is not leaked: it
// ends once the server reads the client's close.
func blockedSessionLoops(stacks string) int {
	n := 0
	for _, g := range strings.Split(stacks, "\n\n") {
		if strings.Contains(g, sessionLoopFrame) && strings.Contains(g, "[chan send") {
			n++
		}
	}
	return n
}

// sessionLoopsWait bounds how long waitSessionLoops waits for the session
// request loops to end. A loop ends only once the server has read the
// client's close, and on a loaded machine whose loopback connections stall
// that has taken 15 seconds. A leaked loop never ends, so a long bound only
// slows down a run that fails.
const sessionLoopsWait = 60 * time.Second

// waitSessionLoops waits until no more than want session request loops run,
// polling until the count comes down. On a timeout it logs the stack of
// every goroutine, so the failure shows where the loops left over are.
func waitSessionLoops(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(sessionLoopsWait)
	for {
		stacks := allStacks()
		n := strings.Count(stacks, sessionLoopFrame)
		if n <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Logf("goroutines:\n%s", stacks)
			t.Fatalf("%d session request loops (%d blocked on a send) still run %v after every connection closed, want at most %d",
				n, blockedSessionLoops(stacks), sessionLoopsWait, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func windowPayload(width, height uint32) []byte {
	return gossh.Marshal(struct{ Width, Height, PixelWidth, PixelHeight uint32 }{width, height, 0, 0})
}

func envPayload(key, value string) []byte {
	return gossh.Marshal(struct{ Key, Value string }{key, value})
}

// sendRequest sends a channel request that wants a reply and returns the
// reply, failing the test if none comes within five seconds: a request loop
// blocked for ever never replies.
func sendRequest(t *testing.T, s *gossh.Session, name string, payload []byte) bool {
	t.Helper()
	type reply struct {
		ok  bool
		err error
	}
	done := make(chan reply, 1)
	go func() {
		ok, err := s.SendRequest(name, true, payload)
		done <- reply{ok, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("%s: %v", name, r.err)
		}
		return r.ok
	case <-time.After(sessionLoopsWait):
		t.Fatalf("%s: no reply: the session's request loop is blocked", name)
		return false
	}
}

// noAccessConfig returns the config of a server that gives a key with no
// account nothing, so the tests below show what such a key can still do.
func noAccessConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	noAccess := access.NoAccess
	cfg.AnonAccess = &noAccess
	keyless := false
	cfg.AllowKeyless = &keyless
	return cfg
}

// A window-change before any command is refused, rather than block the
// session's request loop for ever, which would keep the loop, the session
// and what the client wrote to it after the connection closed. Nothing is
// left once the client goes.
func TestWindowChangeBeforeCommandLeavesNothing(t *testing.T) {
	addr, _ := startTestServer(t, noAccessConfig(t))
	before := sessionLoops()

	for i := 0; i < 3; i++ {
		c, err := dialClient(t, addr)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 3; j++ {
			s, err := c.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
				t.Fatal(err)
			}
			// No reply wanted: a loop blocked for ever never sends one.
			for k := uint32(0); k < 2; k++ {
				if _, err := s.SendRequest("window-change", false, windowPayload(100+k, 50)); err != nil {
					t.Fatal(err)
				}
			}
			stdin, err := s.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			_, _ = stdin.Write(bytes.Repeat([]byte{'x'}, 64<<10))
		}
		_ = c.Close()
	}
	waitSessionLoops(t, before)

	// A client that wants a reply gets a refusal.
	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	if sendRequest(t, s, "window-change", windowPayload(120, 60)) {
		t.Error("window-change before a command accepted, want refused")
	}
}

// startPtyCommand opens a session on c with a pty and runs command on it,
// waiting for its first line of output so the command is known to run.
func startPtyCommand(t *testing.T, c *gossh.Client, command string) *gossh.Session {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(command); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("%s: %v", command, err)
	}
	return s
}

// A command run with a pty has no TUI to read its window changes: they are
// accepted and dropped instead of blocking the session's request loop.
func TestWindowChangeDuringPtyCommand(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, ctx := startTestServer(t, cfg)
	if _, err := depsOf(ctx).be.CreateRepository(ctx, "repo1", fixtureAdmin(t, depsOf(ctx).be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	before := sessionLoops()

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	// git-upload-pack waits for the client after listing its refs.
	s := startPtyCommand(t, c, "git-upload-pack repo1")
	for i := uint32(0); i < 5; i++ {
		if !sendRequest(t, s, "window-change", windowPayload(100+i, 50)) {
			t.Fatal("window-change during a pty command refused")
		}
	}
	_ = c.Close()
	waitSessionLoops(t, before)
}

// The TUI still gets every window change.
func TestWindowChangeDuringTUI(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, _ := startTestServer(t, cfg)
	before := sessionLoops()

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	// Wait for the TUI to draw, so it runs. charm.land/ssh writes the
	// window size without a lock, and the TUI reads it when it starts.
	if _, err := stdout.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < 5; i++ {
		if !sendRequest(t, s, "window-change", windowPayload(100+i, 50)) {
			t.Fatal("window-change in the TUI refused")
		}
	}
	_ = c.Close()
	waitSessionLoops(t, before)
}

// execPayload is the payload of an exec request for command.
func execPayload(command string) []byte {
	return gossh.Marshal(struct{ Value string }{command})
}

// skipUnderRace skips a test that sends window changes while the session's
// handler starts. charm.land/ssh writes the window size without a lock and
// Session.Pty copies it, so the race detector reports the library's race,
// which these tests cannot avoid: they need the window changes to arrive
// before the handler returns.
func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("charm.land/ssh races on the window size of a resize sent while the handler starts")
	}
}

// ptyExecThenResize requests a pty on s, then sends an exec request for
// command and three window changes without waiting for any reply, as a
// client that means harm would.
func ptyExecThenResize(t *testing.T, s *gossh.Session, command string) {
	t.Helper()
	if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendRequest("exec", false, execPayload(command)); err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < 3; i++ {
		// The server may have closed the channel already.
		_, _ = s.SendRequest("window-change", false, windowPayload(100+i, 50))
	}
}

// A session that a middleware ends before any command runs, here for an
// overlong command, has no reader of its window changes: they are dropped
// once its handler has returned, instead of blocking the request loop for
// ever.
func TestWindowChangeAfterRefusedCommandLeavesNothing(t *testing.T) {
	skipUnderRace(t)
	addr, _ := startTestServer(t, noAccessConfig(t))
	before := sessionLoops()
	long := strings.Repeat("x", 64<<10)
	for i := 0; i < 3; i++ {
		c, err := dialClient(t, addr)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		ptyExecThenResize(t, s, long)
		_ = c.Close()
	}
	waitSessionLoops(t, before)
}

// A pty command refused by max_sessions also drops its window changes.
func TestWindowChangeAfterSessionLimitLeavesNothing(t *testing.T) {
	skipUnderRace(t)
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	addr, _ := startTestServer(t, cfg, withMaxSessions(1))
	before := sessionLoops()
	for i := 0; i < 3; i++ {
		c, err := dialClient(t, addr)
		if err != nil {
			t.Fatal(err)
		}
		// A TUI keeps its place under max_sessions after its channel
		// closes, until the connection ends.
		tui, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err := tui.RequestPty("xterm", 40, 80, nil); err != nil {
			t.Fatal(err)
		}
		stdout, err := tui.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := tui.Shell(); err != nil {
			t.Fatal(err)
		}
		if _, err := stdout.Read(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		_ = tui.Close()
		var s *gossh.Session
		waitFor(t, "the closed channel's place", func() bool {
			s, err = c.NewSession()
			return err == nil
		})
		ptyExecThenResize(t, s, "repo list")
		_ = c.Close()
	}
	waitSessionLoops(t, before)
}

// Only one short GIT_PROTOCOL env request is accepted; the rest are refused
// and not kept until the connection closes.
func TestEnvRequestsNotStored(t *testing.T) {
	addr, _ := startTestServer(t, noAccessConfig(t))
	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}

	if !sendRequest(t, s, "env", envPayload("GIT_PROTOCOL", "version=2")) {
		t.Error("GIT_PROTOCOL refused")
	}

	const n, size = 200, 200 << 10
	value := strings.Repeat("x", size)
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	heapBefore := m.HeapAlloc
	accepted := 0
	for i := 0; i < n; i++ {
		if sendRequest(t, s, "env", envPayload(fmt.Sprintf("VAR%d", i), value)) {
			accepted++
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&m)
	// The session is still open, so whatever it stored is still live.
	if grown := int64(m.HeapAlloc) - int64(heapBefore); grown > n*size/4 {
		t.Errorf("heap grew by %d MiB over %d MiB of env requests", grown>>20, n*size>>20)
	}
	if accepted > 0 {
		t.Errorf("%d of %d env requests accepted, want none", accepted, n)
	}
}

// startFilterServer starts a bare SSH server whose session channels pass
// through filterSessionRequests, with an emulated pty, running handler.
func startFilterServer(t *testing.T, handler ssh.Handler) string {
	t.Helper()
	srv := &ssh.Server{
		Handler:          handler,
		PublicKeyHandler: func(ssh.Context, ssh.PublicKey) bool { return true },
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session": filterSessionRequests(ssh.DefaultSessionHandler),
		},
	}
	if err := ssh.EmulatePty()(srv); err != nil {
		t.Fatal(err)
	}
	srv.AddHostKey(newSigner(t))
	return testsession.Listen(t, srv)
}

// The session's environment holds only the first short GIT_PROTOCOL.
func TestFilterEnvRequests(t *testing.T) {
	addr := startFilterServer(t, func(s ssh.Session) {
		_, _ = fmt.Fprint(s, strings.Join(s.Environ(), "\n"))
		_ = s.Exit(0)
	})
	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	t.Run("accepted once", func(t *testing.T) {
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			key, value string
			want       bool
		}{
			{"LANG", "C.UTF-8", false},
			{"GIT_PROTOCOL", "version=2", true},
			{"GIT_PROTOCOL", "version=1", false},
			{"git_protocol", "version=1", false},
		} {
			if got := sendRequest(t, s, "env", envPayload(tc.key, tc.value)); got != tc.want {
				t.Errorf("env %s=%s: accepted %t, want %t", tc.key, tc.value, got, tc.want)
			}
		}
		if _, err := s.SendRequest("env", true, []byte("garbage")); err != nil {
			t.Fatal(err)
		}
		out, err := s.Output("env")
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != "GIT_PROTOCOL=version=2" {
			t.Errorf("environment %q, want only GIT_PROTOCOL=version=2", out)
		}
	})

	t.Run("too long", func(t *testing.T) {
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if sendRequest(t, s, "env", envPayload("GIT_PROTOCOL", strings.Repeat("v", maxGitProtocolLength+1))) {
			t.Error("overlong GIT_PROTOCOL accepted")
		}
		if !sendRequest(t, s, "env", envPayload("GIT_PROTOCOL", strings.Repeat("v", maxGitProtocolLength))) {
			t.Error("GIT_PROTOCOL of the longest length refused")
		}
		out, err := s.Output("env")
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != "GIT_PROTOCOL="+strings.Repeat("v", maxGitProtocolLength) {
			t.Errorf("environment %q", out)
		}
	})
}

// Window changes before a command are refused; after a shell starts, the
// handler gets them, and a second pty-req is refused.
//
// The handler reads window changes until the session ends, rather than
// exiting once it has the size it waits for: charm.land/ssh sends a size to
// the handler before it replies to the request, so a handler that exited on
// receipt could close the channel before that reply, and the client's
// want-reply request would end with EOF. The client closes the session only
// once it has the reply.
func TestFilterWindowChanges(t *testing.T) {
	sizes := make(chan ssh.Window, 10)
	done := make(chan struct{})
	addr := startFilterServer(t, func(s ssh.Session) {
		defer close(done)
		_, winch, ok := s.Pty()
		if !ok {
			_ = s.Exit(1)
			return
		}
		_, _ = fmt.Fprintln(s, "ready")
		// The request loop closes winch when the session ends.
		for w := range winch {
			sizes <- w
		}
	})
	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if sendRequest(t, s, "window-change", windowPayload(90, 30)) {
		t.Error("window-change before pty-req accepted")
	}
	if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	if sendRequest(t, s, "window-change", windowPayload(100, 30)) {
		t.Error("window-change before shell accepted")
	}
	if err := s.RequestPty("xterm", 40, 80, nil); err == nil {
		t.Error("second pty-req accepted")
	}
	readyAfterShell(t, s)()
	if !sendRequest(t, s, "window-change", windowPayload(200, 60)) {
		t.Error("window-change after shell refused")
	}
	// End the session; the handler's loop ends with it.
	_ = s.Close()
	select {
	case <-done:
	case <-time.After(sessionLoopsWait):
		t.Fatal("handler still running after the session closed")
	}

	var got []ssh.Window
	for len(sizes) > 0 {
		got = append(got, <-sizes)
	}
	if len(got) != 2 || got[0].Width != 80 || got[0].Height != 40 || got[1].Width != 200 || got[1].Height != 60 {
		t.Errorf("handler got windows %+v, want 80x40 then 200x60", got)
	}
}

// startForwardServer starts a bare SSH server wired like the real one: an
// emulated pty, session channels through filterSessionRequests, and handler
// behind forwardWindowChanges.
func startForwardServer(t *testing.T, handler ssh.Handler) string {
	t.Helper()
	srv := &ssh.Server{
		Handler:          forwardWindowChanges(handler),
		PublicKeyHandler: func(ssh.Context, ssh.PublicKey) bool { return true },
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session": filterSessionRequests(ssh.DefaultSessionHandler),
		},
	}
	if err := ssh.EmulatePty()(srv); err != nil {
		t.Fatal(err)
	}
	srv.AddHostKey(newSigner(t))
	return testsession.Listen(t, srv)
}

// A handler that blocks without draining its window changes -- stuck on a
// write to a client that is not reading (the length or auth check, the
// session-limit refusal, the "no access" message) or inside a TUI whose
// renderer the client has stalled -- must not deadlock the session's
// request loop. Without a drain, the loop fills the one-element window
// channel and then blocks on the next window-change, so it stops answering
// requests; x/crypto's request buffer fills and the connection read loop
// blocks behind it, and the server never sees another request, the
// disconnect or a timeout.
//
// forwardWindowChanges drains the window changes whatever the handler does,
// so the loop keeps answering. This sends window-changes that want a reply
// while the handler is blocked and checks every one is answered; without the
// drain the second never is (the loop is stuck sending the first to a
// channel nobody reads). A blocked handler stands in for a zero-window
// client, whose first byte written blocks the same way.
func TestForwardWindowChangesKeepsLoopAnswering(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	addr := startForwardServer(t, func(s ssh.Session) {
		_, _, _ = s.Pty()
		close(ready)
		<-release // block without reading the window changes
	})
	defer close(release)

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendRequest("exec", true, execPayload("x")); err != nil {
		t.Fatal(err)
	}
	<-ready
	// sendRequest fails the test if a reply does not come within five
	// seconds, which is what a blocked request loop would do.
	for i := uint32(0); i < 32; i++ {
		if !sendRequest(t, s, "window-change", windowPayload(100+i, 50)) {
			t.Fatalf("window-change %d refused", i)
		}
	}
}

// forwardWindowChanges gives the handler the newest window size and never
// blocks the request loop. The handler reads its pty first and prints
// "ready"; the client sends its resizes only after, so no resize races with
// the one read of the library's pty (charm.land/ssh writes the window
// without a lock), and this test needs no race skip.
func TestForwardWindowChangesLatest(t *testing.T) {
	type result struct {
		w  ssh.Window
		ok bool
	}
	got := make(chan result, 1)
	addr := startForwardServer(t, func(s ssh.Session) {
		_, winch, ok := s.Pty()
		_, _ = fmt.Fprintln(s, "ready")
		// Wait for the client's go-ahead byte, sent after its resizes, so
		// the library has already processed them all.
		_, _ = bufio.NewReader(s).ReadByte()
		// Read until the newest size arrives. Latest-wins may collapse the
		// ones in between, but the final one always comes through.
		var last ssh.Window
		for w := range winch {
			last = w
			if w.Width == 163 && w.Height == 113 {
				break
			}
		}
		got <- result{last, ok}
		_ = s.Exit(0)
	})

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := s.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < 64; i++ {
		if !sendRequest(t, s, "window-change", windowPayload(100+i, 50+i)) {
			t.Fatalf("window-change %d refused", i)
		}
	}
	if _, err := stdin.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-got:
		if !r.ok {
			t.Fatal("handler had no pty")
		}
		// Latest-wins keeps the newest; it may miss ones in between.
		if r.w.Width != 163 || r.w.Height != 113 {
			t.Errorf("handler saw %dx%d, want the newest 163x113", r.w.Width, r.w.Height)
		}
	case <-time.After(sessionLoopsWait):
		t.Fatal("handler did not report the window")
	}
}

// readyAfterShell starts a shell on s and returns a func that waits for the
// handler's "ready" line. The handlers print it once they have read the pty:
// charm.land/ssh writes the window size without a lock on every window
// change, so the tests send none before.
func readyAfterShell(t *testing.T, s *gossh.Session) func() {
	t.Helper()
	stdout, err := s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
}
