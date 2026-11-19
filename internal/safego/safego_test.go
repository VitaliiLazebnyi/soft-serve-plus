package safego

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
)

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunReturnsNilWithoutPanic(t *testing.T) {
	called := false
	if err := Run(log.New(io.Discard), func() { called = true }); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if !called {
		t.Fatal("fn was not called")
	}
}

func TestRunRecoversAndLogs(t *testing.T) {
	var buf syncBuffer
	logger := log.New(&buf)
	cause := errors.New("boom")
	deferred := false

	err := Run(logger, func() {
		defer func() { deferred = true }()
		panic(cause)
	})

	var perr *PanicError
	if !errors.As(err, &perr) {
		t.Fatalf("Run = %v, want a *PanicError", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("Run = %v, want it to wrap the panic value", err)
	}
	if !bytes.Contains(perr.Stack, []byte("TestRunRecoversAndLogs")) {
		t.Errorf("stack does not show where the panic happened:\n%s", perr.Stack)
	}
	if !deferred {
		t.Error("fn's deferred call did not run")
	}
	if out := buf.String(); !strings.Contains(out, "recovered panic") || !strings.Contains(out, "boom") {
		t.Errorf("panic not logged: %q", out)
	}
}

func TestRunNilLoggerUsesDefault(t *testing.T) {
	var buf syncBuffer
	prev := log.Default()
	log.SetDefault(log.New(&buf))
	t.Cleanup(func() { log.SetDefault(prev) })

	if err := Run(nil, func() { panic("nil logger") }); err == nil {
		t.Fatal("Run = nil, want the panic")
	}
	if !strings.Contains(buf.String(), "nil logger") {
		t.Errorf("panic not logged to the default logger: %q", buf.String())
	}
}

// A panic in a goroutine started by Go is logged and does not end the
// process; without the recover the test binary would die here.
func TestGoRecovers(t *testing.T) {
	var buf syncBuffer
	done := make(chan struct{})
	Go(log.New(&buf), func() {
		defer close(done)
		panic("in goroutine")
	})
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("goroutine did not run")
	}
	deadline := time.Now().Add(time.Minute)
	for !strings.Contains(buf.String(), "in goroutine") {
		if time.Now().After(deadline) {
			t.Fatalf("panic not logged: %q", buf.String())
		}
		time.Sleep(time.Millisecond)
	}
}
