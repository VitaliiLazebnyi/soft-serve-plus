package testnet

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

// fakeDial replaces dialContext with one that fails with each of errs in
// turn and then connects, and returns how many attempts were made.
func fakeDial(t *testing.T, errs ...error) *int {
	t.Helper()
	attempts := new(int)
	orig := dialContext
	t.Cleanup(func() { dialContext = orig })
	dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		*attempts++
		if *attempts <= len(errs) {
			return nil, errs[*attempts-1]
		}
		c1, c2 := net.Pipe()
		t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
		return c1, nil
	}
	return attempts
}

func dialErr(err error) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", err)}
}

// A connect that timed out in the kernel never reached the server, so it is
// tried again; up to maxDialAttempts in all.
func TestDialRetriesConnectTimeout(t *testing.T) {
	timedOut := dialErr(syscall.ETIMEDOUT)
	attempts := fakeDial(t, timedOut, timedOut)
	c, err := Dial(context.Background(), "tcp", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial = %v; want a connection after two timeouts", err)
	}
	_ = c.Close()
	if *attempts != 3 {
		t.Fatalf("%d attempts; want 3", *attempts)
	}
}

func TestDialGivesUpAfterMaxAttempts(t *testing.T) {
	timedOut := dialErr(syscall.ETIMEDOUT)
	attempts := fakeDial(t, timedOut, timedOut, timedOut, timedOut, timedOut)
	_, err := Dial(context.Background(), "tcp", "127.0.0.1:1")
	if !errors.Is(err, syscall.ETIMEDOUT) {
		t.Fatalf("Dial = %v; want ETIMEDOUT", err)
	}
	if *attempts != maxDialAttempts {
		t.Fatalf("%d attempts; want %d", *attempts, maxDialAttempts)
	}
}

// Every other failure, and the caller's own deadline, is returned at once,
// so a server that is down or slow still fails its test.
func TestDialDoesNotRetryOtherErrors(t *testing.T) {
	for _, err := range []error{
		dialErr(syscall.ECONNREFUSED),
		dialErr(syscall.ECONNRESET),
		&net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded},
		&net.OpError{Op: "read", Net: "tcp", Err: syscall.ETIMEDOUT},
	} {
		attempts := fakeDial(t, err, err)
		if _, got := Dial(context.Background(), "tcp", "127.0.0.1:1"); got == nil {
			t.Fatalf("Dial after %v connected; want the error", err)
		}
		if *attempts != 1 {
			t.Fatalf("%v: %d attempts; want 1", err, *attempts)
		}
	}
}

func TestDialStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut := dialErr(syscall.ETIMEDOUT)
	attempts := fakeDial(t, timedOut, timedOut)
	if _, err := Dial(ctx, "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("Dial connected after the context ended")
	}
	if *attempts != 1 {
		t.Fatalf("%d attempts; want 1", *attempts)
	}
}

// A real connection still works through Dial and DialSSH's dial.
func TestDialConnects(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close() //nolint:errcheck
	c, err := Dial(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}
