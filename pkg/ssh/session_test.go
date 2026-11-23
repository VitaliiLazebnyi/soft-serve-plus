package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/matryer/is"
	dto "github.com/prometheus/client_model/go"
	gossh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite" // sqlite driver
)

func TestSession(t *testing.T) {
	is := is.New(t)
	t.Run("authorized repo access", func(t *testing.T) {
		t.Log("setting up")
		s := setup(t)
		s.Stderr = os.Stderr
		t.Log("requesting pty")
		err := s.RequestPty("xterm", 80, 40, nil)
		is.NoErr(err)
		// Close the session once the TUI has drawn, so the command is
		// known to run: a close before the exec request fails the request
		// instead of ending the command.
		out := &firstWrite{written: make(chan struct{})}
		s.Stdout = out
		is.NoErr(s.Start("test"))
		select {
		case <-out.written:
		case <-time.After(time.Minute):
			t.Fatal("the TUI wrote nothing within a minute")
		}
		s.Close() //nolint: errcheck
		t.Log("waiting for session to exit")
		err = s.Wait()
		var ee *gossh.ExitMissingError
		is.True(errors.As(err, &ee))
		t.Log("session exited")
	})
}

// firstWrite discards what is written to it and closes written on the
// first write.
type firstWrite struct {
	once    sync.Once
	written chan struct{}
}

func (w *firstWrite) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.written) })
	return len(p), nil
}

func setup(tb testing.TB) *gossh.Session {
	tb.Helper()
	return testsession.New(tb, setupServer(tb), nil)
}

// setupServer returns an SSH server running the TUI middleware. Its database
// is closed when the test ends, after the server: see closeTestDB.
func setupServer(tb testing.TB) *ssh.Server {
	tb.Helper()
	return setupServerWith(tb, func(hs *Server) func(ssh.Handler) ssh.Handler { return hs.bubbleTeaMiddleware })
}

// setupServerWith is setupServer with tui as the TUI middleware.
func setupServerWith(tb testing.TB, tui func(*Server) func(ssh.Handler) ssh.Handler) *ssh.Server {
	tb.Helper()
	ctx := context.TODO()
	cfg := config.DefaultConfig()
	cfg.DataPath = tb.TempDir()
	if err := cfg.Validate(); err != nil {
		tb.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	closeTestDB(tb, dbx)
	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		tb.Fatal(err)
	}
	dbstore := database.New(nil)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(cfg, dbx, dbstore, log.FromContext(ctx))
	hs := handlerServer(tb, cfg, dbx, dbstore, be, log.Default())
	return &ssh.Server{
		Handler: hs.loggerMiddleware(tui(hs)(func(s ssh.Session) {
			_, _, active := s.Pty()
			if !active {
				tb.Error("session has no pty")
				_ = s.Exit(1)
				return
			}
			_ = s.Exit(0)
		})),
	}
}

// TestBubbleTeaMiddlewareClosesZoneManager checks that a TUI session that
// ends without the quit key, here by the client dropping the connection,
// still stops its zone manager's worker goroutine.
//
// Workers are tracked by goroutine ID, so a worker left by an earlier test
// that is still shutting down cannot be mistaken for this session's.
func TestBubbleTeaMiddlewareClosesZoneManager(t *testing.T) {
	before := zoneWorkers()
	srv := setupServer(t)
	addr := testsession.Listen(t, srv)

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	cc, chans, reqs, err := gossh.NewClientConn(conn, addr, &gossh.ClientConfig{
		User:            "testuser",
		Auth:            []gossh.AuthMethod{gossh.Password("testpass")},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec
	})
	if err != nil {
		t.Fatal(err)
	}
	client := gossh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { _ = client.Close() })
	s, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 80, 40, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("test"); err != nil {
		t.Fatal(err)
	}

	var ours map[string]bool
	waitZoneWorkers(t, "the TUI to start", func(running map[string]bool) bool {
		ours = map[string]bool{}
		for id := range running {
			if !before[id] {
				ours[id] = true
			}
		}
		return len(ours) > 0
	})
	_ = client.Close()
	waitZoneWorkers(t, "the session's zone manager to stop", func(running map[string]bool) bool {
		for id := range ours {
			if running[id] {
				return false
			}
		}
		return true
	})
}

