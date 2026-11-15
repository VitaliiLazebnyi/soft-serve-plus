//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/x/term"
)

// forwardedSignals are passed on from the supervising process to the server.
var forwardedSignals = []os.Signal{
	syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP,
	syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2,
}

// superviseIfInit makes soft its own init when it runs as PID 1, as it does in
// a container started without an init process. git leaves orphaned processes
// behind (since git 2.47, one "git maintenance run --auto --detach" per push
// that updates a ref, and a helper per transfer cut off mid-way). The kernel
// re-parents each orphan to PID 1, which must reap it, or it stays a zombie
// that counts against the container's PID limit.
//
// The server cannot reap them itself: waiting for any child would also
// collect the exit status of the git processes os/exec is waiting for. So when
// soft is PID 1 it starts itself again as a child, forwards signals to it,
// reaps every process that exits, and exits with the child's status. It
// returns only when soft is not PID 1, or when the child cannot be started; in
// that case it warns and the server runs unsupervised.
func superviseIfInit() {
	if os.Getpid() != 1 {
		return
	}
	exe, err := os.Executable()
	if err == nil {
		// The server must outlive any context: it stops when it is sent
		// a signal, never because this process gave up on it.
		child := exec.CommandContext(context.Background(), exe, os.Args[1:]...)
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		sigs := make(chan os.Signal, 16)
		signal.Notify(sigs, forwardedSignals...)
		// The configuration is not read yet; match the server's default
		// timestamp format at least.
		log.SetTimeFormat(time.DateTime)
		log.Info("soft is PID 1: running the server as a child process and reaping exited processes; " +
			"an init such as tini (docker run --init, init: true in compose) uses less memory")
		var code int
		code, err = supervise(child, sigs)
		if err == nil {
			os.Exit(code)
		}
		signal.Reset(forwardedSignals...)
	}
	log.Warn("soft is PID 1 and cannot supervise itself: exited git processes will not be reaped "+
		"and will count against the container's PID limit; run soft under an init "+
		"(docker run --init, init: true in compose, or tini)", "err", err)
}

// supervise starts child, sends it every signal received on sigs, and reaps
// every process that exits until child does. It returns child's exit code, or
// 128 plus the signal number if a signal ended it. Callers must not wait for
// processes themselves while it runs: it reaps any child of this process.
//
// The child runs in a process group of its own, so a signal sent to this
// process's group (a terminal's ^C) reaches it once, through sigs, and not a
// second time directly. When the child's stdin is a terminal, the child's
// group becomes the terminal's foreground group: the terminal then signals
// only the child, and the child may read from it. That fails when the
// terminal is not this process's controlling terminal; the child is then
// started again without the terminal.
func supervise(child *exec.Cmd, sigs <-chan os.Signal) (int, error) {
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var withoutTerminal *exec.Cmd
	if f, ok := child.Stdin.(*os.File); ok && term.IsTerminal(f.Fd()) {
		// A Cmd cannot be started twice, so the retry needs a copy. It
		// copies only the fields superviseIfInit sets; keep it in step
		// if that ever sets another.
		withoutTerminal = &exec.Cmd{
			Path: child.Path, Args: child.Args, Env: child.Env, Dir: child.Dir,
			Stdin: child.Stdin, Stdout: child.Stdout, Stderr: child.Stderr,
			ExtraFiles:  child.ExtraFiles,
			SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
		}
		child.SysProcAttr.Foreground = true
		child.SysProcAttr.Ctty = 0 // the child's stdin
	}
	err := child.Start()
	if err != nil && withoutTerminal != nil {
		// Retry after any error, not only ENOTTY: a child in a group
		// of its own beats an unsupervised server, and if the cause
		// was something else, the retry fails too.
		child = withoutTerminal
		if retryErr := child.Start(); retryErr != nil {
			err = fmt.Errorf("%w; without the terminal: %w", err, retryErr)
		} else {
			err = nil
		}
	}
	if err != nil {
		return 0, err
	}
	go func() {
		// Process.Signal uses a pidfd where the kernel has them, so a
		// signal that arrives after the child was reaped below is
		// dropped instead of reaching a process that reused its PID.
		for s := range sigs {
			_ = child.Process.Signal(s)
		}
	}()
	pid := child.Process.Pid
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(-1, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			// ECHILD: nothing left to wait for, so the child is gone
			// without its status having been seen here.
			return 1, nil
		}
		if wpid != pid {
			continue // an adopted orphan; reaping it was the point
		}
		if ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ws.ExitStatus(), nil
	}
}
