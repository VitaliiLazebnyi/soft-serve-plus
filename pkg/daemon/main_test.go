package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/internal/testnet"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	_ "modernc.org/sqlite" // sqlite driver
)

var (
	testDaemon  *Server
	testBackend *backend.Backend
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests starts the daemon the tests talk to, runs the tests and tears the
// daemon down again. It is separate from TestMain so that its deferred
// cleanup runs before os.Exit.
func runTests(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "soft-serve-test")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck
	ctx := context.TODO()
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	// Each request in these tests closes its connection, but the daemon notices
	// asynchronously; leave headroom so back-to-back requests are not refused
	// with "too many connections" on slow (-race) runners.
	cfg.Git.MaxConnections = 10
	cfg.Git.MaxTimeout = 100
	// Long enough that no client, however slowed by a loaded machine, is
	// timed out between its steps. A test that waits for the idle timeout
	// serves its own daemon (startIdleDaemon).
	cfg.Git.IdleTimeout = 600
	cfg.Git.ListenAddr = fmt.Sprintf("127.0.0.1:%d", testnet.RandomPort()) // RandomPort checks only loopback
	e, remove, err := backendtest.NewUnmanaged(tmp, backendtest.UseConfig(cfg))
	if err != nil {
		log.Fatal(err)
	}
	defer remove() //nolint:errcheck
	be := e.Backend
	testBackend = be
	d, err := NewServer(cfg, be, nil)
	if err != nil {
		log.Fatal(err)
	}
	patient(d)
	testDaemon = d
	defer d.Close() //nolint:errcheck
	// Bind before running the tests so the first dial cannot race the
	// listener and be refused.
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", cfg.Git.ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
	go d.Serve(l) //nolint:errcheck
	return m.Run()
}