// waitZoneWorkers waits until ok returns true for the set of running
// bubblezone worker goroutine IDs.
func waitZoneWorkers(t *testing.T, what string, ok func(map[string]bool) bool) {
	t.Helper()
	deadline := time.Now().Add(sessionLoopsWait)
	for {
		running := zoneWorkers()
		if ok(running) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %d zone manager workers running", what, len(running))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// zoneWorkers returns the IDs of the running bubblezone worker goroutines.
func zoneWorkers() map[string]bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	ids := map[string]bool{}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "bubblezone/v2.(*Manager).zoneWorker") {
			continue
		}
		// The first line is "goroutine <id> [<state>]:".
		if f := strings.Fields(g); len(f) > 1 && f[0] == "goroutine" {
			ids[f[1]] = true
		}
	}
	return ids
}

// The time spent in the TUI is counted when the TUI session ends, not when
// the connection it ran on closes.
func TestTUISessionDurationCountedAtSessionEnd(t *testing.T) {
	srv := setupServer(t)
	addr := testsession.Listen(t, srv)

	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	before := tuiSeconds(t)
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	// Quit the TUI, retrying until the program reads the key.
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(sessionLoopsWait)
wait:
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("session: %v", err)
			}
			break wait
		case <-tick.C:
			_, _ = io.WriteString(stdin, "q")
		case <-timeout:
			t.Fatal("TUI did not quit")
		}
	}

	// The connection is still open.
	if after := tuiSeconds(t); after <= before {
		t.Fatalf("tui_session_seconds_total = %v after the session ended, want more than %v", after, before)
	}
}

func tuiSeconds(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := tuiSessionDuration.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// heapAlloc returns the live heap after a garbage collection.
func heapAlloc() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A TUI renders at most 500x200 cells, whatever size the client asks for.
// Bubble Tea allocates its screen at the size it is given, about 250 bytes a
// cell: at the initial size, which it does not check, and again on every
// resize it lets through (up to 4096 columns or rows and 1<<20 cells), as
// the renderer resizes its screen and cell buffer on the next frame. So a
// 5000x300 pty used to hold over 300 MiB, a 4000x250 window-change over
// 200 MiB, and a 30000x30000 pty would have exhausted memory.
// newSessionProgram clamps the initial size and sessionFilter the resizes.
func TestTUIWindowSizeClamped(t *testing.T) {
	// The clamped 500x200 costs about 25 MiB; the sizes below, unclamped,
	// over 200 MiB.
	const limit = 100 << 20
	for _, tc := range []struct {
		name          string
		width, height int
		resize        []byte
	}{
		{name: "pty-req", width: 5000, height: 300},
		{name: "window-change", width: 80, height: 40, resize: windowPayload(4000, 250)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.DataPath = t.TempDir()
			addr, _ := startTestServer(t, cfg)
			before := heapAlloc()
			check := func() {
				t.Helper()
				grown := int64(heapAlloc()) - int64(before)
				t.Logf("heap grew by %d MiB", grown>>20)
				if grown > limit {
					t.Fatalf("heap grew by %d MiB with the TUI open, want at most %d MiB", grown>>20, limit>>20)
				}
			}

			loops := sessionLoops()
			c, err := dialClient(t, addr)
			if err != nil {
				t.Fatal(err)
			}
			// On the way out, close the connection and wait for the
			// server's session to end and its memory to be released, so the
			// next subtest, or the next run with -count, starts from a heap
			// that no longer holds this TUI.
			defer func() {
				_ = c.Close()
				if t.Failed() {
					return
				}
				waitSessionLoops(t, loops)
				waitFor(t, "the TUI's memory to be released", func() bool {
					return int64(heapAlloc())-int64(before) < 16<<20
				})
			}()
			s, err := c.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RequestPty("xterm", tc.height, tc.width, nil); err != nil {
				t.Fatal(err)
			}
			stdout, err := s.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Shell(); err != nil {
				t.Fatal(err)
			}
			// Read the output in the background, so the TUI never blocks on
			// it.
			var mu sync.Mutex
			var out []byte
			go func() {
				buf := make([]byte, 4096)
				for {
					n, rerr := stdout.Read(buf)
					mu.Lock()
					out = append(out, buf[:n]...)
					mu.Unlock()
					if rerr != nil {
						return
					}
				}
			}()
			// drawn reports whether the output after its first mark
			// bytes holds text; written returns how long it is.
			drawn := func(text string, mark int) func() bool {
				return func() bool {
					mu.Lock()
					defer mu.Unlock()
					return bytes.Contains(out[mark:], []byte(text))
				}
			}
			written := func() int {
				mu.Lock()
				defer mu.Unlock()
				return len(out)
			}
			// The renderer sizes its screen and cell buffer for a frame
			// before it writes the frame, so once a frame at the size
			// under test is out, its memory is taken. The help line ends
			// with this only when the screen is wider than the 80
			// columns of the window-change case before its resize.
			const wide = "toggle help"
			if tc.resize == nil {
				waitFor(t, "the TUI to draw its first frame", drawn(wide, 0))
				check()
				return
			}
			waitFor(t, "the TUI to draw its first frame", drawn("Repositories", 0))
			if drawn(wide, 0)() {
				t.Fatalf("the help line ends with %q at 80 columns too: it shows no longer which size a frame was drawn for", wide)
			}
			check()

			mark := written()
			if ok, err := s.SendRequest("window-change", true, tc.resize); err != nil || !ok {
				t.Fatalf("window-change: %t, %v", ok, err)
			}
			// The reply comes from the request loop, before the TUI has
			// handled the resize: wait for the frame at the new size,
			// among what the TUI wrote after the request went out.
			waitFor(t, "the TUI to draw at the new size", drawn(wide, mark))
			check()
		})
	}
}

