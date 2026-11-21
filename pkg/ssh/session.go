package ssh

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	bm "charm.land/wish/v2/bubbletea"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The TUI counters have no labels: the repository and terminal come from the
// client, and anyone with read access may open the TUI, so labelling by them
// would let clients add series without limit.
var tuiSessionCounter = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "soft_serve",
	Subsystem: "ssh",
	Name:      "tui_session_total",
	Help:      "The total number of TUI sessions",
})

var tuiSessionDuration = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "soft_serve",
	Subsystem: "ssh",
	Name:      "tui_session_seconds_total",
	Help:      "The total time spent in TUI sessions",
})

// bubbleTeaMiddleware runs the soft-serve TUI for sessions with a pty, and
// counts the time each one spends in it.
//
// It releases the session's UI resources once the program has exited,
// however the session ended: quit, disconnect or idle timeout. They cannot
// be released any earlier, as the program renders one last frame after the
// model quits, and that render needs them.
//
// The time runs until the program exits, which ends the session. The
// connection may outlive it, for example when a client multiplexes several
// sessions over one connection, so its close is not the TUI's end. So the
// UI's work, such as the git commands of a load still running, is canceled
// when the program exits, not when the connection closes.
func (srv *Server) bubbleTeaMiddleware(next ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		var c *common.Common
		cancel := func() {}
		var start time.Time
		stop := func() {
			if !start.IsZero() {
				tuiSessionDuration.Add(time.Since(start).Seconds())
				start = time.Time{}
			}
		}
		defer func() {
			// A panic skips the handler below; count that session's time
			// too.
			stop()
			cancel()
			if c != nil {
				c.Zone.Close()
			}
		}()
		bm.MiddlewareWithProgramHandler(func(s ssh.Session) *tea.Program {
			p, pc, pcancel := srv.newSessionProgram(s)
			c, cancel = pc, pcancel
			if p != nil {
				start = time.Now()
			}
			return p
		})(func(s ssh.Session) {
			// The program has exited: stop the clock and the UI's work
			// before handing the session on.
			stop()
			cancel()
			next(s)
		})(s)
	}
}

// newSessionProgram returns the TUI program for the session, the Common
// its UI was built with, and the function that cancels the UI's context,
// which the caller calls once the program has exited. When the session
// gets no TUI, the program and the Common are nil.
func (srv *Server) newSessionProgram(s ssh.Session) (*tea.Program, *common.Common, context.CancelFunc) {
	none := func() {}
	pty, _, active := s.Pty()
	if !active {
		return nil, nil, none
	}

	ctx := s.Context()
	be := srv.be
	cmd := s.Command()
	initialRepo := ""
	if len(cmd) == 1 {
		initialRepo = cmd[0]
	}

	// Gate the TUI on the caller's baseline access only. Whether they may
	// open the requested repository is decided by the UI, which treats a
	// private and a missing repository the same way.
	//
	// AuthenticationMiddleware has already resolved the session's key to a
	// user (or to nobody) and stored it in the context, so it is not looked
	// up again here. Admin keys are still recognised from the context's key.
	level, err := be.BaseAccessLevelForUser(ctx, proto.UserFromContext(ctx))
	if err != nil {
		logAccessCheckFailure(ctx, srv.logger, err)
		wish.Fatalln(s, ErrAccessCheckFailed)
		return nil, nil, none
	}
	if level < access.ReadOnlyAccess {
		wish.Fatalln(s, proto.ErrUnauthorized)
		return nil, nil, none
	}

	// The UI's context is the session's, not the connection's: it ends
	// when the program exits, so a client that keeps the connection open
	// for its next session does not keep the UI's git commands running.
	// pkg/ui still takes the config and backend from it.
	uiCtx, cancel := context.WithCancel(ctx)
	uiCtx = config.WithContext(uiCtx, srv.cfg)
	uiCtx = backend.WithContext(uiCtx, be)
	if srv.onUIContext != nil {
		srv.onUIContext(uiCtx)
	}

	// The terminal size is the client's to choose, and Bubble Tea allocates
	// width x height cells for the initial size without checking it. Later
	// options win, so these replace the size and filter bm.MakeOptions sets.
	width, height := common.ClampSize(pty.Window.Width, pty.Window.Height)
	opts := bm.MakeOptions(s)
	opts = append(opts,
		tea.WithContext(uiCtx),
		tea.WithColorProfile(common.DefaultColorProfile),
		tea.WithWindowSize(width, height),
		tea.WithFilter(sessionFilter),
	)

	c := common.NewCommon(uiCtx, width, height)
	m := NewUI(c, initialRepo)
	p := tea.NewProgram(m, opts...)

	tuiSessionCounter.Inc()

	return p, &c, cancel
}

// sessionFilter is the TUI program's message filter. It limits window sizes
// from the client's resizes by common.ClampSize, and, like the filter
// bm.MakeOptions sets, which it replaces, resumes instead of suspending: a
// remote session cannot be suspended.
func sessionFilter(_ tea.Model, msg tea.Msg) tea.Msg {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		msg.Width, msg.Height = common.ClampSize(msg.Width, msg.Height)
		return msg
	case tea.SuspendMsg:
		return tea.ResumeMsg{}
	}
	return msg
}
