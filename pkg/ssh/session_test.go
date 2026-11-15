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
		s, close := setup(t)
		s.Stderr = os.Stderr
		t.Log("requesting pty")
		err := s.RequestPty("xterm", 80, 40, nil)
		is.NoErr(err)
		go func() {
			time.Sleep(1 * time.Second)
			// s.Signal(gossh.SIGTERM)
			s.Close() //nolint: errcheck
		}()
		t.Log("waiting for session to exit")
		_, err = s.Output("test")
		var ee *gossh.ExitMissingError
		is.True(errors.As(err, &ee))
		t.Log("session exited")
		is.NoErr(close())
	})
}

func setup(tb testing.TB) (*gossh.Session, func() error) {
	tb.Helper()
	srv, closeDB := setupServer(tb)
	return testsession.New(tb, srv, nil), closeDB
}

// setupServer returns an SSH server running the TUI middleware, and a func
// that closes its database.
func setupServer(tb testing.TB) (*ssh.Server, func() error) {
	tb.Helper()
	ctx := context.TODO()
	cfg := config.DefaultConfig()
	cfg.DataPath = tb.TempDir()
	if err := cfg.Validate(); err != nil {
		tb.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		tb.Fatal(err)
	}
	if err := migrate.Migrate(ctx, dbx); err != nil {
		tb.Fatal(err)
	}
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	return &ssh.Server{
		Handler: ContextMiddleware(cfg, dbx, dbstore, be, log.Default())(BubbleTeaMiddleware(func(s ssh.Session) {
			_, _, active := s.Pty()
			if !active {
				tb.Error("session has no pty")
				_ = s.Exit(1)
				return
			}
			_ = s.Exit(0)
		})),
	}, dbx.Close
}

// TestBubbleTeaMiddlewareClosesZoneManager checks that a TUI session that
// ends without the quit key, here by the client dropping the connection,
// still stops its zone manager's worker goroutine.
//
// Workers are tracked by goroutine ID, so a worker left by an earlier test
// that is still shutting down cannot be mistaken for this session's.
func TestBubbleTeaMiddlewareClosesZoneManager(t *testing.T) {
	before := zoneWorkers()
	srv, closeDB := setupServer(t)
	t.Cleanup(func() { _ = closeDB() })
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
	deadline := time.Now().Add(30 * time.Second)
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
	srv, closeDB := setupServer(t)
	t.Cleanup(func() { _ = closeDB() })
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
	timeout := time.After(10 * time.Second)
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
				if grown > limit {
					t.Fatalf("heap grew by %d MiB with the TUI open, want at most %d MiB", grown>>20, limit>>20)
				}
			}

			c, err := dialClient(t, addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close() //nolint: errcheck
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
			// it, and track when it last produced a frame.
			var mu sync.Mutex
			var total int
			var lastRead time.Time
			go func() {
				buf := make([]byte, 4096)
				for {
					n, rerr := stdout.Read(buf)
					if n > 0 {
						mu.Lock()
						total += n
						lastRead = time.Now()
						mu.Unlock()
					}
					if rerr != nil {
						return
					}
				}
			}()
			// Wait for the first frame and for the output to go quiet,
			// instead of a fixed wait.
			waitFor(t, "the TUI to draw and settle", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return total > 0 && time.Since(lastRead) > 300*time.Millisecond
			})
			check()
			if tc.resize == nil {
				return
			}

			if ok, err := s.SendRequest("window-change", true, tc.resize); err != nil || !ok {
				t.Fatalf("window-change: %t, %v", ok, err)
			}
			// The reply comes from the request loop, before the TUI has
			// handled the resize, so quiet output proves nothing here: watch
			// the heap for a while and fail as soon as it is over the limit.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				check()
				time.Sleep(50 * time.Millisecond)
			}
			check()
		})
	}
}

// newSessionProgram gives the UI the clamped size.
func TestNewSessionProgramClampsSize(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := serverContext(t, cfg, log.Default())
	srv := &ssh.Server{
		Handler: ContextMiddleware(cfg, db.FromContext(ctx), store.FromContext(ctx), backend.FromContext(ctx), log.Default())(func(s ssh.Session) {
			p, c := newSessionProgram(s)
			if p == nil {
				_ = s.Exit(1)
				return
			}
			c.Zone.Close()
			_, _ = fmt.Fprintf(s.Stderr(), "%dx%d", c.Width, c.Height)
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