// The UI's context ends when the TUI program exits, through the cancel
// newSessionProgram returns, and not only with the connection: a client
// that keeps the connection open, as for its next session, does not keep
// the UI's git commands running (UI-11, R57).
func TestNewSessionProgramContextEndsWithProgram(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	d := depsOf(serverContext(t, cfg, log.Default()))
	hs := handlerServer(t, cfg, d.dbx, d.st, d.be, log.Default())
	srv := &ssh.Server{
		Handler: hs.loggerMiddleware(func(s ssh.Session) {
			p, ui, cancel := hs.newSessionProgram(s)
			if p == nil {
				_ = s.Exit(1)
				return
			}
			defer ui.common.Zone.Close()
			before := ui.common.Context().Err()
			cancel()
			_, _ = fmt.Fprintf(s.Stderr(), "before=%v after=%v conn=%v", before, ui.common.Context().Err(), s.Context().Err())
			_ = s.Exit(0)
		}),
	}
	if err := ssh.EmulatePty()(srv); err != nil {
		t.Fatal(err)
	}
	sess := testsession.New(t, srv, &gossh.ClientConfig{User: "test"})
	if err := sess.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run(""); err != nil {
		t.Fatal(err)
	}
	if got, want := stderr.String(), "before=<nil> after=context canceled conn=<nil>"; got != want {
		t.Errorf("UI context: %s, want %s", got, want)
	}
}

// newSessionProgram gives the UI the clamped size.
func TestNewSessionProgramClampsSize(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := serverContext(t, cfg, log.Default())
	hs := handlerServer(t, cfg, depsOf(ctx).dbx, depsOf(ctx).st, depsOf(ctx).be, log.Default())
	srv := &ssh.Server{
		Handler: hs.loggerMiddleware(func(s ssh.Session) {
			p, ui, cancel := hs.newSessionProgram(s)
			if p == nil {
				_ = s.Exit(1)
				return
			}
			cancel()
			ui.common.Zone.Close()
			_, _ = fmt.Fprintf(s.Stderr(), "%dx%d", ui.width, ui.height)
			_ = s.Exit(0)
		}),
	}
	if err := ssh.EmulatePty()(srv); err != nil {
		t.Fatal(err)
	}
	sess := testsession.New(t, srv, &gossh.ClientConfig{User: "test"})
	if err := sess.RequestPty("xterm", 30000, 30000, nil); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run(""); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); got != "500x200" {
		t.Errorf("UI size %s, want 500x200", got)
	}
}

