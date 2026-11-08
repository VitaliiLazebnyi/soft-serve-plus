package ssh

import (
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

// BubbleTeaMiddleware runs the soft-serve TUI for sessions with a pty.
// This middleware must be run after the ContextMiddleware.
//
// It releases the session's UI resources once the program has exited,
// however the session ended: quit, disconnect or idle timeout. They cannot
// be released any earlier, as the program renders one last frame after the
// model quits, and that render needs them.
func BubbleTeaMiddleware(next ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		var c *common.Common
		defer func() {
			if c != nil {
				c.Zone.Close()
			}
		}()
		bm.MiddlewareWithProgramHandler(func(s ssh.Session) *tea.Program {
			p, pc := newSessionProgram(s)
			c = pc
			return p
		})(next)(s)
	}
}

// newSessionProgram returns the TUI program for the session, and the Common
// its UI was built with, or nil for both when the session gets no TUI.
func newSessionProgram(s ssh.Session) (*tea.Program, *common.Common) {
	pty, _, active := s.Pty()
	if !active {
		return nil, nil
	}

	ctx := s.Context()
	be := backend.FromContext(ctx)
	cfg := config.FromContext(ctx)
	cmd := s.Command()
	initialRepo := ""
	if len(cmd) == 1 {
		initialRepo = cmd[0]
	}

	// Gate the TUI on the caller's baseline access only. Whether they may
	// open the requested repository is decided by the UI, which treats a
	// private and a missing repository the same way.
	if be.BaseAccessLevelByPublicKey(ctx, s.PublicKey()) < access.ReadOnlyAccess {
		wish.Fatalln(s, proto.ErrUnauthorized)
		return nil, nil
	}

	opts := bm.MakeOptions(s)
	opts = append(opts,
		tea.WithoutCatchPanics(),
		tea.WithContext(ctx),
		tea.WithColorProfile(common.DefaultColorProfile),
	)

	c := common.NewCommon(ctx, pty.Window.Width, pty.Window.Height)
	c.SetValue(common.ConfigKey, cfg)
	m := NewUI(c, initialRepo)
	p := tea.NewProgram(m, opts...)

	tuiSessionCounter.Inc()

	start := time.Now()
	go func() {
		<-ctx.Done()
		tuiSessionDuration.Add(time.Since(start).Seconds())
	}()

	return p, &c
}
