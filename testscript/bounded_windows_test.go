package testscript

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// interruptSignals are the signals that stop runBounded early.
var interruptSignals = []os.Signal{os.Interrupt}

// ownProcessGroup does nothing: killProcessTree finds the processes cmd
// starts by their parent.
func ownProcessGroup(*exec.Cmd) {}

// killProcessTree kills p and every process it started. It runs only when
// runBounded's own limit passes: at the test binary's deadline testscript
// kills runBounded with os.Kill, which cannot be caught, so git and its ssh
// are left running then, as they were before runBounded.
func killProcessTree(p *os.Process) {
	_ = runDiagnostic("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	_ = p.Kill()
}

// quitProcess fails: Windows has no signal that makes a Go program print its
// goroutines.
func quitProcess(*os.Process) error {
	return errors.New("not supported on Windows")
}

// describeProcesses writes the running processes to w; pid is the one
// runBounded started. Windows has no list of open files to add.
func describeProcesses(w io.Writer, pid int, _ bool) {
	_, _ = fmt.Fprintf(w, "harness: running processes (the command is pid %d):\n%s", pid, runDiagnostic("tasklist"))
}