// TestSessionFilter checks the TUI's message filter: resizes are clamped,
// and a suspend resumes, as with the filter of bm.MakeOptions it replaces.
func TestSessionFilter(t *testing.T) {
	for _, tc := range []struct {
		in, want tea.WindowSizeMsg
	}{
		{tea.WindowSizeMsg{Width: 4000, Height: 250}, tea.WindowSizeMsg{Width: common.MaxWidth, Height: common.MaxHeight}},
		{tea.WindowSizeMsg{Width: 120, Height: 250}, tea.WindowSizeMsg{Width: 120, Height: common.MaxHeight}},
		{tea.WindowSizeMsg{Width: 80, Height: 24}, tea.WindowSizeMsg{Width: 80, Height: 24}},
		{tea.WindowSizeMsg{Width: -1, Height: -1}, tea.WindowSizeMsg{Width: 0, Height: 0}},
	} {
		if got := sessionFilter(nil, tc.in); got != tc.want {
			t.Errorf("sessionFilter(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	if got := sessionFilter(nil, tea.SuspendMsg{}); got != (tea.ResumeMsg{}) {
		t.Errorf("sessionFilter(SuspendMsg) = %#v, want ResumeMsg", got)
	}
	if got := sessionFilter(nil, tea.QuitMsg{}); got != (tea.QuitMsg{}) {
		t.Errorf("sessionFilter(QuitMsg) = %#v, want it unchanged", got)
	}
}

// The TUI's git work belongs to the session, not to the connection: once the
// program has exited, its context is done although the client keeps the
// connection open, as a multiplexing client does (UI-11, R57).
//
// The middleware cancels the UI's context when the program exits, before it
// hands the session on, and not only in its deferred cleanup: the next
// handler, which runs after the program and before that cleanup, already
// sees the UI's context done. The context checked is the one the built UI
// holds, and the test requires that the UI was built, so a TUI that fails
// to start does not pass it.
func TestTUIContextEndsWithSession(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	d := depsOf(serverContext(t, cfg, log.Default()))
	hs := handlerServer(t, cfg, d.dbx, d.st, d.be, log.Default())
	// built gets the UI newSessionProgram returns, nil when it built none;
	// uiCtx gets the built UI's context, for the next handler.
	built := make(chan *UI, 1)
	uiCtx := make(chan context.Context, 1)
	type errs struct{ ui, conn error }
	after := make(chan errs, 1)
	srv := &ssh.Server{
		Handler: hs.loggerMiddleware(tuiMiddleware(func(s ssh.Session) (*tea.Program, *UI, context.CancelFunc) {
			p, ui, cancel := hs.newSessionProgram(s)
			if ui != nil {
				uiCtx <- ui.common.Context()
			}
			built <- ui
			return p, ui, cancel
		})(func(s ssh.Session) {
			// The program has exited; the middleware's deferred cleanup
			// has not run yet.
			var e errs
			select {
			case ctx := <-uiCtx:
				e.ui = ctx.Err()
			default:
				e.ui = errors.New("no UI was built")
			}
			e.conn = s.Context().Err()
			after <- e
			_ = s.Exit(0)
		})),
	}
	if err := ssh.EmulatePty()(srv); err != nil {
		t.Fatal(err)
	}
	addr := testsession.Listen(t, srv)

	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
		Timeout:         sessionLoopsWait,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	var ui *UI
	select {
	case ui = <-built:
	case <-time.After(sessionLoopsWait):
		t.Fatal("newSessionProgram was not called")
	}
	if ui == nil {
		t.Fatal("the TUI did not start: newSessionProgram built no UI")
	}
	ctx := ui.common.Context()
	if err := ctx.Err(); err != nil {
		t.Fatalf("the TUI's context is done while it runs: %v", err)
	}

	// Quit the TUI, retrying until the program reads the key.
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(sessionLoopsWait)
	var got errs
wait:
	for {
		select {
		case got = <-after:
			break wait
		case <-tick.C:
			_, _ = io.WriteString(stdin, "q")
		case <-timeout:
			t.Fatal("TUI did not quit")
		}
	}
	if !errors.Is(got.ui, context.Canceled) {
		t.Errorf("UI context after the program exited, before the middleware's cleanup: %v, want %v", got.ui, context.Canceled)
	}
	if got.conn != nil {
		t.Errorf("connection context after the program exited: %v, want it open", got.conn)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("session: %v", err)
	}

	// The session has ended and the connection is still open: a new
	// session on it works.
	if _, err := client.NewSession(); err != nil {
		t.Fatalf("the connection did not stay open: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("the TUI's context after its session ended: %v, want %v", ctx.Err(), context.Canceled)
	}
}
